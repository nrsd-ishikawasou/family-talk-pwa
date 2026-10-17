// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
)

// Typing は入力中（B-105。第4章 5.4）。記憶の上だけで持ち、feed に書かない（room.typing は入力中の送り先で流す）。
type Typing struct {
	d       *Deps
	ports   *ports
	members *Members
	mu      sync.Mutex
	// rooms はトークごとの入力中の人（gen は期限の取り消しの印：同じ人が打ち直せば前の期限は外さない）。
	rooms map[uuid.UUID]map[uuid.UUID]typingEntry
}

type typingEntry struct {
	gen   int
	until time.Time
}

// typingReadTimeout は宛先の読みの期限（要求の ctx の外で走るので Background に付ける）。
const typingReadTimeout = 5 * time.Second

// Set は PUT /rooms/{id}/typing と WebSocket の typing（srv-boundary TypingSink）。構成員の確かめは呼ぶ側が先に済ませる。timeout が
// 0 なら config の room.typing_timeout（10 秒。範囲 5〜30）。変えるたびにそのトークの入力中の人の全部を
// {type: room.typing, room_id, payload: {user_ids, timeout}}（pos、id、ts は無い）で active の構成員へ送る。期限は Deps.Clock.After で
// 外して送り直す。
func (t *Typing) Set(userId, roomId uuid.UUID, typing bool, timeout time.Duration) {
	if timeout <= 0 {
		timeout = t.d.cfgDur("room.typing_timeout")
	}
	t.mu.Lock()
	if t.rooms == nil {
		t.rooms = map[uuid.UUID]map[uuid.UUID]typingEntry{}
	}
	room := t.rooms[roomId]
	if room == nil {
		room = map[uuid.UUID]typingEntry{}
		t.rooms[roomId] = room
	}
	if typing {
		e := typingEntry{gen: room[userId].gen + 1, until: t.d.now().Add(timeout)}
		room[userId] = e
		go func() {
			<-t.d.Clock.After(timeout)
			t.mu.Lock()
			cur, ok := t.rooms[roomId][userId]
			if !ok || cur.gen != e.gen {
				t.mu.Unlock()
				return
			}
			delete(t.rooms[roomId], userId)
			t.mu.Unlock()
			t.send(roomId, timeout)
		}()
	} else {
		delete(room, userId)
	}
	t.mu.Unlock()
	t.send(roomId, timeout)
}

// send はトークの入力中の人の全部を active の構成員へ送る（宛先が読めなければ送らずにログ）。
func (t *Typing) send(roomId uuid.UUID, timeout time.Duration) {
	t.mu.Lock()
	var users []uuid.UUID
	for u := range t.rooms[roomId] {
		users = append(users, u)
	}
	if len(t.rooms[roomId]) == 0 {
		delete(t.rooms, roomId)
	}
	t.mu.Unlock()
	slices.SortFunc(users, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if users == nil {
		users = []uuid.UUID{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), typingReadTimeout)
	defer cancel()
	active, err := t.members.Of(ctx, t.d.Pool, roomId)
	if err != nil {
		slog.Warn("入力中の宛先を読めない", "room_id", roomId.String(), "err", err)
		return
	}
	frame := map[string]any{"type": kindRoomTyping, "room_id": roomId.String(),
		"payload": roomTyping{UserIds: users, Timeout: int(timeout / time.Second)}}
	t.ports.broadcast(memberIds(active), kindRoomTyping, frame)
}
