// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	validator "github.com/pb33f/libopenapi-validator"
	verrors "github.com/pb33f/libopenapi-validator/errors"
	"golang.org/x/mod/semver"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

var _ gen.ServeMux = (*RouteMux)(nil)

// 要求の本文の上限（第2章 3。初期値）。
const (
	BodyLimitJson   = 1 << 20
	BodyLimitAppend = 64 << 20
)

// 要求の頭と経路の名（第2章 4.1、4.2、9.3、DD-2-12）。
const (
	cookieSession   = "__Host-session"
	headerRequested = "X-Requested-With"
	requestedValue  = "family-talk"
	headerApp       = "X-Family-Talk-App"
	headerIdemKey   = "Idempotency-Key"
	apiPrefix       = "/api/v1/"
	opAppendMedia   = "appendMedia"
	policyApiGlobal = "api_global"
	sessionNotice   = 7 * 24 * time.Hour
	readOnlyRetry   = 60
	maxParamErrors  = 20
)

// appHeader は X-Family-Talk-App（<platform>/<SemVer 2.0.0>）。
var appHeader = regexp.MustCompile(`^(web|android)/((0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?)$`)

// MiddlewareChain は要求にかける処理。各々は func(http.Handler) http.Handler。
// 全体の層（外から）：RequestId、Recover、FetchMetadata、AppVersion。圧縮は前段の caddy の encode。
// 経路の層（外から）：Deprecation、BodyLimit、Session、ReadOnlyGate、Authz、RateLimit、Validate、Idempotency。
// Problem は層ではなく、全部が使う書き出しの口。
type MiddlewareChain struct {
	base      Base
	authz     *AuthzTable
	verifiers Verifiers
	version   *VersionHandler
	validator validator.Validator
	origin    string
	metrics   *Metrics
}

// Global は全体の層をかける（WebSocket、SpaIndex、/metrics を含む全部の要求）。
func (c *MiddlewareChain) Global(next http.Handler) http.Handler {
	return c.RequestId(c.Recover(c.FetchMetadata(c.AppVersion(next))))
}

// ForRoute は operationId の経路の層をかける。
func (c *MiddlewareChain) ForRoute(operationId string, next http.Handler) http.Handler {
	inner := c.Deprecation(c.BodyLimit(c.Session(c.ReadOnlyGate(c.Authz(c.RateLimit(c.Validate(c.Idempotency(next))))))))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ri := reqFrom(r.Context()); ri != nil {
			ri.op = operationId
		}
		inner.ServeHTTP(w, r.WithContext(WithOperation(r.Context(), operationId)))
	})
}

// RouteMux は生成の gen.ServeMux を満たす口。HandleFunc の pattern（動詞と path）から operationId を引き、
// ForRoute で包んで持ち、finish で標準の ServeMux に登録する。引けない経路は err に残し、Build が Fault にする。
// Go の ServeMux は GET のパターンを HEAD にも当てるので、HEAD だけのパターンは他の GET とぶつかりうる
// （HEAD /media/{id} と GET /media/config）。同じ path の GET と HEAD は 1 つの GET の登録にまとめ、HEAD は HEAD の操作へ振り分ける。
type RouteMux struct {
	mux   *http.ServeMux
	chain *MiddlewareChain
	authz *AuthzTable
	// skip は登録しない pattern（server.metrics が偽の時の GET /metrics）。
	skip    map[string]bool
	pending map[string]http.Handler
	err     error
}

// HandleFunc は生成の HandlerWithOptions が経路ごとに呼ぶ。
func (m *RouteMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if m.skip[pattern] {
		return
	}
	r, ok := m.authz.RouteByPattern(pattern)
	if !ok {
		if m.err == nil {
			m.err = fmt.Errorf("spec に無い経路: %s", pattern)
		}
		return
	}
	if m.pending == nil {
		m.pending = map[string]http.Handler{}
	}
	m.pending[pattern] = m.chain.ForRoute(r.OperationId, http.HandlerFunc(handler))
}

// finish は持った経路を標準の ServeMux に登録する。HEAD は同じ path の GET にまとめる（GET が無い HEAD は GET として登録し、
// GET の要求は 405 にする）。
func (m *RouteMux) finish() {
	for pattern, h := range m.pending {
		method, path, _ := strings.Cut(pattern, " ")
		switch method {
		case http.MethodHead:
			continue
		case http.MethodGet:
			head := m.pending[http.MethodHead+" "+path]
			m.mux.Handle(pattern, headOr(head, h))
		default:
			m.mux.Handle(pattern, h)
		}
	}
	for pattern, h := range m.pending {
		method, path, _ := strings.Cut(pattern, " ")
		if method == http.MethodHead {
			if _, ok := m.pending[http.MethodGet+" "+path]; !ok {
				m.mux.Handle(http.MethodGet+" "+path, headOr(h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Allow", http.MethodHead)
					w.WriteHeader(http.StatusMethodNotAllowed)
				})))
			}
		}
	}
}

// headOr は HEAD の要求を head へ、他を get へ渡す（head が無ければ全部を get へ）。
func headOr(head, get http.Handler) http.Handler {
	if head == nil {
		return get
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			head.ServeHTTP(w, r)
			return
		}
		get.ServeHTTP(w, r)
	})
}

// ServeHTTP は標準の ServeMux へ渡す。
func (m *RouteMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

// RequestId は要求ごとに UUIDv7 を発行する（端末の X-Request-Id は使わない。第2章 9.1）。
// 終わりに Debug の水準で 1 行（operationId、状態、所要）を書く。アクセスのログは caddy が持つ。
func (c *MiddlewareChain) RequestId(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := c.base.Ids.NewV7()
		ri := &reqInfo{id: id, origin: c.origin, start: c.base.Deps.Clock.Now(),
			log: c.base.Log.With(logx.Field{Key: "req_id", Value: logx.Id(id)})}
		rec := wrapWriter(w)
		r = r.WithContext(withReq(r.Context(), ri))
		next.ServeHTTP(rec, r)
		dur := c.base.Deps.Clock.Now().Sub(ri.start)
		c.metrics.Observe(ri.op, rec.status, dur)
		ri.log.Debug("要求を終えた", logx.Field{Key: "op", Value: logx.Path(ri.op)},
			logx.Field{Key: "status", Value: logx.Count(int64(rec.status))}, logx.Field{Key: "dur", Value: logx.Duration(dur)})
	})
}

// panicValue は panic の値の代わり（値は秘密を含みうるのでログに書かない）。
type panicValue struct{}

func (panicValue) Error() string { return "panic" }

// Recover は panic を Fault にし、SRV-003 を返す。http.ErrAbortHandler は標準どおり再び panic する。
// Fault の Stack は defer の中で取るので、panic の起きた場所を含む。
func (c *MiddlewareChain) Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := wrapWriter(w)
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			f := fault.Wrap(panicValue{})
			if rec.written() {
				if ri := reqFrom(r.Context()); ri != nil {
					ri.log.Error("応答の途中の panic", logx.Field{Key: "stack", Value: logx.Trace(f.Stack)})
				}
				return
			}
			writeError(rec, r, f)
		}()
		next.ServeHTTP(rec, r)
	})
}

// FetchMetadata は /api/v1/ の状態を変える動詞で Sec-Fetch-Site と X-Requested-With を確かめる（NET-003。第2章 4.2）。
// 根の経路（/up/{token}、/internal/*）は機械の経路なので対象外。
func (c *MiddlewareChain) FetchMetadata(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiPrefix) && stateChanging(r.Method) {
			site := r.Header.Get("Sec-Fetch-Site")
			if (site != "" && site != "same-origin" && site != "none") || r.Header.Get(headerRequested) != requestedValue {
				Problem(w, r, fault.New(cgen.NET003, nil))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// stateChanging は状態を変える動詞か。
func stateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// AppVersion は X-Family-Talk-App を min_app と比べ、下回れば NET-006。形が合わない・無い時は比べない。
func (c *MiddlewareChain) AppVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiPrefix) {
			if m := appHeader.FindStringSubmatch(r.Header.Get(headerApp)); m != nil {
				if min, ok := c.version.MinApp(m[1]); ok && semver.Compare("v"+m[2], "v"+min) < 0 {
					Problem(w, r, fault.New(cgen.NET006, map[string]any{"min_app": min}))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// route は ctx の operationId の属性。経路の層の中だけで呼ぶ。
func (c *MiddlewareChain) route(r *http.Request) Route {
	rt, _ := c.authz.Route(OperationFrom(r.Context()))
	return rt
}

// Deprecation は経路の x-deprecation があれば Deprecation と Sunset を付ける（RFC 9745、8594。第2章 10）。
func (c *MiddlewareChain) Deprecation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rt := c.route(r); rt.DeprecatedAt != nil {
			w.Header().Set("Deprecation", "@"+strconv.FormatInt(rt.DeprecatedAt.Unix(), 10))
			w.Header().Set("Sunset", rt.Sunset.Format(http.TimeFormat))
		}
		next.ServeHTTP(w, r)
	})
}

// BodyLimit は本文の大きさを JSON 1 MiB、追記 64 MiB で切る（NET-009）。
// JSON の経路は上限まで読んで持ち、検証と冪等が同じ本文を読み直せるようにする。追記は読まずに上限だけ掛ける
// （超えた時の *http.MaxBytesError は writeError が NET-009 にする）。
func (c *MiddlewareChain) BodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(BodyLimitJson)
		if OperationFrom(r.Context()) == opAppendMedia {
			limit = BodyLimitAppend
		}
		if r.ContentLength > limit {
			Problem(w, r, tooLarge(limit))
			return
		}
		if limit == BodyLimitAppend {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeError(w, r, err)
			return
		}
		if int64(len(body)) > limit {
			Problem(w, r, tooLarge(limit))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		next.ServeHTTP(w, r)
	})
}

// tooLarge は NET-009。
func tooLarge(limit int64) *fault.Event {
	return fault.New(cgen.NET009, map[string]any{"limit": limit})
}

// Session は Cookie（__Host-session）か Bearer（シェル）を確かめ、Principal を ctx に入れる。
// 確かめの誤りを返すのは、経路の x-authz が全部セッションを要する値の時だけ。他（none、トークン、ws_token、
// internal を含む経路）は捨てて主体なしで続ける（/family の family_fields、WebSocket の auth、forward auth）。
func (c *MiddlewareChain) Session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var p Principal
		var ev *fault.Event
		switch {
		case cookieValue(r) != "":
			p, ev = c.verifiers.Session.Verify(ctx, cookieValue(r))
		case bearer(r) != "":
			p, ev = c.verifiers.Shell.Verify(ctx, bearer(r))
		default:
			next.ServeHTTP(w, r)
			return
		}
		if ev != nil {
			if !needsSession(c.route(r).Authz) {
				next.ServeHTTP(w, r)
				return
			}
			Problem(w, r, ev)
			return
		}
		if p.Kind == KindUser {
			exp := c.verifiers.Session.ExpiresHeader(p)
			if !exp.IsZero() && exp.Sub(c.base.Deps.Clock.Now()) <= sessionNotice {
				w.Header().Set("X-Session-Expires", exp.UTC().Format(time.RFC3339))
			}
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, p)))
	})
}

// cookieValue は __Host-session の値。
func cookieValue(r *http.Request) string {
	ck, err := r.Cookie(cookieSession)
	if err != nil {
		return ""
	}
	return ck.Value
}

// bearer は Authorization: Bearer の値。
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// needsSession は x-authz の値が全部、主体（セッションかシェル）を要するか。
func needsSession(authz []string) bool {
	if len(authz) == 0 {
		return false
	}
	for _, a := range authz {
		switch a {
		case authzSession, authzApproved, authzDeviceSelf, authzRoomMember, authzAdmin, authzShell:
		default:
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ReadOnlyGate は読み取り専用の時、GET、HEAD、x-readonly: allow の経路以外を SRV-001 で拒む（第2章 10、I-9）。
// 停止の時は全部を拒む。
func (c *MiddlewareChain) ReadOnlyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch c.verifiers.State.State() {
		case StateStopped:
			w.Header().Set("Retry-After", strconv.Itoa(readOnlyRetry))
			Problem(w, r, fault.New(cgen.SRV001, nil))
			return
		case StateReadOnly:
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !c.route(r).ReadOnlyAllow {
				w.Header().Set("Retry-After", strconv.Itoa(readOnlyRetry))
				Problem(w, r, fault.New(cgen.SRV001, map[string]any{"reason": c.verifiers.State.ReadOnlyReason()}))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Authz は AuthzTable.Check を呼ぶ。room_member と device_self の経路は path の {id} を引数にする。
func (c *MiddlewareChain) Authz(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args AuthzArgs
		if id, err := uuid.FromString(r.PathValue("id")); err == nil {
			rt := c.route(r)
			if contains(rt.Authz, authzRoomMember) {
				args.RoomId = &id
			}
			if contains(rt.Authz, authzDeviceSelf) {
				args.DeviceId = &id
			}
		}
		if ev := c.authz.Check(r.Context(), OperationFrom(r.Context()), PrincipalFrom(r.Context()), args); ev != nil {
			Problem(w, r, ev)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RateLimit は経路の x-ratelimit と、/api/v1/ の api_global を数える（第2章 9.3）。
// 拒めば policy の on_exceed の番号（NET-004 は params.policy）、Retry-After、RateLimit-Policy、RateLimit。
// 通った時は残りの最も少ない policy の 2 つのヘッダを付ける。
func (c *MiddlewareChain) RateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policies := append([]string{}, c.route(r).RateLimit...)
		if strings.HasPrefix(r.URL.Path, apiPrefix) {
			policies = append(policies, policyApiGlobal)
		}
		s := c.subjects(r)
		now := c.base.Deps.Clock.Now()
		var tight *rateResult
		for _, name := range policies {
			d := c.base.Limiter.Allow(name, s)
			res := &rateResult{name: name, d: d}
			if !d.Allowed {
				setRateHeaders(w, res, now)
				w.Header().Set("Retry-After", strconv.Itoa(ceilSeconds(d.RetryAfter)))
				code, params := onExceed(name)
				Problem(w, r, fault.New(code, params))
				return
			}
			if d.Limit > 0 && (tight == nil || d.Remaining < tight.d.Remaining) {
				tight = res
			}
		}
		if tight != nil {
			setRateHeaders(w, tight, now)
		}
		next.ServeHTTP(w, r)
	})
}

type rateResult struct {
	name string
	d    ratelimit.Decision
}

// setRateHeaders は草案の 2 つのヘッダを付ける（第2章 9.3）。t は上限が満ちるまでの秒。
func setRateHeaders(w http.ResponseWriter, res *rateResult, now time.Time) {
	w.Header().Set("RateLimit-Policy", fmt.Sprintf(`"%s";q=%d;w=%d`, res.name, res.d.Limit, res.d.Window))
	w.Header().Set("RateLimit", fmt.Sprintf(`"%s";r=%d;t=%d`, res.name, res.d.Remaining, ceilSeconds(res.d.Reset.Sub(now))))
}

// ceilSeconds は秒に切り上げる（負は 0）。
func ceilSeconds(d time.Duration) int {
	return int(math.Max(0, math.Ceil(d.Seconds())))
}

// onExceed は policy の超過の番号（ratelimits.yaml の on_exceed）。
func onExceed(name string) (string, map[string]any) {
	for _, p := range cgen.Policies {
		if p.Name == name && p.OnExceed != "" && p.OnExceed != cgen.NET004 {
			return p.OnExceed, nil
		}
	}
	return cgen.NET004, map[string]any{"policy": name}
}

// subjects は数える相手（ip、user、device、room）。connection は WebSocket の中だけ。
func (c *MiddlewareChain) subjects(r *http.Request) ratelimit.Subjects {
	s := ratelimit.Subjects{Ip: clientIp(r)}
	p := PrincipalFrom(r.Context())
	if p.UserId != nil {
		s.User = p.UserId.String()
	}
	if p.DeviceId != nil {
		s.Device = p.DeviceId.String()
	}
	if contains(c.route(r).Authz, authzRoomMember) {
		s.Room = r.PathValue("id")
	}
	return s
}

// clientIp は端末の IP。caddy は端末の送った X-Forwarded-For を捨てて作り直すので、最後の値が端末の物。
// 無ければ直接の相手（ホストの 127.0.0.1 の SFU など）。
func clientIp(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Validate は要求を spec で検証する（NET-001）。Idempotency-Key の欠けと形の誤りは NET-007 で先に返す。
// Base.ValidateResponses が真なら応答も検証し、違えば Fault（試験が赤になる）。
func (c *MiddlewareChain) Validate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptable(r.Header.Get("Accept"), c.route(r).Produces) {
			ev := fault.New(cgen.NET002, nil)
			ev.Status = http.StatusNotAcceptable
			Problem(w, r, ev)
			return
		}
		if ok, errs := c.validator.ValidateHttpRequestSync(r); !ok {
			Problem(w, r, requestErrors(errs))
			return
		}
		if r.GetBody != nil {
			r.Body, _ = r.GetBody()
		}
		if !c.base.ValidateResponses || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		buf := newBufferWriter()
		next.ServeHTTP(buf, r)
		if ok, errs := c.validator.ValidateHttpResponse(r, buf.response(r)); !ok {
			writeError(w, r, fmt.Errorf("応答が spec に合わない: %s: %s", errs[0].Message, errs[0].Reason))
			return
		}
		buf.flushTo(w)
	})
}

// acceptable は Accept が経路の応答の型のどれかを受けるか（RFC 9110 12.5.1。無ければ受ける。q=0 は断り）。
// 応答の型を持たない経路（本文の無い応答だけ）は確かめない。
func acceptable(accept string, produces []string) bool {
	if accept == "" || len(produces) == 0 {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		fields := strings.Split(part, ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		refused := false
		for _, f := range fields[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
			if strings.EqualFold(k, "q") {
				if q, err := strconv.ParseFloat(v, 64); err == nil && q == 0 {
					refused = true
				}
			}
		}
		if refused {
			continue
		}
		for _, p := range produces {
			typ, _, _ := strings.Cut(p, "/")
			if mt == "*/*" || mt == p || mt == typ+"/*" {
				return true
			}
		}
	}
	return false
}

// requestErrors は検証の誤りを NET-002、NET-007、NET-001（params.errors[]。最大 20 件）にする。
// Content-Type が経路の型に合わない（無い時を含む）のは NET-002（415）。
func requestErrors(errs []*verrors.ValidationError) *fault.Event {
	list := []map[string]any{}
	for _, e := range errs {
		if e.ValidationSubType == "contentType" {
			return fault.New(cgen.NET002, nil)
		}
		if e.ParameterName == headerIdemKey && e.ValidationSubType == "header" {
			return fault.New(cgen.NET007, nil)
		}
		if len(list) < maxParamErrors {
			list = append(list, map[string]any{"path": errorPath(e), "message": e.Reason})
		}
	}
	return fault.New(cgen.NET001, map[string]any{"errors": list})
}

// errorPath は誤りの場所（query.<名>、header.<名>、path.<名>、cookie.<名>、body と JSON Pointer）。
func errorPath(e *verrors.ValidationError) string {
	if e.ParameterName != "" {
		return e.ValidationSubType + "." + e.ParameterName
	}
	for _, f := range e.SchemaValidationErrors {
		if len(f.InstancePath) > 0 {
			return "body/" + strings.Join(f.InstancePath, "/")
		}
	}
	return "body"
}

// Idempotency は x-idempotency が key の経路で、srv-store Idempotency の Begin と Finish を同じトランザクションで行う（第2章 11）。
// 操作は ctx のトランザクション（store.WithTx）に乗る（業務は Pool.InTx で書く）。
// 応答は手元に持ち、2xx なら Finish とコミットの後に送る（コミットの失敗を成功と伝えないため）。2xx 以外は rollback して送る。
func (c *MiddlewareChain) Idempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.route(r).Idempotency != IdempotencyKey {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		p := PrincipalFrom(ctx)
		if p.UserId == nil {
			Problem(w, r, fault.New(cgen.AUTH001, nil))
			return
		}
		body, err := readBody(r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		key := r.Header.Get(headerIdemKey)
		hash := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		tx, err := c.base.Pool.Begin(ctx)
		if err != nil {
			writeError(w, r, err)
			return
		}
		defer tx.Rollback(ctx)
		stored, err := c.base.Idempotency.Begin(ctx, tx, *p.UserId, key, hash)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if stored != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(stored.Status)
			_, _ = w.Write(stored.Body)
			return
		}
		buf := newBufferWriter()
		next.ServeHTTP(buf, r.WithContext(store.WithTx(ctx, tx)))
		if buf.status < 200 || buf.status > 299 {
			tx.Rollback(ctx)
			buf.flushTo(w)
			return
		}
		if err := c.base.Idempotency.Finish(ctx, tx, *p.UserId, key, buf.status, buf.body.Bytes()); err != nil {
			writeError(w, r, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			writeError(w, r, err)
			return
		}
		buf.flushTo(w)
	})
}

// readBody は BodyLimit が持った本文を読む。
func readBody(r *http.Request) ([]byte, error) {
	if r.GetBody == nil {
		return nil, errors.New("本文を読み直せない")
	}
	rc, err := r.GetBody()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// bufferWriter は応答を手元に持つ ResponseWriter（冪等と応答の検証）。
type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferWriter() *bufferWriter {
	return &bufferWriter{header: http.Header{}}
}

// Header は持つヘッダ。
func (b *bufferWriter) Header() http.Header { return b.header }

// WriteHeader は状態を持つ。
func (b *bufferWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

// Write は本文を持つ。
func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// flushTo は持った応答を w に書く。
func (b *bufferWriter) flushTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}

// response は検証の部品に渡す http.Response。
func (b *bufferWriter) response(r *http.Request) *http.Response {
	return &http.Response{StatusCode: b.status, Header: b.header, Request: r,
		Body: io.NopCloser(bytes.NewReader(b.body.Bytes()))}
}
