// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/deps"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Base は下のブロックの部品（cmd/ft が作って渡す）。
type Base struct {
	Config      *config.Config
	Deps        deps.Deps
	Log         *logx.Logger
	Limiter     *ratelimit.Limiter
	Ids         *ids.Ids
	Pool        *store.Pool
	Feed        *store.Feed
	Notifier    *store.Notifier
	Idempotency *store.Idempotency
	Jobs        *store.Jobs
	// WwwDir は index.html と dl/version.json の置き場（イメージの中の /srv/www）。
	WwwDir string
	// ValidateResponses は応答を spec で検証する（試験と開発だけ真。運用の設定ではない）。
	ValidateResponses bool
}

// spaRoutes は画面の入口（ページの SpaIndex の注記。openapi には載せない）。Go の ServeMux のパターン。
var spaRoutes = []string{
	"GET /{$}", "GET /r/", "GET /d/", "GET /i/", "GET /settings/", "GET /welcome/", "GET /login",
	"GET /recover/", "GET /admin/", "GET /calls", "GET /call/", "GET /albums/", "GET /stickers/", "GET /new",
}

// metricsPattern は /metrics の経路（server.metrics が偽なら登録しない）。
const metricsPattern = "GET /metrics"

// Server は api のプロセスの HTTP の口（B-040。全体実装設計書 2.1）。
type Server struct {
	base    Base
	hub     *Hub
	upWs    *UpWsHandler
	handler http.Handler
	mu      sync.Mutex
	httpSrv *http.Server
	cancel  context.CancelFunc
}

// NewServer は Server を作る。
func NewServer() *Server {
	return &Server{}
}

// Build は生成した経路と本ブロックの経路を束ね、middleware をかけた handler を返す。
// impls の Sync、Server、Internal は本ブロックの実装で埋める（渡した値は使わない）。他の欄の欠けは Fault。
// x-authz の欠けなど起動を止める誤りは Fault（I-12）。
func (s *Server) Build(base Base, impls gen.Implementations, verifiers Verifiers, links Links) (http.Handler, error) {
	if err := requireFields(base, "Base"); err != nil {
		return nil, err
	}
	if err := requireFields(verifiers, "Verifiers"); err != nil {
		return nil, err
	}
	if err := requireFields(links, "Links"); err != nil {
		return nil, err
	}
	authz, err := FromSpec(gen.Spec, verifiers.Membership)
	if err != nil {
		return nil, err
	}
	version, err := newVersionHandler(base, authz)
	if err != nil {
		return nil, err
	}
	origin, wsOrigin := origins(base.Config)
	doc, err := libopenapi.NewDocument(gen.Spec)
	if err != nil {
		return nil, fault.Wrap(err)
	}
	val, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		return nil, fault.Wrap(errors.Join(errs...))
	}
	hub := NewHub(base, links)
	upWs := &UpWsHandler{base: base, verifiers: verifiers, links: links, origin: origin, conns: map[uuid.UUID]*WsConn{}}
	metrics := newMetrics(base, hub, upWs, version.Get().Server)
	chain := &MiddlewareChain{base: base, authz: authz, verifiers: verifiers, version: version, validator: val,
		origin: origin, metrics: metrics}
	spa, err := newSpaIndex(base, wsOrigin)
	if err != nil {
		return nil, err
	}
	impls.Sync = &SyncHandler{base: base, hub: hub, links: links}
	impls.Server = &serverApi{health: &Health{base: base, links: links, state: verifiers.State, client: &http.Client{},
		sfuURL: sfuHealthURL}, version: version, metrics: metrics}
	impls.Internal = &InternalAuth{verifiers: verifiers}
	if err := requireFields(impls, "Implementations"); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	rm := &RouteMux{mux: mux, chain: chain, authz: authz, skip: map[string]bool{}}
	if !base.Config.Get(cgen.KeyServerMetrics).Bool() {
		rm.skip[metricsPattern] = true
	}
	strict := gen.NewStrictHandlerWithOptions(impls, nil, gen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  bindError,
		ResponseErrorHandlerFunc: writeError,
	})
	gen.HandlerWithOptions(strict, gen.StdHTTPServerOptions{BaseRouter: rm, ErrorHandlerFunc: bindError})
	if rm.err != nil {
		return nil, fault.Wrap(rm.err)
	}
	rm.finish()
	mux.Handle("GET /api/v1/ws", chain.ForRoute("connectWs", &WsHandler{base: base, hub: hub, verifiers: verifiers,
		links: links, origin: origin}))
	mux.Handle("GET /api/v1/up/ws", chain.ForRoute("connectUpWs", upWs))
	for _, p := range spaRoutes {
		mux.Handle(p, spa)
	}

	s.base, s.hub, s.upWs = base, hub, upWs
	s.handler = chain.Global(mux)
	return s.handler, nil
}

// bindError は生成のコードの引数・本文の取り込みの誤り（検証を通った後なので、まず起きない）を NET-001 にする。
func bindError(w http.ResponseWriter, r *http.Request, err error) {
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		Problem(w, r, tooLarge(big.Limit))
		return
	}
	Problem(w, r, fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": "request", "message": "bind"}}}))
}

// requireFields は構造体の interface と pointer の欄が全部入っていることを確かめる（欠けは Fault。どの欄かを名に入れる）。
func requireFields(v any, name string) error {
	rv := reflect.ValueOf(v)
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if (f.Kind() == reflect.Interface || f.Kind() == reflect.Pointer) && f.IsNil() {
			return fault.Wrap(fmt.Errorf("%s.%s が無い", name, rv.Type().Field(i).Name))
		}
	}
	return nil
}

// origins は家族のサーバーのオリジン（https://名前、443 でなければ :ポート）と WebSocket の宛先（wss://）。
func origins(c *config.Config) (string, string) {
	host := c.Domain
	if c.HttpsPort != 0 && c.HttpsPort != 443 {
		host += ":" + strconv.Itoa(c.HttpsPort)
	}
	return "https://" + host, "wss://" + host
}

// ListenAndServe は addr（:8080）で受け、hub を回す。Shutdown の後は http.ErrServerClosed。
func (s *Server) ListenAndServe(addr string) error {
	if s.handler == nil {
		return fault.Wrap(errors.New("Build の前に ListenAndServe を呼んだ"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.httpSrv = &http.Server{Addr: addr, Handler: s.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	s.cancel = cancel
	srv := s.httpSrv
	s.mu.Unlock()
	go func() {
		if err := s.hub.Run(ctx); err != nil {
			s.base.Log.Error("hub が止まった", logx.Field{Key: "cause", Value: logx.Trace(fault.Wrap(err).Error())})
		}
	}()
	return srv.ListenAndServe()
}

// Shutdown は全接続に bye{retry_after} を送って閉じ、ロングポールの待ちを解き、最大 10 秒待って閉じる。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv, cancel := s.httpSrv, s.cancel
	s.mu.Unlock()
	if s.hub != nil {
		s.hub.CloseAll(ShutdownGrace)
	}
	if s.upWs != nil {
		s.upWs.closeAll()
	}
	if cancel != nil {
		defer cancel()
	}
	if srv == nil {
		return nil
	}
	sctx, scancel := context.WithTimeout(ctx, ShutdownGrace)
	defer scancel()
	return srv.Shutdown(sctx)
}

// Hub は Build で作った hub（srv-push、srv-rooms、srv-identity、srv-admin が B-043 で使う）。
func (s *Server) Hub() *Hub {
	return s.hub
}

// UpWs は Build で作った /up/ws の器（srv-push が Deliver を呼ぶ。B-045）。
func (s *Server) UpWs() *UpWsHandler {
	return s.upWs
}
