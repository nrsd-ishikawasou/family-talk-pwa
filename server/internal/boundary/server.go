// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"net/http"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/deps"
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
}

// Server は api のプロセスの HTTP の口（B-040。全体実装設計書 2.1）。
type Server struct {
	base     Base
	hub      *Hub
	ws       *WsHandler
	upWs     *UpWsHandler
	httpSrv  *http.Server
	handler  http.Handler
	authz    *AuthzTable
	version  *VersionHandler
	health   *Health
	metrics  *Metrics
	spa      *SpaIndex
	internal *InternalAuth
	sync     *SyncHandler
	routes   *RouteMux
	chain    *MiddlewareChain
}

// NewServer は Server を作る。
func NewServer() *Server {
	panic("未実装")
}

// Build は生成した経路と本ブロックの経路を束ね、middleware を順にかける。
// impls の Sync、Server、Internal は本ブロックの実装で埋める（渡した値は使わない）。
// x-authz の欠けなど起動を止める誤りは Fault（I-12）。
func (s *Server) Build(base Base, impls gen.Implementations, verifiers Verifiers, links Links) (http.Handler, error) {
	panic("未実装")
}

// ListenAndServe は addr（:8080）で受ける。Shutdown の後は http.ErrServerClosed。
func (s *Server) ListenAndServe(addr string) error {
	panic("未実装")
}

// Shutdown は全接続に bye{retry_after} を送り、最大 10 秒待って閉じる。
func (s *Server) Shutdown(ctx context.Context) error {
	panic("未実装")
}

// Hub は Build で作った hub（srv-push、srv-rooms、srv-identity、srv-admin が B-043 で使う）。
func (s *Server) Hub() *Hub {
	panic("未実装")
}

// UpWs は Build で作った /up/ws の器（srv-push が Deliver を呼ぶ。B-045）。
func (s *Server) UpWs() *UpWsHandler {
	panic("未実装")
}
