// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package deps

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/libdns/libdns"
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// ---------------------------------------------------------------------------
// 内部の範囲（第1章 10.6）

// R: R-12-5-1
func TestIsInternal(t *testing.T) {
	self := []netip.Addr{netip.MustParseAddr("198.51.100.77")}
	internal := []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.5.4", "192.0.0.9",
		"192.168.1.1", "198.18.0.1", "240.0.0.1", "255.255.255.255", "224.0.0.251", "198.51.100.77",
		"::1", "::", "::ffff:10.0.0.1", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1", "2001:db8::1",
		"2002:c000:204::1", "2001:0:4136:e378::1", "3fff::1", "5f00::1", "fc00::1", "fd12::1", "fe80::1", "ff02::1",
	}
	for _, s := range internal {
		if !isInternal(netip.MustParseAddr(s), self) {
			t.Errorf("%s を内部と判定しない", s)
		}
	}
	public := []string{"1.1.1.1", "93.184.215.14", "64:ff9b::808:808", "2606:4700:4700::1111", "::ffff:1.1.1.1"}
	for _, s := range public {
		if isInternal(netip.MustParseAddr(s), self) {
			t.Errorf("%s を内部と判定した", s)
		}
	}
	if len(nonGlobal) != 37 {
		t.Fatalf("IANA の登録の表が 37 行でない: %d", len(nonGlobal))
	}
}

// ---------------------------------------------------------------------------
// HttpFetcher

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	a, ok := f[host]
	if !ok {
		return nil, errors.New("見つからない")
	}
	return a, nil
}

var limits = FetchLimits{Connect: 2 * time.Second, Total: 5 * time.Second, MaxBytes: 1 << 20, MaxRedirects: 3}

func isMedia010(err error) bool {
	var ev *fault.Event
	return errors.As(err, &ev) && ev.Code == gen.MEDIA010
}

func TestFetcherRejects(t *testing.T) {
	f := NewHttpFetcher(fakeResolver{
		"internal.test": {netip.MustParseAddr("10.0.0.5")},
		"mixed.test":    {netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("192.168.0.1")},
		"mapped.test":   {netip.MustParseAddr("::ffff:127.0.0.1")},
		"nat64.test":    {netip.MustParseAddr("64:ff9b::a9fe:a9fe")},
		"self.test":     {netip.MustParseAddr("198.51.100.77")},
	}, []netip.Addr{netip.MustParseAddr("198.51.100.77")}, "family-talk/test")
	for _, u := range []string{
		"http://internal.test/", "https://internal.test/", "https://mixed.test/", "https://mapped.test/",
		"https://nat64.test/", "https://self.test/", "https://unknown.test/", "https://user:pw@internal.test/",
		"https://169.254.169.254/latest/meta-data", "ftp://x/", "://bad",
	} {
		if _, err := f.Get(context.Background(), u, limits); !isMedia010(err) {
			t.Errorf("%s を MEDIA-010 で拒まない: %v", u, err)
		}
	}
}

func TestFetcherOnceReadsWithinLimitAndKeepsHost(t *testing.T) {
	var gotHost, gotUA string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotUA = r.Host, r.Header.Get("User-Agent")
		if r.URL.Path == "/big" {
			_, _ = w.Write(make([]byte, 2048))
			return
		}
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()
	f := NewHttpFetcher(fakeResolver{}, nil, "family-talk/1.0")
	f.roots = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	addr := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "https://"))
	u, _ := url.Parse(fmt.Sprintf("https://example.com:%d/page", addr.Port()))
	resp, err := f.once(context.Background(), u, addr.Addr(), limits)
	if err != nil || resp.Status != 200 || string(resp.Body) != "<html>ok</html>" {
		t.Fatalf("取れない: %v %+v", err, resp)
	}
	if gotHost != u.Host || gotUA != "family-talk/1.0" {
		t.Fatalf("Host か User-Agent が違う: %q %q", gotHost, gotUA)
	}
	small := limits
	small.MaxBytes = 1024
	u.Path = "/big"
	if _, err := f.once(context.Background(), u, addr.Addr(), small); !isMedia010(err) {
		t.Fatalf("上限を超える本文を受けた: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HttpPush

func TestPushPostAndRetryAfter(t *testing.T) {
	var gotTTL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTTL = r.Header.Get("TTL")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := clock.NewTestClock(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	p := NewHttpPush(c)
	resp, err := p.Post(context.Background(), srv.URL, []byte("x"), http.Header{"TTL": {"60"}})
	if err != nil || resp.Status != 429 || resp.RetryAfter != 120*time.Second || gotTTL != "60" {
		t.Fatalf("応答か Retry-After が違う: %+v %v", resp, err)
	}
	if _, err := p.Post(context.Background(), srv.URL, make([]byte, 4097), nil); err == nil {
		t.Fatal("4096 バイトを超える本文を送った")
	}
	date := c.Now().Add(90 * time.Second).Format(http.TimeFormat)
	if got := retryAfter(date, c.Now()); got != 90*time.Second {
		t.Fatalf("HTTP の日付の Retry-After が違う: %v", got)
	}
	if retryAfter("abc", c.Now()) != 0 || retryAfter("", c.Now()) != 0 {
		t.Fatal("読めない Retry-After が 0 でない")
	}
}

// ---------------------------------------------------------------------------
// DnsApi

type fakeSetter struct {
	zone string
	recs []libdns.Record
}

func (f *fakeSetter) SetRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	f.zone, f.recs = zone, recs
	return recs, nil
}

func TestDnsApiWritesRelativeNames(t *testing.T) {
	set := &fakeSetter{}
	RegisterDnsProvider("testdns", func(token []byte) (libdns.RecordSetter, error) {
		if string(token) != "tok" {
			return nil, errors.New("トークンが違う")
		}
		return set, nil
	})
	d, err := NewDnsApi("testdns", []byte("tok"), "example.org")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetA(context.Background(), "family.example.org", netip.MustParseAddr("203.0.113.5")); err != nil {
		t.Fatal(err)
	}
	a := set.recs[0].(libdns.Address)
	if set.zone != "example.org." || a.Name != "family" || a.IP.String() != "203.0.113.5" || a.TTL != 300*time.Second {
		t.Fatalf("書いたレコードが違う: %q %+v", set.zone, a)
	}
	if err := d.SetAAAA(context.Background(), "example.org", netip.MustParseAddr("2001:db8::5")); err != nil || set.recs[0].(libdns.Address).Name != "@" {
		t.Fatalf("zone そのものが @ にならない: %v", err)
	}
	if d.SetA(context.Background(), "x.other.org", netip.MustParseAddr("203.0.113.5")) == nil {
		t.Fatal("zone の外の名を書いた")
	}
	if d.SetA(context.Background(), "family.example.org", netip.MustParseAddr("2001:db8::5")) == nil {
		t.Fatal("A に IPv6 を書いた")
	}
	if _, err := NewDnsApi("nope", nil, "example.org"); err == nil {
		t.Fatal("表に無い提供者を受けた")
	}
}

// ---------------------------------------------------------------------------
// 子のプロセス（偽の命令）

// fakeCmd は引数と 3 番・4 番のファイル記述子の中身を記録し、stdout を出して code で終わる台本を作る。
func fakeCmd(t *testing.T, stdout string, code int) (bin, argsFile, fd3File, fd4File string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, fd3File, fd4File = filepath.Join(dir, "args"), filepath.Join(dir, "fd3"), filepath.Join(dir, "fd4")
	out := filepath.Join(dir, "stdout")
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
[ -e /dev/fd/3 ] && cat /dev/fd/3 > %q 2>/dev/null
[ -e /dev/fd/4 ] && cat /dev/fd/4 > %q 2>/dev/null
cat %q
echo "最後の行" >&2
exit %d
`, argsFile, fd3File, fd4File, out, code)
	bin = filepath.Join(dir, "cmd")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile, fd3File, fd4File
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

// R: R-11-6-1
func TestResticBackupPassesPasswordByPipe(t *testing.T) {
	summary := `{"message_type":"status"}` + "\n" + `{"message_type":"summary","snapshot_id":"abc123","data_added":42}`
	bin, args, fd3, _ := fakeCmd(t, summary, 0)
	r := NewResticCli(bin)
	repo := ResticRepo{Url: "s3:https://s3.example/bucket", Password: []byte("pw-secret"), Env: map[string]string{"AWS_ACCESS_KEY_ID": "k"}}
	res, err := r.Backup(context.Background(), repo, BackupInput{StdinCmd: []string{"pg_dump", "-Fc"}, StdinFilename: "db.dump", Tags: []string{"daily"}})
	if err != nil || res.SnapshotId != "abc123" || res.AddedBytes != 42 {
		t.Fatalf("要約が読めない: %+v %v", res, err)
	}
	a := read(t, args)
	if strings.Contains(a, "pw-secret") || read(t, fd3) != "pw-secret" {
		t.Fatalf("パスワードが引数に出る、またはパイプで届かない: %q", a)
	}
	for _, want := range []string{"--password-file\n/dev/fd/3", "--stdin-from-command", "--stdin-filename\ndb.dump", "--tag\ndaily", "--\npg_dump\n-Fc"} {
		if !strings.Contains(a, want) {
			t.Fatalf("引数に %q が無い: %s", want, a)
		}
	}
	if _, err := r.Backup(context.Background(), repo, BackupInput{Paths: []string{"/data"}, StdinCmd: []string{"x"}}); err == nil {
		t.Fatal("Paths と StdinCmd の両方を受けた")
	}
}

func TestResticErrorsAndSnapshots(t *testing.T) {
	bin, _, _, _ := fakeCmd(t, "", 1)
	if err := NewResticCli(bin).Check(context.Background(), ResticRepo{Url: "/r"}, "10%"); err == nil || !strings.Contains(err.Error(), "最後の行") {
		t.Fatalf("終了コード 1 を誤りにしない、または標準エラーの最後が無い: %v", err)
	}
	snaps := `[{"id":"s1","time":"2026-10-14T03:00:00+09:00","paths":["/data/media"],"tags":["daily"]}]`
	bin2, args, _, _ := fakeCmd(t, snaps, 0)
	got, err := NewResticCli(bin2).Snapshots(context.Background(), ResticRepo{Url: "/r", Password: []byte("p")})
	if err != nil || len(got) != 1 || got[0].Id != "s1" || got[0].Time.Location() != time.UTC {
		t.Fatalf("snapshots が読めない: %+v %v", got, err)
	}
	if !strings.Contains(read(t, args), "--json") {
		t.Fatal("--json を付けない")
	}
}

func TestDockerComposeArgs(t *testing.T) {
	bin, args, _, _ := fakeCmd(t, "", 0)
	d := NewDockerApi(bin, "/etc/family-talk")
	if err := d.ComposeRecreate(context.Background(), "/etc/family-talk", []string{"api", "worker"}); err != nil {
		t.Fatal(err)
	}
	a := read(t, args)
	for _, want := range []string{"--project-directory\n/etc/family-talk", "--env-file\n/etc/family-talk/.env", "up\n-d\n--force-recreate\n--wait\napi\nworker"} {
		if !strings.Contains(a, want) {
			t.Fatalf("引数に %q が無い: %s", want, a)
		}
	}
	bin2, _, _, _ := fakeCmd(t, "busy\n", 3)
	res, err := NewDockerApi(bin2, "/etc/family-talk").Exec(context.Background(), "api", []string{"ft", "admin", "calls-active"})
	if err != nil || res.ExitCode != 3 || string(res.Stdout) != "busy\n" {
		t.Fatalf("Exec が終了コードを結果で返さない: %+v %v", res, err)
	}
}

func TestCryptsetupFormatReturnsRecoveryKey(t *testing.T) {
	key := "cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv"
	luks, _, fd3a, _ := fakeCmd(t, "", 0)
	enroll, args, fd3b, _ := fakeCmd(t, "A secret recovery key has been generated:\n    "+key+"\n", 0)
	c := NewCryptsetupCli(luks, enroll, strings.NewReader(strings.Repeat("k", 64)))
	got, err := c.Format(context.Background(), "/dev/sdz")
	if err != nil || string(got) != key {
		t.Fatalf("回復鍵が読めない: %q %v", got, err)
	}
	if read(t, fd3a) != strings.Repeat("k", 64) || read(t, fd3b) != strings.Repeat("k", 64) {
		t.Fatal("一時の鍵がパイプで渡らない")
	}
	if !strings.Contains(read(t, args), "--recovery-key\n--wipe-slot=password") {
		t.Fatalf("回復鍵の登録と一時の slot の削除を頼まない: %s", read(t, args))
	}
}

func TestSystemdDropInWritesAtomically(t *testing.T) {
	bin, args, _, _ := fakeCmd(t, "", 0)
	dir := t.TempDir()
	s := NewSystemdFiles(dir, bin)
	if err := s.WriteDropIn(context.Background(), "docker.service", "family-talk", "[Unit]\nRequiresMountsFor=/data\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docker.service.d", "family-talk.conf")); !strings.Contains(string(b), "RequiresMountsFor=/data") {
		t.Fatal("drop-in が書けない")
	}
	if read(t, args) != "daemon-reload\n" {
		t.Fatal("daemon-reload を呼ばない")
	}
	if s.WriteDropIn(context.Background(), "../x", "y", "") == nil || s.Enable(context.Background(), "a b") == nil {
		t.Fatal("道のりの外へ書ける名を受けた")
	}
}

func TestPmtilesExtractRenamesOnSuccess(t *testing.T) {
	bin, args, _, _ := fakeCmd(t, "", 0)
	dir := t.TempDir()
	dest := filepath.Join(dir, "japan.pmtiles")
	// 偽の命令は出力のファイルを作らないので、改名が失敗して誤りになる（作った時だけ置く）。
	if err := NewPmtilesCli(bin, "https://maps.example/build.pmtiles").Extract(context.Background(), "/regions/japan.geojson", dest); err == nil {
		t.Fatal("出力が無いのに成功した")
	}
	if !strings.Contains(read(t, args), "extract\nhttps://maps.example/build.pmtiles") || !strings.Contains(read(t, args), "--region=/regions/japan.geojson") {
		t.Fatalf("引数が違う: %s", read(t, args))
	}
}

// ---------------------------------------------------------------------------
// GhRelease

func TestReleaseVersionJsonAndFetchFt(t *testing.T) {
	ft := []byte("ft-binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r/releases/latest/download/version.json":
			_, _ = w.Write([]byte(`{"v":1}`))
		case "/o/r/releases/latest/download/ft-linux-amd64":
			_, _ = w.Write(ft)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := NewGhRelease("o/r", "ghcr.io/o/family-talk", t.TempDir())
	g.base = srv.URL
	v, err := g.VersionJson(context.Background())
	if err != nil || string(v) != `{"v":1}` {
		t.Fatalf("version.json が取れない: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "ft")
	if err := g.FetchFt(context.Background(), "amd64", sha256.Sum256([]byte("違う")), dest); err == nil {
		t.Fatal("sha256 が違うのに置いた")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("確かめる前の ft が置かれた")
	}
	if err := g.FetchFt(context.Background(), "amd64", sha256.Sum256(ft), dest); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dest); st == nil || st.Mode().Perm() != 0o755 {
		t.Fatal("一致した ft が置かれない")
	}
	if g.FetchFt(context.Background(), "riscv64", sha256.Sum256(ft), dest) == nil {
		t.Fatal("知らない arch を受けた")
	}
	if g.VerifyImage(context.Background(), "sha256:xyz") == nil || g.VerifyImage(context.Background(), "md5:"+strings.Repeat("0", 64)) == nil {
		t.Fatal("digest の形の誤りを受けた")
	}
}

// ---------------------------------------------------------------------------
// SfuApi

func TestSfuDeleteRoomIsIdempotent(t *testing.T) {
	var gotRoom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livekit.DeleteRoomRequest
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("Content-Type"), "protobuf") {
			_ = proto.Unmarshal(b, &req)
		} else {
			_ = protojson.Unmarshal(b, &req)
		}
		gotRoom = req.GetRoom()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","msg":"room not found"}`))
	}))
	defer srv.Close()
	id := uuid.Must(uuid.NewV7())
	if err := NewSfuApi(srv.URL, "key", []byte(strings.Repeat("s", 32))).DeleteRoom(context.Background(), id); err != nil {
		t.Fatalf("無いルームの削除が誤りになった: %v", err)
	}
	if gotRoom != id.String() {
		t.Fatalf("ルームの名が通話の id でない: %q", gotRoom)
	}
}

// seqCmd は呼ばれるたびに outs の次の出力を返し、4 番のファイル記述子の中身を記録する台本を作る。
func seqCmd(t *testing.T, outs []string) (bin, fd4File string) {
	t.Helper()
	dir := t.TempDir()
	for i, o := range outs {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("out%d", i)), []byte(o), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fd4File = filepath.Join(dir, "fd4")
	script := fmt.Sprintf(`#!/bin/sh
n=$(cat %[1]q/count 2>/dev/null || echo 0)
echo $((n+1)) > %[1]q/count
[ -e /dev/fd/4 ] && cat /dev/fd/4 > %[2]q 2>/dev/null
cat %[1]q/out$n
`, dir, fd4File)
	bin = filepath.Join(dir, "cmd")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, fd4File
}

// R: R-12-7-1
func TestResticKeyAddReturnsTheNewKey(t *testing.T) {
	bin, fd4 := seqCmd(t, []string{`[{"id":"old"}]`, "", `[{"id":"old"},{"id":"new1"}]`})
	id, err := NewResticCli(bin).KeyAdd(context.Background(), ResticRepo{Url: "/r", Password: []byte("p")}, []byte("new-pw"))
	if err != nil || id != "new1" {
		t.Fatalf("足した鍵の id が違う: %q %v", id, err)
	}
	if read(t, fd4) != "new-pw" {
		t.Fatal("新しいパスワードが 4 番のパイプで渡らない")
	}
}
