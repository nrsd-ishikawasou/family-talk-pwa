// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"net/http"

	"github.com/gofrs/uuid/v5"
)

// WsHandler は /api/v1/ws（connectWs）を受ける。strict の生成から除いた経路。
type WsHandler struct {
	base      Base
	hub       *Hub
	authz     *AuthzTable
	verifiers Verifiers
	links     Links
}

// Upgrade は Origin を家族のサーバーのオリジンと完全に一致で確かめ（無い要求も拒む。失敗は 403）、WebSocket に切り替える。
func (h *WsHandler) Upgrade(w http.ResponseWriter, r *http.Request) {
	panic("未実装")
}

// Auth は 5 秒以内の {type: auth, token} を WsTokenVerifier で確かめる。失敗で切る。
func (h *WsHandler) Auth(ctx context.Context, conn *WsConn) (Principal, error) {
	panic("未実装")
}

// ReadLoop は端末のフレームを読む。typing は構成員を確かめて TypingSink へ。ping と pong。100 件/分を超えれば切る。
func (h *WsHandler) ReadLoop(ctx context.Context, conn *WsConn) {
	panic("未実装")
}

// UpWsHandler は /api/v1/up/ws（connectUpWs。配布元の常時接続）を受ける（B-045）。
type UpWsHandler struct {
	base      Base
	authz     *AuthzTable
	verifiers Verifiers
	links     Links
}

// Upgrade はシェルのトークン（Authorization: Bearer）を確かめ、未配信を古い順に送り、ack を受ける。ping は 60 秒。
// Origin は無い要求を許し、ある時は WsHandler と同じ一致を求める。
func (h *UpWsHandler) Upgrade(w http.ResponseWriter, r *http.Request) {
	panic("未実装")
}

// Deliver は端末の接続があれば送りの channel に入れて true。無ければ false（メールボックスに残る）。
func (h *UpWsHandler) Deliver(deviceId uuid.UUID, msg UpMessage) bool {
	panic("未実装")
}
