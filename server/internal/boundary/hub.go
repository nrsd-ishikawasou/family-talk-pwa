// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"time"

	"github.com/coder/websocket"
	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
)

// WebSocket の接続の規則（第2章 6.3）。
const (
	SendQueueLen  = 256
	PingInterval  = 30 * time.Second
	PongTimeout   = 60 * time.Second
	AuthTimeout   = 5 * time.Second
	ReadLimit     = 64 << 10
	ShutdownGrace = 10 * time.Second
)

// Frame は WebSocket の 1 つのフレーム（JSON。第2章 6.1）。
type Frame []byte

// WsConn は 1 つの /ws の接続（記憶の上。再起動で消え、端末が再接続する）。
type WsConn struct {
	DeviceId    uuid.UUID
	UserId      uuid.UUID
	Send        chan Frame
	LastSentPos int64
	LastPong    time.Time
	conn        *websocket.Conn
	closed      chan struct{}
}

// Close は理由を付けて接続を閉じる。
func (c *WsConn) Close(reason string) {
	panic("未実装")
}

// Hub は接続の表と feed の配り手（全体実装設計書 2.3、2.4）。
type Hub struct {
	base    Base
	limiter *ratelimit.Limiter
}

// NewHub は Hub を作る。
func NewHub(base Base) *Hub {
	panic("未実装")
}

// Run は Notifier.Listen を 1 本持ち、通知を受けた利用者の接続を CatchUp する。LISTEN が戻ったら全接続を CatchUp する。
func (h *Hub) Run(ctx context.Context) error {
	panic("未実装")
}

// Attach は接続を表に入れ、ready{pos} を送る。1 端末 1 接続（2 本目は古い方を切る）。
func (h *Hub) Attach(conn *WsConn, p Principal, lastPos int64) {
	panic("未実装")
}

// Detach は端末の接続を切る（ログアウト、取り消し。B-043）。
func (h *Hub) Detach(deviceId uuid.UUID) {
	panic("未実装")
}

// Broadcast は feed に無い種類（room.typing、bye）を利用者たちの接続へ送る（B-043）。
func (h *Hub) Broadcast(userIds []uuid.UUID, kind string, payload any) {
	panic("未実装")
}

// CatchUp は Feed.ReadSince(lastSent) で読んで送り、lastSent を進める。
func (h *Hub) CatchUp(ctx context.Context, deviceId uuid.UUID) {
	panic("未実装")
}

// Connected は利用者の開いている端末（srv-push の判定。第7章 3.2。B-043）。
func (h *Hub) Connected(userId uuid.UUID) []uuid.UUID {
	panic("未実装")
}

// Wait は利用者の feed に通知が来るか、ctx が終わるまで待つ（/sync のロングポール）。通知が来れば true。
func (h *Hub) Wait(ctx context.Context, userId uuid.UUID) bool {
	panic("未実装")
}

// CloseAll は全接続に bye{retry_after} を送って閉じる（Shutdown）。
func (h *Hub) CloseAll(retryAfter time.Duration) {
	panic("未実装")
}
