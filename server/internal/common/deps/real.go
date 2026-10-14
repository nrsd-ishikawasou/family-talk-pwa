// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package deps

// 本物の実装（srv-common B-010）。cmd/ft が配線する。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/libdns/libdns"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/twitchtv/twirp"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// 本物が interface を満たすことをコンパイルの時に確かめる。
var (
	_ PushTransport = (*HttpPush)(nil)
	_ Fetcher       = (*HttpFetcher)(nil)
	_ DnsProvider   = (*DnsApi)(nil)
	_ Restic        = (*ResticCli)(nil)
	_ Docker        = (*DockerApi)(nil)
	_ Luks          = (*CryptsetupCli)(nil)
	_ Systemd       = (*SystemdFiles)(nil)
	_ ReleaseSource = (*GhRelease)(nil)
	_ SfuAdmin      = (*SfuApi)(nil)
	_ MapSource     = (*PmtilesCli)(nil)
)

// 外とのやり取りの上限（第1章 4.2）。
const (
	pushConnect     = 10 * time.Second
	pushTotal       = 30 * time.Second
	pushBodyMax     = 4096
	releaseTotal    = 30 * time.Minute
	releaseSizeMax  = 500 << 20
	versionJSONMax  = 1 << 20
	dnsConnect      = 10 * time.Second
	ddnsTTL         = 300 * time.Second
	stderrTailBytes = 512
)

// noRedirect は転送を追わない（必要な所は自分で 1 段ずつ追う）。
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// newTransport は環境変数の proxy を使わない transport（外へ出る経路を第1章 4.2 の表の相手だけにする）。
func newTransport(connect time.Duration) *http.Transport {
	d := &net.Dialer{Timeout: connect}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         d.DialContext,
		TLSHandshakeTimeout: connect,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// ---------------------------------------------------------------------------
// HttpPush

// HttpPush は net/http でプッシュ配信サービスへ POST する（暗号化と VAPID の署名は srv-push）。
type HttpPush struct {
	client *http.Client
	clk    clock.Clock
}

// NewHttpPush は HttpPush を作る。clk は Retry-After の HTTP の日付を秒に直すのに使う。
func NewHttpPush(clk clock.Clock) *HttpPush {
	return &HttpPush{client: &http.Client{Transport: newTransport(pushConnect), CheckRedirect: noRedirect}, clk: clk}
}

// Post は PushTransport.Post の本物。本文が 4,096 バイトを超えれば送らない（第1章 4.2）。
func (d *HttpPush) Post(ctx context.Context, endpoint string, body []byte, header http.Header) (PushResponse, error) {
	if len(body) > pushBodyMax {
		return PushResponse{}, errors.New("deps: 通知の本文が 4096 バイトを超える")
	}
	ctx, cancel := context.WithTimeout(ctx, pushTotal)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return PushResponse{}, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return PushResponse{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, pushBodyMax))
	return PushResponse{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), d.clk.Now())}, nil
}

// retryAfter は Retry-After（秒の数か HTTP の日付。RFC 9110 10.2.3）を時間に直す。読めなければ 0。
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// ---------------------------------------------------------------------------
// HttpFetcher

// Resolver は名前の解決（本物は net.DefaultResolver。試験は表）。
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// HttpFetcher は外の URL を取る。名前の解決の結果と転送の各段で内部のアドレス帯を拒み、
// 確かめたアドレスに直接つなぐ（解決とつなぐ間の差し替えを防ぐ。第1章 10.6）。
type HttpFetcher struct {
	resolver  Resolver
	self      []netip.Addr
	userAgent string
	// roots は TLS の信頼の起点（nil なら OS の既定。試験だけが手元の証明書を差す）。
	roots *x509.CertPool
}

// NewHttpFetcher は HttpFetcher を作る。self は家族のサーバー自身のアドレス、userAgent は本システムの名と版。
func NewHttpFetcher(resolver Resolver, self []netip.Addr, userAgent string) *HttpFetcher {
	return &HttpFetcher{resolver: resolver, self: append([]netip.Addr(nil), self...), userAgent: userAgent}
}

// rejected はプレビューの取得の失敗（MEDIA-010。理由は応答に出さない）。
func rejected() error {
	return fault.New(gen.MEDIA010, nil)
}

// Get は Fetcher.Get の本物。https だけ。転送は limits.MaxRedirects 回まで 1 段ずつ確かめて追う。
func (d *HttpFetcher) Get(ctx context.Context, rawURL string, limits FetchLimits) (FetchResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, limits.Total)
	defer cancel()
	cur := rawURL
	for hop := 0; ; hop++ {
		u, err := url.Parse(cur)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
			return FetchResponse{}, rejected()
		}
		addr, err := d.pick(ctx, u.Hostname())
		if err != nil {
			return FetchResponse{}, err
		}
		resp, err := d.once(ctx, u, addr, limits)
		if err != nil {
			return FetchResponse{}, err
		}
		if resp.Status >= 300 && resp.Status < 400 {
			loc := resp.Header.Get("Location")
			if loc == "" || hop >= limits.MaxRedirects {
				return FetchResponse{}, rejected()
			}
			next, err := u.Parse(loc)
			if err != nil {
				return FetchResponse{}, rejected()
			}
			cur = next.String()
			continue
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return FetchResponse{}, rejected()
		}
		resp.FinalUrl = u.String()
		return resp, nil
	}
}

// pick は host を解決し、全部のアドレスが内部でないことを確かめて、つなぐアドレスを 1 つ返す。
func (d *HttpFetcher) pick(ctx context.Context, host string) (netip.Addr, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		addrs, err = d.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addrs) == 0 {
			return netip.Addr{}, rejected()
		}
	}
	for _, a := range addrs {
		if isInternal(a, d.self) {
			return netip.Addr{}, rejected()
		}
	}
	return addrs[0].Unmap(), nil
}

// once は addr に直接つないで 1 回の GET を行う。SNI と Host は元の名前。本文は MaxBytes まで。
func (d *HttpFetcher) once(ctx context.Context, u *url.URL, addr netip.Addr, limits FetchLimits) (FetchResponse, error) {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialer := &net.Dialer{Timeout: limits.Connect}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		},
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname(), RootCAs: d.roots},
		TLSHandshakeTimeout: limits.Connect,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: noRedirect}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchResponse{}, rejected()
	}
	req.Header.Set("User-Agent", d.userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return FetchResponse{}, rejected()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limits.MaxBytes+1))
	if err != nil || int64(len(body)) > limits.MaxBytes {
		return FetchResponse{}, rejected()
	}
	return FetchResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: body}, nil
}

// ---------------------------------------------------------------------------
// DnsApi

// DnsProviderFactory は DNS の提供者の名から libdns の RecordSetter を作る関数（token は API トークン）。
type DnsProviderFactory func(token []byte) (libdns.RecordSetter, error)

var (
	dnsMu        sync.RWMutex
	dnsProviders = map[string]DnsProviderFactory{}
)

// RegisterDnsProvider は提供者を表に足す（FT_DNS_PROVIDER の名。caddy の DNS のモジュールと同じ提供者。deploy が足す）。
func RegisterDnsProvider(name string, f DnsProviderFactory) {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	dnsProviders[name] = f
}

// DnsApi は libdns の RecordSetter で A と AAAA を書く（第14章 5.2。提供者は FT_DNS_PROVIDER の名で選ぶ）。
type DnsApi struct {
	setter libdns.RecordSetter
	zone   string
}

// NewDnsApi は provider の名の提供者で DnsApi を作る。zone は「example.org.」の形（末尾の点つき）。
// 表に無い名は起動の誤り（第1章 7.1 の 1）。
func NewDnsApi(provider string, token []byte, zone string) (*DnsApi, error) {
	dnsMu.RLock()
	f, ok := dnsProviders[provider]
	dnsMu.RUnlock()
	if !ok {
		return nil, errors.New("deps: 知らない DNS の提供者: " + provider)
	}
	s, err := f(token)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(zone, ".") {
		zone += "."
	}
	return &DnsApi{setter: s, zone: zone}, nil
}

// relative は名前を zone の相対の名にする（zone そのものは @）。
func (d *DnsApi) relative(name string) (string, error) {
	fq := strings.TrimSuffix(name, ".") + "."
	if fq == d.zone {
		return "@", nil
	}
	if !strings.HasSuffix(fq, "."+d.zone) {
		return "", errors.New("deps: 名前が zone の中に無い")
	}
	return strings.TrimSuffix(fq, "."+d.zone), nil
}

// set は 1 つのアドレスのレコードを書く。
func (d *DnsApi) set(ctx context.Context, name string, ip netip.Addr) error {
	rel, err := d.relative(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, dnsConnect+pushTotal)
	defer cancel()
	_, err = d.setter.SetRecords(ctx, d.zone, []libdns.Record{libdns.Address{Name: rel, TTL: ddnsTTL, IP: ip}})
	return err
}

// SetA は DnsProvider.SetA の本物。
func (d *DnsApi) SetA(ctx context.Context, name string, ip netip.Addr) error {
	if !ip.Unmap().Is4() {
		return errors.New("deps: A に IPv4 でないアドレス")
	}
	return d.set(ctx, name, ip.Unmap())
}

// SetAAAA は DnsProvider.SetAAAA の本物。
func (d *DnsApi) SetAAAA(ctx context.Context, name string, ip netip.Addr) error {
	if !ip.Is6() || ip.Is4In6() {
		return errors.New("deps: AAAA に IPv6 でないアドレス")
	}
	return d.set(ctx, name, ip)
}

// ---------------------------------------------------------------------------
// 子のプロセス

// runResult は子のプロセスの結果。
type runResult struct {
	stdout []byte
	stderr []byte
	code   int
}

// run は bin を args で動かす。secrets は 3 番からのファイル記述子にパイプで渡す（引数と環境変数に置かない）。
// env は子の環境に足す値。標準出力は全部、標準エラーは最後の部分だけを返す。
func run(ctx context.Context, bin string, args []string, secrets [][]byte, env []string, stdin io.Reader) (runResult, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	var writers []*os.File
	for range secrets {
		r, w, err := os.Pipe()
		if err != nil {
			return runResult{}, err
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, r)
		writers = append(writers, w)
	}
	if err := cmd.Start(); err != nil {
		for i, w := range writers {
			_ = w.Close()
			_ = cmd.ExtraFiles[i].Close()
		}
		return runResult{}, err
	}
	for i, w := range writers {
		_ = cmd.ExtraFiles[i].Close()
		go func(w *os.File, b []byte) {
			_, _ = w.Write(b)
			_ = w.Close()
		}(w, secrets[i])
	}
	err := cmd.Wait()
	res := runResult{stdout: stdout.Bytes(), stderr: tail(stderr.Bytes()), code: cmd.ProcessState.ExitCode()}
	if err != nil {
		return res, fmt.Errorf("deps: %s が終了コード %d で止まった: %s", filepath.Base(bin), res.code, strings.TrimSpace(string(res.stderr)))
	}
	return res, nil
}

// tail は標準エラーの最後の部分（秘密は標準エラーに出さない道具だけを呼ぶ）。
func tail(b []byte) []byte {
	if len(b) > stderrTailBytes {
		return b[len(b)-stderrTailBytes:]
	}
	return b
}

// ---------------------------------------------------------------------------
// ResticCli

// ResticCli はイメージに同梱した restic を呼ぶ（第14章 5.4）。パスワードはパイプで渡し、引数と環境変数に置かない。
type ResticCli struct {
	bin string
}

// NewResticCli は ResticCli を作る。
func NewResticCli(bin string) *ResticCli {
	return &ResticCli{bin: bin}
}

// args は全命令に共通の引数。パスワードは 3 番のファイル記述子。
func (d *ResticCli) args(repo ResticRepo, cmd ...string) []string {
	return append([]string{"--json", "--repo", repo.Url, "--password-file", "/dev/fd/3"}, cmd...)
}

// env は置き場の接続の値（S3 互換の鍵）を子の環境に置く。
func repoEnv(repo ResticRepo) []string {
	keys := make([]string, 0, len(repo.Env))
	for k := range repo.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+repo.Env[k])
	}
	return out
}

// Backup は Restic.Backup の本物。Paths か StdinCmd のどちらか一方（restic は 1 回の実行で両方を受けない）。
func (d *ResticCli) Backup(ctx context.Context, repo ResticRepo, input BackupInput) (BackupResult, error) {
	if (len(input.Paths) > 0) == (len(input.StdinCmd) > 0) {
		return BackupResult{}, errors.New("deps: restic の backup は Paths か StdinCmd のどちらか一方")
	}
	a := []string{"backup"}
	for _, t := range input.Tags {
		a = append(a, "--tag", t)
	}
	if len(input.StdinCmd) > 0 {
		a = append(a, "--stdin-from-command", "--stdin-filename", input.StdinFilename, "--")
		a = append(a, input.StdinCmd...)
	} else {
		a = append(a, "--")
		a = append(a, input.Paths...)
	}
	res, err := run(ctx, d.bin, d.args(repo, a...), [][]byte{repo.Password}, repoEnv(repo), nil)
	if err != nil {
		return BackupResult{}, err
	}
	var out BackupResult
	for _, line := range bytes.Split(res.stdout, []byte("\n")) {
		var m struct {
			MessageType string `json:"message_type"`
			SnapshotId  string `json:"snapshot_id"`
			DataAdded   int64  `json:"data_added"`
		}
		if json.Unmarshal(line, &m) == nil && m.MessageType == "summary" {
			out = BackupResult{SnapshotId: m.SnapshotId, AddedBytes: m.DataAdded}
		}
	}
	if out.SnapshotId == "" {
		return BackupResult{}, errors.New("deps: restic の backup の要約が無い")
	}
	return out, nil
}

// Forget は Restic.Forget の本物。
func (d *ResticCli) Forget(ctx context.Context, repo ResticRepo, policy ForgetPolicy) error {
	a := []string{"forget", "--keep-daily", strconv.Itoa(policy.Daily), "--keep-weekly", strconv.Itoa(policy.Weekly),
		"--keep-monthly", strconv.Itoa(policy.Monthly)}
	if policy.Prune {
		a = append(a, "--prune")
	}
	_, err := run(ctx, d.bin, d.args(repo, a...), [][]byte{repo.Password}, repoEnv(repo), nil)
	return err
}

// Check は Restic.Check の本物（subset は「10%」のような read-data-subset の値。空なら構造だけ）。
func (d *ResticCli) Check(ctx context.Context, repo ResticRepo, subset string) error {
	a := []string{"check"}
	if subset != "" {
		a = append(a, "--read-data-subset="+subset)
	}
	_, err := run(ctx, d.bin, d.args(repo, a...), [][]byte{repo.Password}, repoEnv(repo), nil)
	return err
}

// Restore は Restic.Restore の本物。
func (d *ResticCli) Restore(ctx context.Context, repo ResticRepo, snapshot, dest string) error {
	_, err := run(ctx, d.bin, d.args(repo, "restore", snapshot, "--target", dest), [][]byte{repo.Password}, repoEnv(repo), nil)
	return err
}

// keyIds は鍵の id の一覧。
func (d *ResticCli) keyIds(ctx context.Context, repo ResticRepo) (map[string]bool, error) {
	res, err := run(ctx, d.bin, d.args(repo, "key", "list"), [][]byte{repo.Password}, repoEnv(repo), nil)
	if err != nil {
		return nil, err
	}
	var keys []struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(res.stdout, &keys); err != nil {
		return nil, errors.New("deps: restic の key list の形が違う")
	}
	out := map[string]bool{}
	for _, k := range keys {
		out[k.Id] = true
	}
	return out, nil
}

// KeyAdd は Restic.KeyAdd の本物。新しいパスワードは 4 番のファイル記述子。足した鍵の id を返す。
func (d *ResticCli) KeyAdd(ctx context.Context, repo ResticRepo, newPassword []byte) (string, error) {
	before, err := d.keyIds(ctx, repo)
	if err != nil {
		return "", err
	}
	if _, err := run(ctx, d.bin, d.args(repo, "key", "add", "--new-password-file", "/dev/fd/4"),
		[][]byte{repo.Password, newPassword}, repoEnv(repo), nil); err != nil {
		return "", err
	}
	after, err := d.keyIds(ctx, repo)
	if err != nil {
		return "", err
	}
	for id := range after {
		if !before[id] {
			return id, nil
		}
	}
	return "", errors.New("deps: restic の足した鍵が見つからない")
}

// KeyRemove は Restic.KeyRemove の本物。
func (d *ResticCli) KeyRemove(ctx context.Context, repo ResticRepo, id string) error {
	_, err := run(ctx, d.bin, d.args(repo, "key", "remove", id), [][]byte{repo.Password}, repoEnv(repo), nil)
	return err
}

// Snapshots は Restic.Snapshots の本物。
func (d *ResticCli) Snapshots(ctx context.Context, repo ResticRepo) ([]Snapshot, error) {
	res, err := run(ctx, d.bin, d.args(repo, "snapshots"), [][]byte{repo.Password}, repoEnv(repo), nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Id    string    `json:"id"`
		Time  time.Time `json:"time"`
		Paths []string  `json:"paths"`
		Tags  []string  `json:"tags"`
	}
	if err := json.Unmarshal(res.stdout, &raw); err != nil {
		return nil, errors.New("deps: restic の snapshots の形が違う")
	}
	out := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		out = append(out, Snapshot{Id: s.Id, Time: s.Time.UTC(), Paths: s.Paths, Tags: s.Tags})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// DockerApi

// DockerApi は docker と docker compose を呼ぶ（ホストの ft だけ）。dir は Exec に使う compose の置き場（/etc/family-talk）。
type DockerApi struct {
	bin string
	dir string
}

// NewDockerApi は DockerApi を作る。
func NewDockerApi(bin, dir string) *DockerApi {
	return &DockerApi{bin: bin, dir: dir}
}

// compose は compose の命令の引数。
func (d *DockerApi) compose(dir string, cmd ...string) []string {
	return append([]string{"compose", "--project-directory", dir, "--file", filepath.Join(dir, "compose.yaml"),
		"--env-file", filepath.Join(dir, ".env")}, cmd...)
}

// ComposeUp は Docker.ComposeUp の本物。
func (d *DockerApi) ComposeUp(ctx context.Context, dir string) error {
	_, err := run(ctx, d.bin, d.compose(dir, "up", "-d", "--wait"), nil, nil, nil)
	return err
}

// ComposeStop は Docker.ComposeStop の本物。
func (d *DockerApi) ComposeStop(ctx context.Context, dir, service string) error {
	_, err := run(ctx, d.bin, d.compose(dir, "stop", service), nil, nil, nil)
	return err
}

// ComposeRecreate は Docker.ComposeRecreate の本物（秘密のファイルや設定を変えた後に読み直させる）。
func (d *DockerApi) ComposeRecreate(ctx context.Context, dir string, services []string) error {
	_, err := run(ctx, d.bin, d.compose(dir, append([]string{"up", "-d", "--force-recreate", "--wait"}, services...)...), nil, nil, nil)
	return err
}

// ComposeDown は Docker.ComposeDown の本物。
func (d *DockerApi) ComposeDown(ctx context.Context, dir string) error {
	_, err := run(ctx, d.bin, d.compose(dir, "down"), nil, nil, nil)
	return err
}

// Pull は Docker.Pull の本物。取った後の digest を返す。
func (d *DockerApi) Pull(ctx context.Context, ref string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, releaseTotal)
	defer cancel()
	if _, err := run(ctx, d.bin, []string{"pull", "--quiet", ref}, nil, nil, nil); err != nil {
		return "", err
	}
	res, err := run(ctx, d.bin, []string{"image", "inspect", "--format", "{{json .RepoDigests}}", ref}, nil, nil, nil)
	if err != nil {
		return "", err
	}
	var digests []string
	if err := json.Unmarshal(bytes.TrimSpace(res.stdout), &digests); err != nil || len(digests) == 0 {
		return "", errors.New("deps: イメージの digest が読めない")
	}
	_, digest, ok := strings.Cut(digests[0], "@")
	if !ok {
		return "", errors.New("deps: イメージの digest の形が違う")
	}
	return digest, nil
}

// Inspect は Docker.Inspect の本物。
func (d *DockerApi) Inspect(ctx context.Context, ref string) (ImageInfo, error) {
	res, err := run(ctx, d.bin, []string{"image", "inspect", "--format", "{{json .}}", ref}, nil, nil, nil)
	if err != nil {
		return ImageInfo{}, err
	}
	var m struct {
		RepoDigests []string  `json:"RepoDigests"`
		Created     time.Time `json:"Created"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(res.stdout), &m); err != nil || len(m.RepoDigests) == 0 {
		return ImageInfo{}, errors.New("deps: イメージの情報が読めない")
	}
	_, digest, _ := strings.Cut(m.RepoDigests[0], "@")
	return ImageInfo{Digest: digest, Created: m.Created.UTC()}, nil
}

// Exec は Docker.Exec の本物（compose の service の中で cmd を動かす）。終了コードが 0 でなくても誤りにせず結果で返す。
func (d *DockerApi) Exec(ctx context.Context, service string, cmd []string) (ExecResult, error) {
	res, err := run(ctx, d.bin, d.compose(d.dir, append([]string{"exec", "-T", service}, cmd...)...), nil, nil, nil)
	out := ExecResult{ExitCode: res.code, Stdout: res.stdout, Stderr: res.stderr}
	if err != nil && res.code <= 0 {
		return out, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// CryptsetupCli

// CryptsetupCli は cryptsetup と systemd-cryptenroll を呼ぶ（ホストの ft だけ。第12章 6.1）。
type CryptsetupCli struct {
	cryptsetup string
	enroll     string
	rand       io.Reader
}

// NewCryptsetupCli は CryptsetupCli を作る。rand は一時の鍵を作る乱数（Deps.Rand）。
func NewCryptsetupCli(cryptsetup, enroll string, rand io.Reader) *CryptsetupCli {
	return &CryptsetupCli{cryptsetup: cryptsetup, enroll: enroll, rand: rand}
}

// Format は Luks.Format の本物。一時の乱数の鍵で LUKS2 を作り、回復鍵を登録して一時の鍵の slot を消す。
// 回復鍵（紙に書く）を返す。
func (d *CryptsetupCli) Format(ctx context.Context, dev string) ([]byte, error) {
	tmp := make([]byte, 64)
	if _, err := io.ReadFull(d.rand, tmp); err != nil {
		return nil, err
	}
	defer clear(tmp)
	if _, err := run(ctx, d.cryptsetup, []string{"luksFormat", "--type", "luks2", "--batch-mode", "--key-file=/dev/fd/3", dev},
		[][]byte{tmp}, nil, nil); err != nil {
		return nil, err
	}
	res, err := run(ctx, d.enroll, []string{dev, "--unlock-key-file=/dev/fd/3", "--recovery-key", "--wipe-slot=password"},
		[][]byte{tmp}, nil, nil)
	if err != nil {
		return nil, err
	}
	key := recoveryKeyPattern.Find(res.stdout)
	if key == nil {
		return nil, errors.New("deps: 回復鍵が標準出力に無い")
	}
	return append([]byte(nil), key...), nil
}

// recoveryKeyPattern は systemd の回復鍵の形（8 文字 × 8、- 区切り。modhex の文字）。
var recoveryKeyPattern = regexp.MustCompile(`[cbdefghijklnrtuv]{8}(-[cbdefghijklnrtuv]{8}){7}`)

// EnrollTpm2 は Luks.EnrollTpm2 の本物（PCR 7。第11章 4 の 2）。
func (d *CryptsetupCli) EnrollTpm2(ctx context.Context, dev string) error {
	_, err := run(ctx, d.enroll, []string{dev, "--tpm2-device=auto", "--tpm2-pcrs=7"}, nil, nil, nil)
	return err
}

// Erase は Luks.Erase の本物。全 keyslot を消した後に、LUKS2 の頭（16 MiB）を 0 で上書きする（第11章 13.2）。
func (d *CryptsetupCli) Erase(ctx context.Context, dev string) error {
	if _, err := run(ctx, d.cryptsetup, []string{"erase", "--batch-mode", dev}, nil, nil, nil); err != nil {
		return err
	}
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	zero := make([]byte, 1<<20)
	for i := 0; i < 16; i++ {
		if _, err := f.Write(zero); err != nil {
			return err
		}
	}
	return f.Sync()
}

// ---------------------------------------------------------------------------
// SystemdFiles

// SystemdFiles は /etc/systemd/system の下に drop-in を書き、systemctl で有効にする（ホストの ft だけ）。
type SystemdFiles struct {
	dir       string
	systemctl string
}

// NewSystemdFiles は SystemdFiles を作る（dir は /etc/systemd/system）。
func NewSystemdFiles(dir, systemctl string) *SystemdFiles {
	return &SystemdFiles{dir: dir, systemctl: systemctl}
}

// unitName は unit と drop-in の名に使える文字だけを許す（道のりの外へ書かない）。
var unitName = regexp.MustCompile(`^[A-Za-z0-9@._-]+$`)

// WriteDropIn は Systemd.WriteDropIn の本物。/etc/systemd/system/<unit>.d/<name>.conf に、一時の名前で書いて改名する。
func (d *SystemdFiles) WriteDropIn(ctx context.Context, unit, name, text string) error {
	if !unitName.MatchString(unit) || !unitName.MatchString(name) {
		return errors.New("deps: unit か drop-in の名に使えない文字")
	}
	dir := filepath.Join(d.dir, unit+".d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name+".conf")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_, err = run(ctx, d.systemctl, []string{"daemon-reload"}, nil, nil, nil)
	return err
}

// Enable は Systemd.Enable の本物。
func (d *SystemdFiles) Enable(ctx context.Context, unit string) error {
	if !unitName.MatchString(unit) {
		return errors.New("deps: unit の名に使えない文字")
	}
	_, err := run(ctx, d.systemctl, []string{"enable", "--now", unit}, nil, nil, nil)
	return err
}

// ---------------------------------------------------------------------------
// GhRelease

// 署名の確かめの決まった値（第13章 3.1）。
const (
	oidcIssuer        = "https://token.actions.githubusercontent.com"
	cosignPredicate   = "https://sigstore.dev/cosign/sign/v1"
	slsaPredicate     = "https://slsa.dev/provenance/v1"
	bundleArtifactPre = "application/vnd.dev.sigstore.bundle"
	tufCacheDays      = 1
)

// GhRelease は公開リポジトリの Release から version.json と ft を取り、イメージの署名とビルド証明を検証する（第13章 3.1。
// sigstore-go と go-containerregistry。信頼の起点は TUF から取って /etc/family-talk/tuf にキャッシュする）。
type GhRelease struct {
	base     string
	repo     string
	image    string
	tufCache string
	client   *http.Client
}

// NewGhRelease は GhRelease を作る。repo は「組織/リポジトリ」、image は「ghcr.io/組織/family-talk」。
func NewGhRelease(repo, image, tufCache string) *GhRelease {
	tr := newTransport(dnsConnect)
	return &GhRelease{base: "https://github.com", repo: repo, image: image, tufCache: tufCache, client: &http.Client{Transport: tr}}
}

// get は url を取り、max を超えれば誤り。
func (d *GhRelease) get(ctx context.Context, u string, max int64, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deps: %s の取得が %d", u, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return err
	}
	if n > max {
		return errors.New("deps: 取得の上限を超えた: " + u)
	}
	return nil
}

// VersionJson は ReleaseSource.VersionJson の本物。1 MiB 以下の JSON であることだけを確かめる。
func (d *GhRelease) VersionJson(ctx context.Context) (VersionJson, error) {
	ctx, cancel := context.WithTimeout(ctx, pushTotal)
	defer cancel()
	var buf bytes.Buffer
	if err := d.get(ctx, d.base+"/"+d.repo+"/releases/latest/download/version.json", versionJSONMax, &buf); err != nil {
		return nil, err
	}
	if !json.Valid(buf.Bytes()) {
		return nil, errors.New("deps: version.json が JSON でない")
	}
	return VersionJson(buf.Bytes()), nil
}

// FetchFt は ReleaseSource.FetchFt の本物。sha256 が一致した時だけ dest に置く。
func (d *GhRelease) FetchFt(ctx context.Context, arch string, want [32]byte, dest string) error {
	if arch != "amd64" && arch != "arm64" {
		return errors.New("deps: 知らない arch: " + arch)
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTotal)
	defer cancel()
	f, err := os.CreateTemp(filepath.Dir(dest), ".ft.tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	h := sha256.New()
	if err := d.get(ctx, d.base+"/"+d.repo+"/releases/latest/download/ft-linux-"+arch, releaseSizeMax, io.MultiWriter(f, h)); err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != want {
		return errors.New("deps: ft の sha256 が version.json と一致しない")
	}
	if err := f.Chmod(0o755); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	ok = true
	return nil
}

// VerifyImage は ReleaseSource.VerifyImage の本物（第13章 3.1 の 1〜5）。
func (d *GhRelease) VerifyImage(ctx context.Context, digest string) error {
	algo, hexDigest, found := strings.Cut(digest, ":")
	raw, err := hex.DecodeString(hexDigest)
	if !found || algo != "sha256" || err != nil || len(raw) != sha256.Size {
		return errors.New("deps: digest の形が違う")
	}
	ref, err := name.NewDigest(d.image + "@" + digest)
	if err != nil {
		return err
	}
	opts := []remote.Option{remote.WithContext(ctx)}
	idx, err := remote.Referrers(ref, opts...)
	if err != nil {
		return err
	}
	manifest, err := idx.IndexManifest()
	if err != nil {
		return err
	}
	tr, err := root.FetchTrustedRootWithOptions(tuf.DefaultOptions().WithCachePath(d.tufCache).WithCacheValidity(tufCacheDays))
	if err != nil {
		return err
	}
	verifier, err := verify.NewVerifier(tr, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	identity, err := verify.NewShortCertificateIdentity(oidcIssuer, "", "",
		`^https://github\.com/`+regexp.QuoteMeta(d.repo)+`/\.github/workflows/release\.yml@refs/tags/v`)
	if err != nil {
		return err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", raw), verify.WithCertificateIdentity(identity))
	seen := map[string]bool{}
	for _, m := range manifest.Manifests {
		if !strings.HasPrefix(m.ArtifactType, bundleArtifactPre) {
			continue
		}
		b, err := d.bundleOf(ref, m.Digest.String(), opts)
		if err != nil {
			continue
		}
		res, err := verifier.Verify(b, policy)
		if err != nil || res.Statement == nil {
			continue
		}
		seen[res.Statement.GetPredicateType()] = true
	}
	if !seen[cosignPredicate] {
		return errors.New("deps: 検証できる署名が無い")
	}
	if !seen[slsaPredicate] {
		return errors.New("deps: 検証できるビルド証明が無い")
	}
	return nil
}

// bundleOf は referrer の manifest の 1 枚目の層を Sigstore の bundle として読む。
func (d *GhRelease) bundleOf(ref name.Digest, manifestDigest string, opts []remote.Option) (*bundle.Bundle, error) {
	mref, err := name.NewDigest(ref.Context().String() + "@" + manifestDigest)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(mref, opts...)
	if err != nil {
		return nil, err
	}
	layers, err := img.Layers()
	if err != nil || len(layers) == 0 {
		return nil, errors.New("deps: referrer に層が無い")
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, versionJSONMax))
	if err != nil {
		return nil, err
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return &b, nil
}

// ---------------------------------------------------------------------------
// SfuApi

// SfuApi は SFU のサーバーの SDK で SFU を操作する（第14章 5.2）。
type SfuApi struct {
	client *lksdk.RoomServiceClient
}

// NewSfuApi は SfuApi を作る（url は host.docker.internal:7880 の http、key と secret は SFU の API の鍵）。
func NewSfuApi(url string, key string, secret []byte) *SfuApi {
	return &SfuApi{client: lksdk.NewRoomServiceClient(url, key, string(secret))}
}

// DeleteRoom は SfuAdmin.DeleteRoom の本物。ルームの名は通話の id。見つからないのは成功と同じ（冪等）。
func (d *SfuApi) DeleteRoom(ctx context.Context, callId uuid.UUID) error {
	_, err := d.client.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: callId.String()})
	var te twirp.Error
	if errors.As(err, &te) && te.Code() == twirp.NotFound {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// PmtilesCli

// PmtilesCli はイメージに同梱した pmtiles の CLI（pmtiles extract）で、地図のデータの供給元から領域を切り出す（第14章 5.2、第5章 8.1）。
type PmtilesCli struct {
	bin       string
	sourceUrl string
}

// NewPmtilesCli は PmtilesCli を作る（sourceUrl は地図のデータの供給元の公開のビルド）。
func NewPmtilesCli(bin string, sourceUrl string) *PmtilesCli {
	return &PmtilesCli{bin: bin, sourceUrl: sourceUrl}
}

// Extract は MapSource.Extract の本物。region は領域の GeoJSON のファイルの道のり（srv-media が名から引く）。
// 一時の名前に書いてから dest に改名する。
func (d *PmtilesCli) Extract(ctx context.Context, region, dest string) error {
	tmp := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp-"+strconv.FormatInt(int64(os.Getpid()), 10))
	_ = os.Remove(tmp)
	if _, err := run(ctx, d.bin, []string{"extract", d.sourceUrl, tmp, "--region=" + region}, nil, nil, nil); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
