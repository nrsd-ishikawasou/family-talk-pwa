// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// WebSocket の接続の規則（第2章 6.3、第1章 10.6）。
const (
	SendQueueLen  = 256
	PingInterval  = 30 * time.Second
	PongTimeout   = 60 * time.Second
	AuthTimeout   = 5 * time.Second
	WriteTimeout  = 10 * time.Second
	ReadLimit     = 64 << 10
	ShutdownGrace = 10 * time.Second
	catchUpBatch  = 100
)

// 閉じる状態の番号（第2章 6.3。IANA の登録と私用の範囲）。
const (
	closePolicy   websocket.StatusCode = 1008
	closeInternal websocket.StatusCode = 1011
	closeRestart  websocket.StatusCode = 1012
	closeTryLater websocket.StatusCode = 1013
	closeRevoked  websocket.StatusCode = 4001
	closeReplaced websocket.StatusCode = 4002
	kindRoomEvent                      = "room.event"
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
	catchUp     chan struct{}
	closeOnce   sync.Once
	closed      chan struct{}
	mu          sync.Mutex
}

// newWsConn は接続を作る。
func newWsConn(c *websocket.Conn, userId, deviceId uuid.UUID, lastPos int64) *WsConn {
	return &WsConn{DeviceId: deviceId, UserId: userId, Send: make(chan Frame, SendQueueLen), LastSentPos: lastPos,
		conn: c, catchUp: make(chan struct{}, 1), closed: make(chan struct{})}
}

// Close は理由を付けて接続を閉じる（理由は短い英語。端末は状態の番号で振る舞いを決める）。
func (c *WsConn) Close(reason string) {
	c.closeWith(websocket.StatusNormalClosure, reason)
}

// closeWith は状態の番号を付けて 1 回だけ閉じる。
func (c *WsConn) closeWith(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.conn != nil {
			_ = c.conn.Close(code, reason)
		}
	})
}

// enqueue は送りの channel に入れる。満杯なら 1013 で閉じる（端末は再接続して /sync で埋める）。
func (c *WsConn) enqueue(f Frame) {
	select {
	case c.Send <- f:
	case <-c.closed:
	default:
		c.closeWith(closeTryLater, "send queue full")
	}
}

// requestCatchUp はキャッチアップの印を立てる（書き手が 1 つずつ行う）。
func (c *WsConn) requestCatchUp() {
	select {
	case c.catchUp <- struct{}{}:
	default:
	}
}

// Hub は接続の表と feed の配り手（全体実装設計書 2.3、2.4）。
type Hub struct {
	base    Base
	links   Links
	mu      sync.RWMutex
	conns   map[uuid.UUID]*WsConn
	byUser  map[uuid.UUID]map[uuid.UUID]bool
	waiters map[uuid.UUID][]chan struct{}
	closing chan struct{}
	once    sync.Once
}

// NewHub は Hub を作る。
func NewHub(base Base, links Links) *Hub {
	return &Hub{base: base, links: links, conns: map[uuid.UUID]*WsConn{}, byUser: map[uuid.UUID]map[uuid.UUID]bool{},
		waiters: map[uuid.UUID][]chan struct{}{}, closing: make(chan struct{})}
}

// Run は Notifier.Listen を 1 本持ち、通知を受けた利用者の接続を CatchUp する。
// uuid.Nil（LISTEN の戻り、取りこぼしの合図）は全接続を CatchUp し、全部の待ちを起こす。
// LISTEN を張り終えた直後にも同じことをする（受け付けは LISTEN より先に始まりうるので、その間の NOTIFY は届いていない）。
func (h *Hub) Run(ctx context.Context) error {
	ch, err := h.base.Notifier.Listen(ctx)
	if err != nil {
		return err
	}
	h.notify(uuid.Nil)
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-ch:
			if !ok {
				return nil
			}
			h.notify(u)
		}
	}
}

// notify は利用者（uuid.Nil は全員）の接続にキャッチアップの印を立て、待ちを起こす。
func (h *Hub) notify(userId uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	wake := func(u uuid.UUID) {
		for _, w := range h.waiters[u] {
			close(w)
		}
		delete(h.waiters, u)
	}
	if userId == uuid.Nil {
		for _, c := range h.conns {
			c.requestCatchUp()
		}
		for u := range h.waiters {
			wake(u)
		}
		return
	}
	for d := range h.byUser[userId] {
		h.conns[d].requestCatchUp()
	}
	wake(userId)
}

// Attach は接続を表に入れ、ready{pos} を送る。1 端末 1 接続（2 本目は古い方を 4002 で閉じる）。
func (h *Hub) Attach(conn *WsConn, p Principal, lastPos int64) {
	h.mu.Lock()
	if old, ok := h.conns[conn.DeviceId]; ok {
		old.closeWith(closeReplaced, "replaced")
	}
	h.conns[conn.DeviceId] = conn
	if h.byUser[conn.UserId] == nil {
		h.byUser[conn.UserId] = map[uuid.UUID]bool{}
	}
	h.byUser[conn.UserId][conn.DeviceId] = true
	h.mu.Unlock()
	ready, _ := json.Marshal(map[string]any{"type": "ready", "pos": lastPos})
	conn.enqueue(ready)
	conn.requestCatchUp()
}

// remove は接続を表から外す（その接続が今の物の時だけ）。
func (h *Hub) remove(conn *WsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[conn.DeviceId] != conn {
		return
	}
	delete(h.conns, conn.DeviceId)
	if set := h.byUser[conn.UserId]; set != nil {
		delete(set, conn.DeviceId)
		if len(set) == 0 {
			delete(h.byUser, conn.UserId)
		}
	}
}

// Detach は端末の接続を 4001 で切る（ログアウト、取り消し、停止。B-043）。
func (h *Hub) Detach(deviceId uuid.UUID) {
	h.mu.RLock()
	c := h.conns[deviceId]
	h.mu.RUnlock()
	if c != nil {
		c.closeWith(closeRevoked, "revoked")
	}
}

// Broadcast は feed に無い種類（room.typing、bye）を利用者たちの接続へ送る（B-043）。
// payload はフレームの全体（type を含む）。
func (h *Hub) Broadcast(userIds []uuid.UUID, kind string, payload any) {
	frame, err := json.Marshal(payload)
	if err != nil {
		h.base.Log.Error("配る物を JSON にできない", logx.Field{Key: "kind", Value: logx.Path(kind)})
		return
	}
	h.mu.RLock()
	var targets []*WsConn
	for _, u := range userIds {
		for d := range h.byUser[u] {
			targets = append(targets, h.conns[d])
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(frame)
	}
}

// CatchUp は Feed.ReadSince(lastSent) を空になるまで読んでフレームにして送り、lastSent を進める。
// 書き手の goroutine から呼ぶ（同じ接続で同時に 2 つ走らない）。
func (h *Hub) CatchUp(ctx context.Context, deviceId uuid.UUID) {
	h.mu.RLock()
	c := h.conns[deviceId]
	h.mu.RUnlock()
	if c != nil {
		_ = h.catchUp(ctx, c)
	}
}

// catchUp は 1 つの接続のキャッチアップ。
func (h *Hub) catchUp(ctx context.Context, c *WsConn) error {
	for {
		rows, err := h.base.Feed.ReadSince(ctx, c.UserId, c.LastSentPos, catchUpBatch)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		frames, err := h.frames(ctx, c.UserId, c.DeviceId, rows)
		if err != nil {
			return err
		}
		for _, f := range frames {
			c.enqueue(Frame(f))
		}
		c.LastSentPos = int64(rows[len(rows)-1].Pos)
		if len(rows) < catchUpBatch {
			return nil
		}
	}
}

// frames は feed の行をフレーム（第2章 6.1：pos、id、type、ts、room_id、payload）にする。
// room.event は EventSource から、その利用者と端末に見える形で引く。返らない物は送らない。
func (h *Hub) frames(ctx context.Context, userId, deviceId uuid.UUID, rows []store.FeedRow) ([]json.RawMessage, error) {
	var ids []uuid.UUID
	for _, r := range rows {
		if r.Ref.Kind == kindRoomEvent {
			ids = append(ids, r.Ref.Id)
		}
	}
	events := map[uuid.UUID]json.RawMessage{}
	if len(ids) > 0 {
		var err error
		if events, err = h.links.Events.ForFeed(ctx, userId, deviceId, ids); err != nil {
			return nil, err
		}
	}
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		payload := r.Ref.Payload
		if r.Ref.Kind == kindRoomEvent {
			ev, ok := events[r.Ref.Id]
			if !ok {
				continue
			}
			payload = ev
		}
		f := map[string]any{"pos": r.Pos, "id": r.Ref.Id, "type": r.Ref.Kind,
			"ts": r.Ref.Ts.UTC().Format(time.RFC3339Nano), "payload": payload}
		if r.Ref.RoomId != uuid.Nil {
			f["room_id"] = r.Ref.RoomId
		}
		b, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// Connected は利用者の開いている端末（srv-push の判定。第7章 3.2。B-043）。
func (h *Hub) Connected(userId uuid.UUID) []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(h.byUser[userId]))
	for d := range h.byUser[userId] {
		out = append(out, d)
	}
	return out
}

// Wait は利用者の feed に通知が来るか、ctx が終わるか、Shutdown が始まるまで待つ（/sync のロングポール）。
// 通知が来れば true。
func (h *Hub) Wait(ctx context.Context, userId uuid.UUID) bool {
	ch := make(chan struct{})
	h.mu.Lock()
	h.waiters[userId] = append(h.waiters[userId], ch)
	h.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
	case <-h.closing:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.waiters[userId]
	for i, w := range list {
		if w == ch {
			h.waiters[userId] = append(list[:i], list[i+1:]...)
			break
		}
	}
	return false
}

// CloseAll は全接続に bye{retry_after} を送って 1012 で閉じ、ロングポールの待ちを解く（Shutdown）。
func (h *Hub) CloseAll(retryAfter time.Duration) {
	h.once.Do(func() { close(h.closing) })
	bye, _ := json.Marshal(map[string]any{"type": "bye", "retry_after": int(retryAfter.Seconds())})
	h.mu.RLock()
	conns := make([]*WsConn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		c.enqueue(bye)
	}
	for _, c := range conns {
		go func(c *WsConn) {
			select {
			case <-c.closed:
			case <-time.After(time.Second):
			}
			c.closeWith(closeRestart, "restart")
		}(c)
	}
}
