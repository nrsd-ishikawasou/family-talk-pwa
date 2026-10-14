// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/deps"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
)

var (
	testNow  = time.Unix(1_800_000_000, 0).UTC()
	userA    = uuid.Must(uuid.FromString("01890a5d-ac96-774b-bcce-b302099a8001"))
	deviceA  = uuid.Must(uuid.FromString("01890a5d-ac96-774b-bcce-b302099a8002"))
	deviceB  = uuid.Must(uuid.FromString("01890a5d-ac96-774b-bcce-b302099a8003"))
	roomA    = uuid.Must(uuid.FromString("01890a5d-ac96-774b-bcce-b302099a8004"))
	roomB    = uuid.Must(uuid.FromString("01890a5d-ac96-774b-bcce-b302099a8005"))
	idemKey  = "01890a5d-ac96-774b-bcce-b302099a8099"
	testHost = "family.example"
)

// 偽の Verifiers。
type fakeSession struct {
	p   Principal
	ev  *fault.Event
	exp time.Time
}

func (f fakeSession) Verify(context.Context, string) (Principal, *fault.Event) { return f.p, f.ev }
func (f fakeSession) ExpiresHeader(Principal) time.Time                        { return f.exp }

type fakeToken struct {
	p  Principal
	ev *fault.Event
}

func (f fakeToken) Verify(context.Context, string) (Principal, *fault.Event) { return f.p, f.ev }

type fakeMembership map[uuid.UUID]bool

func (f fakeMembership) IsMember(_ context.Context, _, roomId uuid.UUID) (bool, error) {
	return f[roomId], nil
}
func (f fakeMembership) RoomRole(context.Context, uuid.UUID, uuid.UUID) (RoomRole, error) {
	return RoomRoleMember, nil
}

type fakeApproval struct{}

func (fakeApproval) IsApproved(context.Context, uuid.UUID) (bool, error) { return true, nil }

type fakeState struct {
	s      ServerState
	reason string
}

func (f fakeState) State() ServerState     { return f.s }
func (f fakeState) ReadOnlyReason() string { return f.reason }

func member() Principal {
	u, d := userA, deviceA
	return Principal{Kind: KindUser, UserId: &u, DeviceId: &d, Role: RoleMember, DeviceApproved: true}
}

// newTestChain は偽の部品で MiddlewareChain を組む（DB は使わない）。
func newTestChain(t *testing.T, v Verifiers) *MiddlewareChain {
	t.Helper()
	clk := clock.NewTestClock(testNow)
	cfg, err := config.Load(map[string]string{"FT_DOMAIN": testHost, "FT_DNS_PROVIDER": "test"}, nil, cgen.Items)
	if err != nil {
		t.Fatal(err)
	}
	base := Base{Config: cfg, Deps: deps.Deps{Clock: clk}, Log: logx.New(io.Discard, "api", logx.LevelDebug, clk.Now),
		Limiter: ratelimit.New(clk, cgen.Policies), Ids: ids.New(clk, rand.Reader)}
	authz, err := FromSpec(gen.Spec, v.Membership)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := libopenapi.NewDocument(gen.Spec)
	if err != nil {
		t.Fatal(err)
	}
	val, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	ver := &VersionHandler{minApp: map[string]string{"web": "1.2.0", "android": "1.0.0"}}
	origin, _ := origins(cfg)
	return &MiddlewareChain{base: base, authz: authz, verifiers: v, version: ver, validator: val, origin: origin}
}

func defaultVerifiers() Verifiers {
	return Verifiers{Session: fakeSession{p: member()}, Shell: fakeToken{ev: fault.New(cgen.AUTH001, nil)},
		Ws: fakeToken{p: member()}, Membership: fakeMembership{roomA: true}, Approval: fakeApproval{},
		State: fakeState{s: StateNormal}}
}

// problemCode は応答の problem の code。
func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Code     string `json:"code"`
		Instance string `json:"instance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		return ""
	}
	return p.Code
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

// R: R-2-1-1
func TestFromSpecReadsEveryOperationOnce(t *testing.T) {
	authz, err := FromSpec(gen.Spec, fakeMembership{})
	if err != nil {
		t.Fatal(err)
	}
	routes := authz.Routes()
	if len(routes) != 154 { // 162 の operation から caddy が配る 8 を除く
		t.Fatalf("経路の数が違う: %d", len(routes))
	}
	ws, ok := authz.Route("connectWs")
	if !ok || ws.WebSocket != "ws" || ws.Path != "/api/v1/ws" {
		t.Fatalf("connectWs の属性が違う: %+v", ws)
	}
	if r, ok := authz.RouteByPattern("GET /health"); !ok || r.OperationId != "getHealth" {
		t.Fatal("pattern から引けない")
	}
	for _, r := range routes {
		if len(r.Authz) == 0 {
			t.Fatalf("x-authz が無い: %s", r.OperationId)
		}
	}
}

func TestFromSpecRejectsBrokenAttributes(t *testing.T) {
	op := func(extra string) []byte {
		return []byte("paths:\n  /a:\n    get:\n      operationId: a\n      x-idempotency: none\n" + extra)
	}
	cases := map[string][]byte{
		"x-authz が無い":      op(""),
		"表に無い x-authz":     op("      x-authz: [nobody]\n"),
		"表に無い x-reauth":    op("      x-authz: [none]\n      x-reauth: sometimes\n"),
		"表に無い policy":      op("      x-authz: [none]\n      x-ratelimit: [nothing]\n"),
		"表に無い x-readonly":  op("      x-authz: [none]\n      x-readonly: deny\n"),
		"sunset が at より前":  op("      x-authz: [none]\n      deprecated: true\n      x-deprecation: {at: 2027-01-01T00:00:00Z, sunset: 2026-01-01T00:00:00Z}\n"),
		"deprecated が無い廃止": op("      x-authz: [none]\n      x-deprecation: {at: 2026-01-01T00:00:00Z, sunset: 2027-01-01T00:00:00Z}\n"),
	}
	for name, spec := range cases {
		if _, err := FromSpec(spec, fakeMembership{}); err == nil {
			t.Errorf("%s：Fault にならない", name)
		}
	}
	good := op("      x-authz: [none]\n      deprecated: true\n      x-deprecation: {at: 2026-01-01T00:00:00Z, sunset: 2027-01-01T00:00:00Z}\n")
	tbl, err := FromSpec(good, fakeMembership{})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := tbl.Route("a"); r.DeprecatedAt == nil || r.Sunset == nil {
		t.Fatal("x-deprecation を読まない")
	}
}

// R: R-2-1-1
func TestCheckTable(t *testing.T) {
	tbl := &AuthzTable{membership: fakeMembership{roomA: true}, routes: map[string]Route{}}
	add := func(op string, authz ...string) { tbl.routes[op] = Route{OperationId: op, Authz: authz} }
	add("none", "none")
	add("session", "session")
	add("approved", "approved")
	add("self", "device_self")
	add("member", "room_member")
	add("admin", "admin")
	add("shell", "shell")
	add("token", "invite_token")
	add("approvedOrToken", "approved", "invite_token")
	unapproved := member()
	unapproved.DeviceApproved = false
	admin := member()
	admin.Role = RoleAdmin
	sd, _ := uuid.NewV7()
	shell := Principal{Kind: KindShell, DeviceId: &sd}
	none := Principal{Kind: KindNone}
	ra, rb, da, db := roomA, roomB, deviceA, deviceB
	cases := []struct {
		op   string
		p    Principal
		args AuthzArgs
		want string
	}{
		{"none", none, AuthzArgs{}, ""},
		{"session", none, AuthzArgs{}, cgen.AUTH001},
		{"session", unapproved, AuthzArgs{}, ""},
		{"approved", unapproved, AuthzArgs{}, cgen.KEY004},
		{"approved", member(), AuthzArgs{}, ""},
		{"self", unapproved, AuthzArgs{DeviceId: &da}, ""},
		{"self", member(), AuthzArgs{DeviceId: &db}, cgen.KEY001},
		{"member", member(), AuthzArgs{RoomId: &ra}, ""},
		{"member", member(), AuthzArgs{RoomId: &rb}, cgen.ROOM002},
		{"member", unapproved, AuthzArgs{RoomId: &ra}, cgen.KEY004},
		{"admin", member(), AuthzArgs{}, cgen.ADM001},
		{"admin", admin, AuthzArgs{}, ""},
		{"shell", shell, AuthzArgs{}, ""},
		{"shell", member(), AuthzArgs{}, cgen.AUTH001},
		{"token", none, AuthzArgs{}, ""},
		{"approvedOrToken", none, AuthzArgs{}, ""},
		{"admin", none, AuthzArgs{}, cgen.AUTH001},
	}
	for _, c := range cases {
		got := ""
		if ev := tbl.Check(context.Background(), c.op, c.p, c.args); ev != nil {
			got = ev.Code
		}
		if got != c.want {
			t.Errorf("%s（%s）：%q、期待 %q", c.op, c.p.Kind, got, c.want)
		}
	}
}

// R: R-2-1-2
func TestFetchMetadata(t *testing.T) {
	c := newTestChain(t, defaultVerifiers())
	h := c.RequestId(c.FetchMetadata(okHandler))
	cases := []struct {
		method, path, site, requested string
		want                          int
	}{
		{"POST", "/api/v1/rooms", "same-origin", "family-talk", http.StatusNoContent},
		{"POST", "/api/v1/rooms", "", "family-talk", http.StatusNoContent},
		{"POST", "/api/v1/rooms", "none", "family-talk", http.StatusNoContent},
		{"POST", "/api/v1/rooms", "cross-site", "family-talk", http.StatusForbidden},
		{"POST", "/api/v1/rooms", "same-site", "family-talk", http.StatusForbidden},
		{"DELETE", "/api/v1/rooms/x", "same-origin", "", http.StatusForbidden},
		{"GET", "/api/v1/rooms", "cross-site", "", http.StatusNoContent},
		{"POST", "/up/abc", "", "", http.StatusNoContent},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.requested != "" {
			r.Header.Set("X-Requested-With", c.requested)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s %s site=%q xrw=%q：%d、期待 %d", c.method, c.path, c.site, c.requested, rec.Code, c.want)
		}
		if c.want == http.StatusForbidden && problemCode(t, rec) != cgen.NET003 {
			t.Errorf("NET-003 でない：%s", rec.Body)
		}
	}
}

func TestAppVersion(t *testing.T) {
	c := newTestChain(t, defaultVerifiers())
	h := c.RequestId(c.AppVersion(okHandler))
	cases := map[string]int{
		"web/1.2.0":        http.StatusNoContent,
		"web/1.10.0":       http.StatusNoContent,
		"web/1.1.9":        http.StatusUpgradeRequired,
		"web/1.2.0-rc.1":   http.StatusUpgradeRequired, // SemVer では前の版
		"android/0.9.0":    http.StatusUpgradeRequired,
		"android/1.0.0+b5": http.StatusNoContent,
		"":                 http.StatusNoContent,
		"ios/0.1.0":        http.StatusNoContent,
		"web/one":          http.StatusNoContent,
	}
	for header, want := range cases {
		r := httptest.NewRequest("GET", "/api/v1/me", nil)
		if header != "" {
			r.Header.Set("X-Family-Talk-App", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%q：%d、期待 %d", header, rec.Code, want)
		}
		if want == http.StatusUpgradeRequired && problemCode(t, rec) != cgen.NET006 {
			t.Errorf("NET-006 でない：%s", rec.Body)
		}
	}
}

// R: R-2-1-1
func TestValidateIdempotencyKeyAndParams(t *testing.T) {
	c := newTestChain(t, defaultVerifiers())
	route := func(op string, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithOperation(r.Context(), op)))
		})
	}
	h := c.RequestId(route("requestRecovery", c.BodyLimit(c.Validate(okHandler))))
	send := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/me/recovery/request", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := send("", `{}`); problemCode(t, rec) != cgen.NET007 {
		t.Fatalf("鍵の欠けが NET-007 でない：%s", rec.Body)
	}
	if rec := send("x", `{"zzz":1}`); problemCode(t, rec) != cgen.NET007 {
		t.Fatalf("鍵の形の誤りが NET-007 でない：%s", rec.Body)
	}
	rec := send(idemKey, `{"zzz":1}`)
	if problemCode(t, rec) != cgen.NET001 {
		t.Fatalf("本文の誤りが NET-001 でない：%s", rec.Body)
	}
	var p struct {
		Params struct {
			Errors []struct{ Path, Message string } `json:"errors"`
		} `json:"params"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if len(p.Params.Errors) == 0 || !strings.HasPrefix(p.Params.Errors[0].Path, "body") {
		t.Fatalf("params.errors[] が違う：%s", rec.Body)
	}
}

func TestBodyLimit(t *testing.T) {
	c := newTestChain(t, defaultVerifiers())
	h := c.RequestId(c.BodyLimit(okHandler))
	big := strings.NewReader(strings.Repeat("a", BodyLimitJson+1))
	r := httptest.NewRequest("POST", "/api/v1/rooms", big)
	r.ContentLength = -1 // Content-Length が無い時も読んで切る
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge || problemCode(t, rec) != cgen.NET009 {
		t.Fatalf("NET-009 でない：%d %s", rec.Code, rec.Body)
	}
}

// R: R-2-3-2
func TestSessionRejectsOnlyWhenEveryValueNeedsASession(t *testing.T) {
	v := defaultVerifiers()
	v.Session = fakeSession{ev: fault.New(cgen.AUTH002, map[string]any{"reason": "expired"})}
	c := newTestChain(t, v)
	for op, want := range map[string]int{
		"getMe":            http.StatusUnauthorized, // approved
		"getFamily":        http.StatusNoContent,    // none を含む
		"connectWs":        http.StatusNoContent,    // ws_token
		"checkForwardAuth": http.StatusNoContent,    // internal
	} {
		r := httptest.NewRequest("GET", "/x", nil)
		r.AddCookie(&http.Cookie{Name: "__Host-session", Value: "old"})
		rec := httptest.NewRecorder()
		c.RequestId(withOp(op, c.Session(okHandler))).ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%s：%d、期待 %d", op, rec.Code, want)
		}
	}
}

func withOp(op string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithOperation(r.Context(), op)))
	})
}

func TestSessionExpiresHeader(t *testing.T) {
	v := defaultVerifiers()
	v.Session = fakeSession{p: member(), exp: testNow.Add(6 * 24 * time.Hour)}
	c := newTestChain(t, v)
	r := httptest.NewRequest("GET", "/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-session", Value: "s"})
	rec := httptest.NewRecorder()
	c.RequestId(withOp("getMe", c.Session(okHandler))).ServeHTTP(rec, r)
	if rec.Header().Get("X-Session-Expires") != testNow.Add(6*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("X-Session-Expires が違う：%q", rec.Header().Get("X-Session-Expires"))
	}
}

// R: R-11-8-1
func TestReadOnlyGate(t *testing.T) {
	v := defaultVerifiers()
	v.State = fakeState{s: StateReadOnly, reason: "capacity"}
	c := newTestChain(t, v)
	cases := []struct {
		method, op string
		want       int
	}{
		{"GET", "getMe", http.StatusNoContent},
		{"POST", "createRoomEvent", http.StatusServiceUnavailable},
		{"POST", "verifyPasskeyLogin", http.StatusNoContent}, // x-readonly: allow
		{"POST", "runAdminBackup", http.StatusNoContent},
	}
	for _, cs := range cases {
		if _, ok := c.authz.Route(cs.op); !ok {
			t.Fatalf("表に無い経路：%s", cs.op)
		}
		rec := httptest.NewRecorder()
		c.RequestId(withOp(cs.op, c.ReadOnlyGate(okHandler))).ServeHTTP(rec, httptest.NewRequest(cs.method, "/x", nil))
		if rec.Code != cs.want {
			t.Errorf("%s %s：%d、期待 %d", cs.method, cs.op, rec.Code, cs.want)
		}
		if cs.want == http.StatusServiceUnavailable {
			if problemCode(t, rec) != cgen.SRV001 || !strings.Contains(rec.Body.String(), `"capacity"`) || rec.Header().Get("Retry-After") != "60" {
				t.Errorf("SRV-001 の形が違う：%s", rec.Body)
			}
		}
	}
}

// R: R-2-1-1
func TestRateLimitHeadersAndCodes(t *testing.T) {
	c := newTestChain(t, defaultVerifiers())
	h := c.RequestId(withOp("verifyPasskeyLogin", c.RateLimit(okHandler)))
	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest("POST", "/api/v1/auth/passkey/login/verify", nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		last = httptest.NewRecorder()
		h.ServeHTTP(last, r)
		if i == 0 && (last.Header().Get("RateLimit-Policy") == "" || last.Header().Get("RateLimit") == "") {
			t.Fatal("通った時の残量のヘッダが無い")
		}
	}
	if last.Code != http.StatusTooManyRequests || problemCode(t, last) != cgen.AUTH004 {
		t.Fatalf("login の超過が AUTH-004 でない：%d %s", last.Code, last.Body)
	}
	if last.Header().Get("Retry-After") == "" || !strings.HasPrefix(last.Header().Get("RateLimit-Policy"), `"login";q=10;w=60`) {
		t.Fatalf("超過のヘッダが違う：%v", last.Header())
	}
	if code, params := onExceed("send"); code != cgen.NET004 || params["policy"] != "send" {
		t.Fatalf("NET-004 の params.policy が違う：%s %v", code, params)
	}
}

func TestProblemShape(t *testing.T) {
	c := newTestChain(t, defaultVerifiers())
	var id uuid.UUID
	h := c.RequestId(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id = RequestIdFrom(r.Context())
		Problem(w, r, fault.New(cgen.ROOM002, nil))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	var p fault.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" || p.Status != 403 ||
		p.Type != "https://"+testHost+"/errors/ROOM-002" || p.Instance != "urn:uuid:"+id.String() {
		t.Fatalf("problem の形が違う：%s %+v", rec.Header().Get("Content-Type"), p)
	}
}

func TestRecoverReturnsSrv003(t *testing.T) {
	c := newTestChain(t, defaultVerifiers())
	h := c.RequestId(c.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret value") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != 500 || problemCode(t, rec) != cgen.SRV003 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("SRV-003 でない、または値が漏れた：%s", rec.Body)
	}
}

func TestSpaIndexNoncePerResponse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<script nonce="__FT_NONCE__"></script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newSpaIndex(Base{WwwDir: dir}, "wss://"+testHost)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		csp := rec.Header().Get("Content-Security-Policy")
		i := strings.Index(csp, "'nonce-")
		nonce := csp[i+7 : i+7+strings.Index(csp[i+7:], "'")]
		if !strings.Contains(rec.Body.String(), `nonce="`+nonce+`"`) || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("nonce が本文と CSP で違う：%s %s", csp, rec.Body)
		}
		if len(nonce) != 24 || seen[nonce] {
			t.Fatalf("nonce が 128 ビットでない、または繰り返した：%q", nonce)
		}
		seen[nonce] = true
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<script></script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSpaIndex(Base{WwwDir: dir}, "wss://x"); err == nil {
		t.Fatal("置き場の無い index.html で Fault にならない")
	}
}

// R: R-11-8-1
func TestHealthStatusTable(t *testing.T) {
	recent := testNow.Add(-time.Minute)
	stale := testNow.Add(-16 * time.Minute)
	cases := []struct {
		state         ServerState
		reason        string
		postgres, sfu bool
		probe         bool
		last          *time.Time
		status        gen.GetHealthResponseStatus
		code          int
		wantReason    string
	}{
		{StateNormal, "", true, true, true, &recent, gen.Ok, 200, ""},
		{StateNormal, "", true, false, true, &recent, gen.Degraded, 200, ""},
		{StateNormal, "", true, true, false, &recent, gen.Degraded, 200, ""},
		{StateNormal, "", true, true, true, &stale, gen.Degraded, 200, ""},
		{StateNormal, "", true, true, true, nil, gen.Degraded, 200, ""},
		{StateNormal, "", false, true, true, &recent, gen.Down, 503, "postgres"},
		{StateStopped, "", true, true, true, &recent, gen.Down, 503, "stopped"},
		{StateReadOnly, "migration", true, true, true, &recent, gen.Readonly, 503, "migration"},
		{StateInitial, "", true, true, true, &recent, gen.Setup, 200, ""},
		{StateBeforeSetup, "", true, false, false, nil, gen.Setup, 200, ""},
	}
	for i, c := range cases {
		h := &Health{base: Base{Deps: deps.Deps{Clock: clock.NewTestClock(testNow)}}, state: fakeState{s: c.state, reason: c.reason}}
		var res gen.GetHealthResponse
		res.Checks.Postgres, res.Checks.Sfu = c.postgres, c.sfu
		got, code := h.status(res, c.last, c.probe)
		reason := ""
		if got.Reason != nil {
			reason = string(*got.Reason)
		}
		if got.Status != c.status || code != c.code || reason != c.wantReason {
			t.Errorf("%d：%s %d %q、期待 %s %d %q", i, got.Status, code, reason, c.status, c.code, c.wantReason)
		}
	}
}

// R: R-2-3-2
func TestHubReplacesSecondConnectionAndClosesOnFullQueue(t *testing.T) {
	h := NewHub(Base{}, Links{})
	a := newWsConn(nil, userA, deviceA, 0)
	h.Attach(a, member(), 5)
	b := newWsConn(nil, userA, deviceA, 0)
	h.Attach(b, member(), 5)
	select {
	case <-a.closed:
	default:
		t.Fatal("同じ端末の古い接続が閉じない")
	}
	if got := h.Connected(userA); len(got) != 1 || got[0] != deviceA {
		t.Fatalf("開いている端末が違う：%v", got)
	}
	first := <-b.Send
	if !strings.Contains(string(first), `"type":"ready"`) || !strings.Contains(string(first), `"pos":5`) {
		t.Fatalf("最初のフレームが ready でない：%s", first)
	}
	for i := 0; i < SendQueueLen+1; i++ {
		b.enqueue(Frame(`{}`))
	}
	select {
	case <-b.closed:
	default:
		t.Fatal("送りのキューが満杯でも閉じない")
	}
	h.remove(b)
	if len(h.Connected(userA)) != 0 {
		t.Fatal("外した接続が残る")
	}
}

func TestHubWaitWakesOnNotifyAndShutdown(t *testing.T) {
	h := NewHub(Base{}, Links{})
	done := make(chan bool)
	go func() { done <- h.Wait(context.Background(), userA) }()
	for {
		h.mu.RLock()
		n := len(h.waiters[userA])
		h.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.notify(userA)
	if !<-done {
		t.Fatal("通知で true にならない")
	}
	go func() { done <- h.Wait(context.Background(), userA) }()
	for {
		h.mu.RLock()
		n := len(h.waiters[userA])
		h.mu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.CloseAll(time.Second)
	if <-done {
		t.Fatal("Shutdown で false にならない")
	}
}

// R: R-2-1-2
func TestCheckOrigin(t *testing.T) {
	origin := "https://" + testHost
	cases := []struct {
		header   string
		required bool
		want     bool
	}{
		{origin, true, true},
		{"", true, false},
		{"", false, true},
		{"http://" + testHost, true, false},
		{"https://" + testHost + ":8443", true, false},
		{"https://evil.example", false, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/v1/ws", nil)
		if c.header != "" {
			r.Header.Set("Origin", c.header)
		}
		if got := checkOrigin(r, origin, c.required); got != c.want {
			t.Errorf("%q required=%v：%v", c.header, c.required, got)
		}
	}
}

func TestRouteMuxRejectsUnknownPattern(t *testing.T) {
	authz, err := FromSpec(gen.Spec, fakeMembership{})
	if err != nil {
		t.Fatal(err)
	}
	m := &RouteMux{mux: http.NewServeMux(), chain: &MiddlewareChain{authz: authz}, authz: authz, skip: map[string]bool{}}
	m.HandleFunc("GET /nothing", okHandler)
	if m.err == nil {
		t.Fatal("spec に無い経路で誤りにならない")
	}
}

func TestRouteMuxMergesHeadIntoGet(t *testing.T) {
	chain := newTestChain(t, defaultVerifiers())
	m := &RouteMux{mux: http.NewServeMux(), chain: chain, authz: chain.authz, skip: map[string]bool{}}
	hit := ""
	mark := func(name string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) { hit = name }
	}
	m.HandleFunc("HEAD /api/v1/media/{id}", mark("head"))
	m.HandleFunc("GET /api/v1/media/{id}", mark("get"))
	m.HandleFunc("GET /api/v1/media/config", mark("config"))
	m.finish() // 標準の ServeMux なら HEAD と GET の 2 つのパターンがぶつかって panic する
	for req, want := range map[[2]string]string{
		{"HEAD", "/api/v1/media/" + idemKey}: "head", {"GET", "/api/v1/media/" + idemKey}: "get", {"GET", "/api/v1/media/config"}: "config",
	} {
		hit = ""
		r := httptest.NewRequest(req[0], req[1], nil)
		r.AddCookie(&http.Cookie{Name: "__Host-session", Value: "s"})
		rec := httptest.NewRecorder()
		chain.RequestId(m).ServeHTTP(rec, r)
		if hit != want {
			t.Errorf("%v：%q、期待 %q", req, hit, want)
		}
	}
}

func TestRequireFields(t *testing.T) {
	v := defaultVerifiers()
	v.State = nil
	if err := requireFields(v, "Verifiers"); err == nil || !strings.Contains(err.Error(), "fault") {
		t.Fatal("欠けた欄で Fault にならない")
	}
	var f *fault.Fault
	if !errors.As(requireFields(v, "Verifiers"), &f) {
		t.Fatal("Fault の型でない")
	}
	if err := requireFields(defaultVerifiers(), "Verifiers"); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsFormat(t *testing.T) {
	m := &Metrics{requests: map[[2]string]int64{}, durations: map[string]*histogram{}, ver: "1.2.3",
		hub: NewHub(Base{}, Links{}), upWs: &UpWsHandler{conns: map[uuid.UUID]*WsConn{}}}
	m.Observe("getMe", 200, 3*time.Millisecond)
	m.Observe("getMe", 200, 2*time.Second)
	m.Observe("", 404, time.Millisecond) // 経路の外は数えない
	out := string(m.Write(context.Background()))
	for _, want := range []string{
		`ft_http_requests_total{op="getMe",code="200"} 2`,
		`ft_http_request_duration_seconds_bucket{op="getMe",le="0.005"} 1`,
		`ft_http_request_duration_seconds_bucket{op="getMe",le="+Inf"} 2`,
		`ft_ws_connections 0`, `ft_build_info{version="1.2.3"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("無い：%s\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "# EOF\n") {
		t.Fatal("# EOF で終わらない")
	}
}
