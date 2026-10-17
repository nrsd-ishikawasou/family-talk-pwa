// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// ロングポールの待ちの上限（第2章 5.9）、/health の外への確かめの打ち切り、worker が古いとみなす時間（第11章 11）。
const (
	LongPollMax      = 30 * time.Second
	ProbeTimeout     = 2 * time.Second
	workerStale      = 15 * time.Minute
	sfuHealthURL     = "http://host.docker.internal:7880/"
	nonceMarker      = "__FT_NONCE__"
	syncLimitDefault = 50
)

// SyncHandler は /sync（B-044。gen.SyncApi）。
type SyncHandler struct {
	base  Base
	hub   *Hub
	links Links
}

var _ gen.SyncApi = (*SyncHandler)(nil)

// GetSync は since なしで初回（Snapshotter）、since ありで Feed.ReadSince。保持の外は NET-005。
// timeout > 0 で 0 件なら hub の通知を待ってから 1 回読み直す。待ちの間に Shutdown が来たら 200 と空の配列。
func (h *SyncHandler) GetSync(ctx context.Context, request gen.GetSyncRequestObject) (gen.GetSyncResponseObject, error) {
	p := PrincipalFrom(ctx)
	if p.Kind != KindUser || p.UserId == nil || p.DeviceId == nil {
		return nil, fault.New(cgen.AUTH001, nil)
	}
	prm := request.Params
	if prm.Since == nil {
		snap, pos, err := h.links.Snapshot.Snapshot(ctx, p)
		if err != nil {
			return nil, err
		}
		return gen.GetSync200JSONResponse{Pos: int(pos), Snapshot: &snap, HasMore: false}, nil
	}
	since := int64(*prm.Since)
	min, err := h.base.Feed.MinPos(ctx, *p.UserId)
	if err != nil {
		return nil, err
	}
	if since < min-1 {
		return nil, fault.New(cgen.NET005, nil)
	}
	limit := syncLimitDefault
	if prm.Limit != nil {
		limit = *prm.Limit
	}
	events, hasMore, last, err := h.read(ctx, p, since, limit)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 && !hasMore && prm.Timeout != nil && *prm.Timeout > 0 {
		wait := time.Duration(*prm.Timeout) * time.Second
		if wait > LongPollMax {
			wait = LongPollMax
		}
		wctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		// 起こされても新しい物が無ければ（全員を起こす合図など）、期限まで待ち直す。期限と Shutdown では空で返す。
		for len(events) == 0 && !hasMore && h.hub.Wait(wctx, *p.UserId) {
			if events, hasMore, last, err = h.read(ctx, p, since, limit); err != nil {
				return nil, err
			}
		}
	}
	return gen.GetSync200JSONResponse{Pos: int(last), Events: &events, HasMore: hasMore}, nil
}

// read は since より後のうち要求の端末に届く物を limit まで読み、フレームにする（第2章 5.9）。
// last は、続きがあれば返した最後の pos、無ければ利用者の最新の pos（他の端末宛ての行を含む）。
// 最新の pos は行を読む前に取る：後に取ると、読みと取得の間に確定したこの端末宛ての行を端末が飛ばす。
func (h *SyncHandler) read(ctx context.Context, p Principal, since int64, limit int) ([]map[string]any, bool, int64, error) {
	latest, err := h.base.Feed.LastPos(ctx, h.base.Pool, *p.UserId)
	if err != nil {
		return nil, false, since, err
	}
	rows, err := h.base.Feed.ReadSince(ctx, *p.UserId, *p.DeviceId, since, limit+1)
	if err != nil {
		return nil, false, since, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	last := max(since, latest)
	if len(rows) > 0 {
		last = int64(rows[len(rows)-1].Pos)
		if !hasMore {
			last = max(last, latest)
		}
	}
	frames, _, err := h.hub.frames(ctx, *p.UserId, *p.DeviceId, rows)
	if err != nil {
		return nil, false, since, err
	}
	out := make([]map[string]any, 0, len(frames))
	for _, f := range frames {
		var m map[string]any
		if err := json.Unmarshal(f, &m); err != nil {
			return nil, false, since, err
		}
		out = append(out, m)
	}
	return out, hasMore, last, nil
}

// Health は /health（B-046。第11章 11）。
type Health struct {
	base   Base
	links  Links
	state  ServerStateReader
	client *http.Client
	sfuURL string
}

// Get は checks を集めて status と HTTP の状態を決める。外への確かめは並行に、各 2 秒で打ち切る。
func (h *Health) Get(ctx context.Context) (gen.GetHealthResponse, int) {
	var res gen.GetHealthResponse
	var facts HealthFacts
	probeOk := false
	var stats struct {
		last *time.Time
	}
	var wg sync.WaitGroup
	run := func(fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
			defer cancel()
			fn(pctx)
		}()
	}
	run(func(c context.Context) { res.Checks.Postgres = h.base.Pool.Ping(c) == nil })
	run(func(c context.Context) { res.Checks.Sfu = h.sfuOk(c) })
	run(func(c context.Context) {
		if f, err := h.links.Probe.Probe(c); err == nil {
			facts, probeOk = f, true
		}
	})
	run(func(c context.Context) {
		if s, err := h.base.Jobs.Stats(c); err == nil {
			stats.last = s.LastDoneAt
		}
	})
	wg.Wait()
	res.Checks.CaddyCertDays = facts.CaddyCertDays
	res.Checks.DiskFreePct = facts.DiskFreePct
	res.Checks.DdnsLastOkAt = facts.DdnsLastOkAt
	// 回線の形は srv-admin の値、取れなければ設定（server.mode）の値。
	res.Checks.Mode = gen.GetHealthResponseChecksMode(facts.Mode)
	if res.Checks.Mode == "" {
		res.Checks.Mode = gen.GetHealthResponseChecksMode(h.base.Config.Mode)
	}
	res.Checks.WorkerLastJobAt = stats.last
	return h.status(res, stats.last, probeOk)
}

// status は第11章 11 の表で status、reason、HTTP の状態を決める。
// srv-admin の値（証明書、ディスク、DDNS）が取れない時も degraded にする（知らずに ok を返さない）。
func (h *Health) status(res gen.GetHealthResponse, lastJob *time.Time, probeOk bool) (gen.GetHealthResponse, int) {
	down := func(reason gen.GetHealthResponseReason) (gen.GetHealthResponse, int) {
		res.Status, res.Reason = gen.Down, &reason
		return res, http.StatusServiceUnavailable
	}
	switch h.state.State() {
	case StateStopped:
		return down(gen.GetHealthResponseReasonStopped)
	}
	if !res.Checks.Postgres {
		return down(gen.GetHealthResponseReasonPostgres)
	}
	switch h.state.State() {
	case StateReadOnly:
		reason := gen.GetHealthResponseReason(h.state.ReadOnlyReason())
		res.Status, res.Reason = gen.Readonly, &reason
		return res, http.StatusServiceUnavailable
	case StateBeforeSetup, StateInitial:
		res.Status = gen.Setup
		return res, http.StatusOK
	}
	now := h.base.Deps.Clock.Now()
	if !res.Checks.Sfu || !probeOk || lastJob == nil || now.Sub(*lastJob) > workerStale {
		res.Status = gen.Degraded
		return res, http.StatusOK
	}
	res.Status = gen.Ok
	return res, http.StatusOK
}

// sfuOk は SFU の疎通（GET / が 200。調査資料 1.17）。
func (h *Health) sfuOk(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.sfuURL, nil)
	if err != nil {
		return false
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// releaseFile は version.json（ft.release/1）のうち本ブロックが読む欄。
type releaseFile struct {
	Server struct {
		Version    string    `json:"version"`
		ReleasedAt time.Time `json:"released_at"`
	} `json:"server"`
	Boundary string `json:"boundary"`
	MinApp   struct {
		Web     string `json:"web"`
		Android string `json:"android"`
	} `json:"min_app"`
}

// VersionHandler は /version（ft.version/1）。min_app は dl/version.json を起動の時に読む。
type VersionHandler struct {
	base    Base
	version gen.FtVersion1
	minApp  map[string]string
}

// newVersionHandler は version.json を読んで確かめる。無い・壊れていれば Fault（イメージの誤り）。
// deprecations は x-deprecation を持つ経路。
func newVersionHandler(base Base, authz *AuthzTable) (*VersionHandler, error) {
	b, err := os.ReadFile(filepath.Join(base.WwwDir, "dl", "version.json"))
	if err != nil {
		return nil, fault.Wrap(err)
	}
	var rel releaseFile
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, fault.Wrap(err)
	}
	for _, v := range []string{rel.Server.Version, rel.Boundary, rel.MinApp.Web, rel.MinApp.Android} {
		if !semver.IsValid("v" + v) {
			return nil, fault.Wrap(fmt.Errorf("version.json の版の形が違う: %q", v))
		}
	}
	if rel.Server.ReleasedAt.IsZero() {
		return nil, fault.Wrap(fmt.Errorf("version.json に公開日が無い"))
	}
	h := &VersionHandler{base: base, minApp: map[string]string{"web": rel.MinApp.Web, "android": rel.MinApp.Android}}
	h.version.Server = rel.Server.Version
	h.version.Boundary = rel.Boundary
	h.version.ReleasedAt = rel.Server.ReleasedAt.UTC()
	h.version.MinApp.Web = rel.MinApp.Web
	h.version.MinApp.Android = rel.MinApp.Android
	h.version.Deprecations = []struct {
		Path   string             `json:"path"`
		Sunset gen.CommonRfc33391 `json:"sunset"`
	}{}
	for _, r := range authz.Routes() {
		if r.Sunset != nil {
			h.version.Deprecations = append(h.version.Deprecations, struct {
				Path   string             `json:"path"`
				Sunset gen.CommonRfc33391 `json:"sunset"`
			}{Path: r.Path, Sunset: *r.Sunset})
		}
	}
	return h, nil
}

// Get は server、boundary、min_app、released_at、deprecations を返す。
func (h *VersionHandler) Get() gen.FtVersion1 {
	return h.version
}

// MinApp は platform（web、android）の最低の版。AppVersion の middleware が使う。
func (h *VersionHandler) MinApp(platform string) (string, bool) {
	v, ok := h.minApp[platform]
	return v, ok
}

// Metrics は /metrics（OpenMetrics。opt-in の server.metrics。第11章 11）。外の部品を足さずに文の形を書く。
type Metrics struct {
	base      Base
	hub       *Hub
	upWs      *UpWsHandler
	mu        sync.Mutex
	requests  map[[2]string]int64
	durations map[string]*histogram
	// ver は動いているサーバーの版（ft_build_info）。
	ver string
}

// 所要の区切り（秒）。
var durationBuckets = []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10}

type histogram struct {
	counts []int64
	sum    float64
	n      int64
}

// newMetrics は Metrics を作る。
func newMetrics(base Base, hub *Hub, upWs *UpWsHandler, ver string) *Metrics {
	return &Metrics{base: base, hub: hub, upWs: upWs, requests: map[[2]string]int64{}, durations: map[string]*histogram{}, ver: ver}
}

// Observe は 1 つの要求を数える（RequestId の middleware が呼ぶ）。ラベルは operationId と状態の数だけ。
func (m *Metrics) Observe(op string, status int, d time.Duration) {
	if m == nil || op == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{op, fmt.Sprint(status)}]++
	hg := m.durations[op]
	if hg == nil {
		hg = &histogram{counts: make([]int64, len(durationBuckets))}
		m.durations[op] = hg
	}
	s := d.Seconds()
	for i, b := range durationBuckets {
		if s <= b {
			hg.counts[i]++
		}
	}
	hg.sum += s
	hg.n++
}

// Write は OpenMetrics の文を書く（# EOF で終える）。
func (m *Metrics) Write(ctx context.Context) []byte {
	var b bytes.Buffer
	m.mu.Lock()
	b.WriteString("# TYPE ft_http_requests counter\n")
	keys := make([][2]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	for _, k := range keys {
		fmt.Fprintf(&b, "ft_http_requests_total{op=\"%s\",code=\"%s\"} %d\n", k[0], k[1], m.requests[k])
	}
	b.WriteString("# TYPE ft_http_request_duration_seconds histogram\n")
	ops := make([]string, 0, len(m.durations))
	for op := range m.durations {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		hg := m.durations[op]
		for i, le := range durationBuckets {
			fmt.Fprintf(&b, "ft_http_request_duration_seconds_bucket{op=\"%s\",le=\"%g\"} %d\n", op, le, hg.counts[i])
		}
		fmt.Fprintf(&b, "ft_http_request_duration_seconds_bucket{op=\"%s\",le=\"+Inf\"} %d\n", op, hg.n)
		fmt.Fprintf(&b, "ft_http_request_duration_seconds_sum{op=\"%s\"} %g\n", op, hg.sum)
		fmt.Fprintf(&b, "ft_http_request_duration_seconds_count{op=\"%s\"} %d\n", op, hg.n)
	}
	m.mu.Unlock()
	b.WriteString("# TYPE ft_ws_connections gauge\n")
	m.hub.mu.RLock()
	fmt.Fprintf(&b, "ft_ws_connections %d\n", len(m.hub.conns))
	m.hub.mu.RUnlock()
	b.WriteString("# TYPE ft_up_connections gauge\n")
	m.upWs.mu.Lock()
	fmt.Fprintf(&b, "ft_up_connections %d\n", len(m.upWs.conns))
	m.upWs.mu.Unlock()
	if m.base.Jobs == nil {
		// 仕事の数は Build の後だけ（Base.Jobs は Build が確かめる）。
	} else if s, err := m.base.Jobs.Stats(ctx); err == nil {
		b.WriteString("# TYPE ft_jobs gauge\n")
		fmt.Fprintf(&b, "ft_jobs{state=\"queued\"} %d\nft_jobs{state=\"running\"} %d\nft_jobs{state=\"failed\"} %d\n", s.Queued, s.Running, s.Failed)
	}
	b.WriteString("# TYPE ft_build info\n")
	fmt.Fprintf(&b, "ft_build_info{version=\"%s\"} 1\n", m.ver)
	b.WriteString("# EOF\n")
	return b.Bytes()
}

// Handler は OpenMetrics の形式で書く handler。
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		_, _ = w.Write(m.Write(r.Context()))
	})
}

// serverApi は gen.ServerApi（Health、VersionHandler、Metrics を束ねる）。
type serverApi struct {
	health  *Health
	version *VersionHandler
	metrics *Metrics
}

var _ gen.ServerApi = (*serverApi)(nil)

// GetHealth は 200（ok、degraded、setup）か 503（readonly、down）を返す。
func (s *serverApi) GetHealth(ctx context.Context, request gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	res, code := s.health.Get(ctx)
	if code == http.StatusServiceUnavailable {
		return gen.GetHealth503JSONResponse(res), nil
	}
	return gen.GetHealth200JSONResponse(res), nil
}

// GetVersion は ft.version/1 を返す。
func (s *serverApi) GetVersion(ctx context.Context, request gen.GetVersionRequestObject) (gen.GetVersionResponseObject, error) {
	return gen.GetVersion200JSONResponse(s.version.Get()), nil
}

// GetMetrics は OpenMetrics の本文を返す。server.metrics が偽なら経路を登録しないので、ここには来ない。
func (s *serverApi) GetMetrics(ctx context.Context, request gen.GetMetricsRequestObject) (gen.GetMetricsResponseObject, error) {
	body := s.metrics.Write(ctx)
	return gen.GetMetrics200ApplicationopenmetricsTextResponse{Body: bytes.NewReader(body), ContentLength: int64(len(body))}, nil
}

// InternalAuth は /internal/auth（forward auth。B-047。gen.InternalApi）。
type InternalAuth struct {
	verifiers Verifiers
}

var _ gen.InternalApi = (*InternalAuth)(nil)

// CheckForwardAuth は Cookie が有効なら 200 と X-User-Id（本文なし）、無効なら 401。
// 主体は Session の middleware が ctx に入れている（x-authz の internal は経路を通す）。
func (a *InternalAuth) CheckForwardAuth(ctx context.Context, request gen.CheckForwardAuthRequestObject) (gen.CheckForwardAuthResponseObject, error) {
	p := PrincipalFrom(ctx)
	if p.Kind != KindUser || p.UserId == nil {
		return gen.CheckForwardAuth401Response{}, nil
	}
	id := p.UserId.String()
	return gen.CheckForwardAuth200Response{Headers: gen.CheckForwardAuth200ResponseHeaders{XUserId: &id}}, nil
}

// SpaIndex は画面の入口に、応答ごとの nonce を埋めた index.html を返す（第8章 6。CSP Level 2 の 4.2）。
type SpaIndex struct {
	base   Base
	index  []byte
	domain string
}

// newSpaIndex は index.html を読み、nonce の置き場があることを確かめる。無ければ Fault（イメージの誤り）。
func newSpaIndex(base Base, wsOrigin string) (*SpaIndex, error) {
	b, err := os.ReadFile(filepath.Join(base.WwwDir, "index.html"))
	if err != nil {
		return nil, fault.Wrap(err)
	}
	if !bytes.Contains(b, []byte(nonceMarker)) {
		return nil, fault.Wrap(fmt.Errorf("index.html に nonce の置き場が無い"))
	}
	return &SpaIndex{base: base, index: b, domain: wsOrigin}, nil
}

// csp は第8章 6 の値（nonce と wss の宛先を入れる）。
func (s *SpaIndex) csp(nonce string) string {
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'nonce-" + nonce + "' 'strict-dynamic' 'wasm-unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"connect-src 'self' " + s.domain,
		"img-src 'self' blob: data:",
		"media-src 'self' blob:",
		"worker-src 'self' blob:",
		"font-src 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"upgrade-insecure-requests",
	}, "; ")
}

// ServeHTTP は 128 ビットの nonce を作って index に埋め、同じ nonce の CSP を付けて返す。
func (s *SpaIndex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeError(w, r, err)
		return
	}
	nonce := base64.StdEncoding.EncodeToString(raw[:])
	h := w.Header()
	h.Set("Content-Security-Policy", s.csp(nonce))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(bytes.ReplaceAll(s.index, []byte(nonceMarker), []byte(nonce)))
}
