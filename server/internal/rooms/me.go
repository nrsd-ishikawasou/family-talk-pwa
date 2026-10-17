// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// RoomMe は自分の設定の 1 行（room_me。ft.settings.room_me/1）。端末から消した位置は端末が持つ（第4章 4.2）。
type RoomMe struct {
	MuteUntil  *time.Time
	Pinned     bool
	Favorite   bool
	Hidden     bool
	Background *string
}

// RoomMeStore は自分の設定（B-102）。
type RoomMeStore struct {
	d *Deps
}

// toGen は線の形（ft.settings.room_me/1。全部の欄を出す。background は空なら出さない）。
func (m RoomMe) toGen() gen.FtSettingsRoomMe1 {
	p, f, h := m.Pinned, m.Favorite, m.Hidden
	out := gen.FtSettingsRoomMe1{Pinned: &p, Favorite: &f, Hidden: &h, Background: m.Background}
	if m.MuteUntil != nil {
		s := m.MuteUntil.UTC().Format(time.RFC3339)
		out.MuteUntil = &s
	}
	return out
}

// defaults は行が無い時の値（config.yaml の room_me.*。mute_until の null はミュートしない）。
func (s *RoomMeStore) defaults() RoomMe {
	c := s.d.Config
	me := RoomMe{Pinned: c.Get("room_me.pinned").Bool(), Favorite: c.Get("room_me.favorite").Bool(), Hidden: c.Get("room_me.hidden").Bool()}
	if v := c.Get("room_me.mute_until"); !v.IsNull() {
		if t, err := time.Parse(time.RFC3339, v.String()); err == nil {
			me.MuteUntil = &t
		}
	}
	if b := c.Get("room_me.background").String(); b != "" {
		me.Background = &b
	}
	return me
}

// Get は自分の設定（行が無ければ config.yaml の room_me.* の既定値）。srv-push の mute_until の判定にも使う。
func (s *RoomMeStore) Get(ctx context.Context, q store.Querier, userId, roomId uuid.UUID) (RoomMe, error) {
	m, err := s.getMany(ctx, q, userId, []uuid.UUID{roomId})
	return m[roomId], err
}

// getMany は複数のトークの自分の設定を 1 つの文で読む（無いトークは既定値）。
func (s *RoomMeStore) getMany(ctx context.Context, q store.Querier, userId uuid.UUID, roomIds []uuid.UUID) (map[uuid.UUID]RoomMe, error) {
	out := make(map[uuid.UUID]RoomMe, len(roomIds))
	def := s.defaults()
	for _, id := range roomIds {
		out[id] = def
	}
	if len(roomIds) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT room_id, mute_until, pinned, favorite, hidden, background FROM room_me WHERE user_id = $1 AND room_id = ANY($2)`,
		userId, roomIds)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var m RoomMe
		if err := rows.Scan(&id, &m.MuteUntil, &m.Pinned, &m.Favorite, &m.Hidden, &m.Background); err != nil {
			return nil, wrap(err)
		}
		if m.Background != nil && *m.Background == "" {
			m.Background = nil
		}
		out[id] = m
	}
	return out, wrap(rows.Err())
}

// Put は PUT /rooms/{id}/me：ft.settings.room_me/1 の全部の欄で置き換え（無い欄は既定値）、room.me を本人の全端末へ。戻りは置いた値。
func (s *RoomMeStore) Put(ctx context.Context, tx *store.Tx, userId, roomId uuid.UUID, me RoomMe) (RoomMe, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO room_me (user_id, room_id, mute_until, pinned, favorite, hidden, background) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, room_id) DO UPDATE SET mute_until = EXCLUDED.mute_until, pinned = EXCLUDED.pinned, favorite = EXCLUDED.favorite,
  hidden = EXCLUDED.hidden, background = EXCLUDED.background`, userId, roomId, me.MuteUntil, me.Pinned, me.Favorite, me.Hidden, me.Background); err != nil {
		return RoomMe{}, wrap(err)
	}
	_, err := s.d.emit(tx, []uuid.UUID{userId}, kindRoomMe, roomId, s.d.Ids.NewV7(), roomMe{Settings: me.toGen()})
	return me, err
}

// fromGen は要求の形を RoomMe にする（無い欄は既定値。mute_until が RFC 3339 でなければ NET-001、path body/mute_until）。
func (s *RoomMeStore) fromGen(in gen.FtSettingsRoomMe1) (RoomMe, error) {
	me := s.defaults()
	if in.MuteUntil != nil {
		t, err := time.Parse(time.RFC3339, *in.MuteUntil)
		if err != nil {
			return RoomMe{}, netError("body/mute_until", "RFC 3339 でない")
		}
		me.MuteUntil = &t
	} else {
		me.MuteUntil = nil
	}
	if in.Pinned != nil {
		me.Pinned = *in.Pinned
	}
	if in.Favorite != nil {
		me.Favorite = *in.Favorite
	}
	if in.Hidden != nil {
		me.Hidden = *in.Hidden
	}
	me.Background = in.Background
	if me.Background != nil && *me.Background == "" {
		me.Background = nil
	}
	return me, nil
}
