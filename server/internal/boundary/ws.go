// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
)

// 端末から来るフレーム（asyncapi の wsReceive、upWsReceive）。
type inFrame struct {
	Type   string `json:"type"`
	Token  string `json:"token"`
	RoomId string `json:"room_id"`
	Typing bool   `json:"typing"`
	Id     string `json:"id"`
}

// policyWsMessages は WebSocket のメッセージの数の policy（ratelimits.yaml。connection あたり）。
const policyWsMessages = "ws_messages"

// acceptOptions は部品の Accept の確かめ。本ブロックが checkOrigin で完全一致を確かめた後なので、
// 部品には家族のサーバーの host を許させる（部品は Origin の host と要求の Host を比べ、前段が Host を変えると拒むため）。
func acceptOptions(origin string) *websocket.AcceptOptions {
	return &websocket.AcceptOptions{OriginPatterns: []string{strings.TrimPrefix(origin, "https://")}}
}

// checkOrigin は Origin が家族のサーバーのオリジンと完全に一致するか（WebSocket の部品は host だけを比べるため）。
// required が真なら Origin の無い要求も拒む（ブラウザは必ず付ける。RFC 6455 4.1）。
func checkOrigin(r *http.Request, origin string, required bool) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return !required
	}
	return o == origin
}

// WsHandler は /api/v1/ws（connectWs）を受ける。strict の生成から除いた経路。
type WsHandler struct {
	base      Base
	hub       *Hub
	verifiers Verifiers
	links     Links
	origin    string
}

// ServeHTTP は Upgrade を呼ぶ（経路の層の中に置く）。
func (h *WsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Upgrade(w, r)
}

// Upgrade は Origin を確かめ（失敗は 403）、WebSocket に切り替え、auth、ready、送り手と読み手を回す。
func (h *WsHandler) Upgrade(w http.ResponseWriter, r *http.Request) {
	if !checkOrigin(r, h.origin, true) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, acceptOptions(h.origin))
	if err != nil {
		return
	}
	c.SetReadLimit(ReadLimit)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	conn := newWsConn(c, uuid.Nil, uuid.Nil, 0)
	p, err := h.Auth(ctx, conn)
	if err != nil {
		conn.closeWith(closePolicy, "auth")
		return
	}
	conn.UserId, conn.DeviceId = *p.UserId, *p.DeviceId
	lastPos, err := h.base.Feed.LastPos(ctx, conn.UserId)
	if err != nil {
		conn.closeWith(closeInternal, "internal")
		return
	}
	conn.LastSentPos = lastPos
	conn.LastPong = h.base.Deps.Clock.Now()
	h.hub.Attach(conn, p, lastPos)
	defer h.hub.remove(conn)
	go h.writeLoop(ctx, conn)
	h.ReadLoop(ctx, conn)
	conn.closeWith(websocket.StatusNormalClosure, "")
}

// Auth は AuthTimeout 以内の {type: auth, token} を WsTokenVerifier で確かめる。
func (h *WsHandler) Auth(ctx context.Context, conn *WsConn) (Principal, error) {
	actx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()
	_, data, err := conn.conn.Read(actx)
	if err != nil {
		return Principal{}, err
	}
	var f inFrame
	if json.Unmarshal(data, &f) != nil || f.Type != "auth" || f.Token == "" {
		return Principal{}, errors.New("auth でない")
	}
	p, ev := h.verifiers.Ws.Verify(ctx, f.Token)
	if ev != nil || p.Kind != KindUser || p.UserId == nil || p.DeviceId == nil {
		return Principal{}, errors.New("auth の失敗")
	}
	return p, nil
}

// ReadLoop は端末のフレームを読む。数の超過（ws_messages）、知らない type は 1008 で閉じる。
// typing は構成員を確かめて TypingSink へ（構成員でなければ無視）。ping には pong。
func (h *WsHandler) ReadLoop(ctx context.Context, conn *WsConn) {
	s := ratelimit.Subjects{Connection: conn.DeviceId.String() + "/" + h.base.Ids.NewV7().String()}
	for {
		_, data, err := conn.conn.Read(ctx)
		if err != nil {
			return
		}
		if !h.base.Limiter.Allow(policyWsMessages, s).Allowed {
			conn.closeWith(closePolicy, "rate")
			return
		}
		var f inFrame
		if json.Unmarshal(data, &f) != nil {
			conn.closeWith(closePolicy, "frame")
			return
		}
		switch f.Type {
		case "ping":
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			conn.enqueue(pong)
		case "pong":
			conn.mu.Lock()
			conn.LastPong = h.base.Deps.Clock.Now()
			conn.mu.Unlock()
		case "typing":
			roomId, err := uuid.FromString(f.RoomId)
			if err != nil {
				conn.closeWith(closePolicy, "frame")
				return
			}
			if ok, err := h.verifiers.Membership.IsMember(ctx, conn.UserId, roomId); err == nil && ok {
				h.links.Typing.Set(conn.UserId, roomId, f.Typing, 0)
			}
		default:
			conn.closeWith(closePolicy, "frame")
			return
		}
	}
}

// writeLoop は送りの channel とキャッチアップの印を書き、PingInterval ごとに ping を送る。
// PongTimeout の間 pong が無ければ切る。
func (h *WsHandler) writeLoop(ctx context.Context, conn *WsConn) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-conn.closed:
			return
		case f := <-conn.Send:
			if writeFrame(ctx, conn.conn, f) != nil {
				conn.closeWith(closeInternal, "write")
				return
			}
		case <-conn.catchUp:
			if err := h.hub.catchUp(ctx, conn); err != nil {
				conn.closeWith(closeInternal, "internal")
				return
			}
		case <-ticker.C:
			conn.mu.Lock()
			late := h.base.Deps.Clock.Now().Sub(conn.LastPong) > PongTimeout
			conn.mu.Unlock()
			if late {
				conn.closeWith(closePolicy, "pong")
				return
			}
			ping, _ := json.Marshal(map[string]string{"type": "ping"})
			if writeFrame(ctx, conn.conn, ping) != nil {
				conn.closeWith(closeInternal, "write")
				return
			}
		}
	}
}

// writeFrame は 1 つのフレームを WriteTimeout の中で書く。
func writeFrame(ctx context.Context, c *websocket.Conn, f []byte) error {
	wctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, f)
}

// UpWsHandler は /api/v1/up/ws（connectUpWs。配布元の常時接続）を受ける（B-045）。
type UpWsHandler struct {
	base      Base
	verifiers Verifiers
	links     Links
	origin    string
	mu        sync.Mutex
	conns     map[uuid.UUID]*WsConn
}

// ServeHTTP は Upgrade を呼ぶ（経路の層の中に置く）。
func (h *UpWsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Upgrade(w, r)
}

// Upgrade はシェルのトークン（Session の middleware が Bearer を確かめた主体）で受け、未配信を古い順に送り、ack を受ける。
// Origin は無い要求を許し、ある時は WsHandler と同じ一致を求める。1 端末 1 本（2 本目は古い方を 4002）。
func (h *UpWsHandler) Upgrade(w http.ResponseWriter, r *http.Request) {
	if !checkOrigin(r, h.origin, false) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	p := PrincipalFrom(r.Context())
	if p.Kind != KindShell || p.DeviceId == nil {
		Problem(w, r, fault.New(cgen.AUTH001, nil))
		return
	}
	c, err := websocket.Accept(w, r, acceptOptions(h.origin))
	if err != nil {
		return
	}
	c.SetReadLimit(ReadLimit)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	conn := newWsConn(c, uuid.Nil, *p.DeviceId, 0)
	conn.LastPong = h.base.Deps.Clock.Now()
	h.mu.Lock()
	if old, ok := h.conns[conn.DeviceId]; ok {
		old.closeWith(closeReplaced, "replaced")
	}
	h.conns[conn.DeviceId] = conn
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.conns[conn.DeviceId] == conn {
			delete(h.conns, conn.DeviceId)
		}
		h.mu.Unlock()
	}()
	pending, err := h.links.Up.Pending(ctx, conn.DeviceId)
	if err != nil {
		conn.closeWith(closeInternal, "internal")
		return
	}
	// 未配信は 1 端末 1,000 件まであり得る（PUSH-003）。送りの channel（256）を通さず、順に直接書く。
	for _, m := range pending {
		if writeFrame(ctx, conn.conn, upFrame(m)) != nil {
			conn.closeWith(closeInternal, "write")
			return
		}
	}
	go h.writeLoop(ctx, conn)
	h.readLoop(ctx, conn)
	conn.closeWith(websocket.StatusNormalClosure, "")
}

// upFrame は msg のフレーム（asyncapi の upMsg。body は base64url）。
func upFrame(m UpMessage) Frame {
	b, _ := json.Marshal(map[string]any{"type": "msg", "id": m.Id,
		"body": base64.RawURLEncoding.EncodeToString(m.Body), "expires_at": m.ExpiresAt.UTC().Format(time.RFC3339)})
	return b
}

// readLoop は ack、ping、pong を読む。
func (h *UpWsHandler) readLoop(ctx context.Context, conn *WsConn) {
	for {
		_, data, err := conn.conn.Read(ctx)
		if err != nil {
			return
		}
		var f inFrame
		if json.Unmarshal(data, &f) != nil {
			conn.closeWith(closePolicy, "frame")
			return
		}
		switch f.Type {
		case "ack":
			id, err := uuid.FromString(f.Id)
			if err != nil {
				conn.closeWith(closePolicy, "frame")
				return
			}
			if err := h.links.Up.Ack(ctx, conn.DeviceId, id); err != nil {
				conn.closeWith(closeInternal, "internal")
				return
			}
		case "ping":
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			conn.enqueue(pong)
		case "pong":
			conn.mu.Lock()
			conn.LastPong = h.base.Deps.Clock.Now()
			conn.mu.Unlock()
		default:
			conn.closeWith(closePolicy, "frame")
			return
		}
	}
}

// upPingInterval は配布元の常時接続の ping の間隔（第2章 6.4）。
const upPingInterval = 60 * time.Second

// writeLoop は送りの channel を書き、upPingInterval ごとに ping を送る。2 回分 pong が無ければ切る。
func (h *UpWsHandler) writeLoop(ctx context.Context, conn *WsConn) {
	ticker := time.NewTicker(upPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-conn.closed:
			return
		case f := <-conn.Send:
			if writeFrame(ctx, conn.conn, f) != nil {
				conn.closeWith(closeInternal, "write")
				return
			}
		case <-ticker.C:
			conn.mu.Lock()
			late := h.base.Deps.Clock.Now().Sub(conn.LastPong) > 2*upPingInterval
			conn.mu.Unlock()
			if late {
				conn.closeWith(closePolicy, "pong")
				return
			}
			ping, _ := json.Marshal(map[string]string{"type": "ping"})
			if writeFrame(ctx, conn.conn, ping) != nil {
				conn.closeWith(closeInternal, "write")
				return
			}
		}
	}
}

// Deliver は端末の接続があれば送りの channel に入れて true。無ければ false（メールボックスに残る）。
func (h *UpWsHandler) Deliver(deviceId uuid.UUID, msg UpMessage) bool {
	h.mu.Lock()
	c := h.conns[deviceId]
	h.mu.Unlock()
	if c == nil {
		return false
	}
	select {
	case <-c.closed:
		return false
	default:
	}
	c.enqueue(upFrame(msg))
	return true
}

// closeAll は全接続を 1012 で閉じる（Shutdown）。
func (h *UpWsHandler) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		c.closeWith(closeRestart, "restart")
	}
}
