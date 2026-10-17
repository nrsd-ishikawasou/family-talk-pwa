// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"

	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Receipt は 1 人の既読（公開の既読、私的な既読、読み終えた位置。第4章 5.4）。
type Receipt struct {
	UserId      uuid.UUID
	Read        *uuid.UUID
	ReadPrivate *uuid.UUID
	FullyRead   *uuid.UUID
}

// Receipts は既読（B-104）。
type Receipts struct {
	d       *Deps
	members *Members
}

// mark は既読の位置（イベントの ts と識別子。(ts, id) の順で比べる）。
type mark struct {
	id uuid.UUID
	ts time.Time
}

// after は m が cur より新しいか（cur が無ければ真）。
func (m mark) after(cur *mark) bool {
	if cur == nil {
		return true
	}
	if !m.ts.Equal(cur.ts) {
		return m.ts.After(cur.ts)
	}
	return string(m.id[:]) > string(cur.id[:])
}

// Put は POST /rooms/{id}/receipts：3 種を単調に進める（第4章 5.4）。与えられた識別子ごとにこのトークのイベントを読み（無ければ
// ROOM-007）、receipts の行の錠（無ければ空の行を入れて錠）で同時の 2 つを直列にし、種類ごとに (ts, id) が今より新しい時だけ進める
// （後退と同じ値は無視して 200）。read が進めば room.receipt{user_id, read} を active の構成員へ、read_private か fully_read が進めば
// 今の 3 つの値を本人へ。進んだ物が無ければ書かない。
func (r *Receipts) Put(ctx context.Context, tx *store.Tx, userId, roomId uuid.UUID, read, readPrivate, fullyRead *uuid.UUID) error {
	want := [3]*mark{}
	for i, id := range []*uuid.UUID{read, readPrivate, fullyRead} {
		if id == nil {
			continue
		}
		var ts time.Time
		err := tx.QueryRow(ctx, `SELECT ts FROM events WHERE id = $1 AND room_id = $2`, *id, roomId).Scan(&ts)
		if noRows(err) {
			return ev(cgen.ROOM007)
		}
		if err != nil {
			return wrap(err)
		}
		want[i] = &mark{id: *id, ts: ts}
	}
	now := r.d.now()
	if _, err := tx.Exec(ctx, `INSERT INTO receipts (user_id, room_id, updated_at) VALUES ($1, $2, $3) ON CONFLICT (user_id, room_id) DO NOTHING`,
		userId, roomId, now); err != nil {
		return wrap(err)
	}
	var cur [3]*mark
	var ids [3]*uuid.UUID
	var tss [3]*time.Time
	if err := tx.QueryRow(ctx, `SELECT r.read_event_id, a.ts, r.read_private_event_id, b.ts, r.fully_read_event_id, c.ts FROM receipts r
LEFT JOIN events a ON a.id = r.read_event_id LEFT JOIN events b ON b.id = r.read_private_event_id LEFT JOIN events c ON c.id = r.fully_read_event_id
WHERE r.user_id = $1 AND r.room_id = $2 FOR UPDATE OF r`, userId, roomId).Scan(&ids[0], &tss[0], &ids[1], &tss[1], &ids[2], &tss[2]); err != nil {
		return wrap(err)
	}
	for i := range cur {
		if ids[i] != nil && tss[i] != nil {
			cur[i] = &mark{id: *ids[i], ts: *tss[i]}
		}
	}
	moved := [3]bool{}
	for i := range want {
		if want[i] != nil && want[i].after(cur[i]) {
			cur[i], moved[i] = want[i], true
		}
	}
	if !moved[0] && !moved[1] && !moved[2] {
		return nil
	}
	val := func(m *mark) *uuid.UUID {
		if m == nil {
			return nil
		}
		id := m.id
		return &id
	}
	if _, err := tx.Exec(ctx, `UPDATE receipts SET read_event_id = $3, read_private_event_id = $4, fully_read_event_id = $5, updated_at = $6
WHERE user_id = $1 AND room_id = $2`, userId, roomId, val(cur[0]), val(cur[1]), val(cur[2]), now); err != nil {
		return wrap(err)
	}
	if moved[0] {
		active, err := r.members.Of(ctx, tx, roomId)
		if err != nil {
			return err
		}
		if _, err := r.d.emit(tx, memberIds(active), kindRoomReceipt, roomId, r.d.Ids.NewV7(), roomReceipt{UserId: userId, Read: val(cur[0])}); err != nil {
			return err
		}
	}
	if moved[1] || moved[2] {
		p := roomReceipt{UserId: userId, Read: val(cur[0]), ReadPrivate: val(cur[1]), FullyRead: val(cur[2])}
		if _, err := r.d.emit(tx, []uuid.UUID{userId}, kindRoomReceipt, roomId, r.d.Ids.NewV7(), p); err != nil {
			return err
		}
	}
	return nil
}

// Of は GET /rooms/{id}/receipts：active の構成員の行だけ（外れた人を出さない）。公開の既読は全員、私的な既読と読み終えた位置は
// userId の分だけ。
func (r *Receipts) Of(ctx context.Context, userId, roomId uuid.UUID) ([]Receipt, error) {
	rows, err := r.d.Pool.Query(ctx, `SELECT r.user_id, r.read_event_id, r.read_private_event_id, r.fully_read_event_id FROM receipts r
JOIN room_members m ON m.room_id = r.room_id AND m.user_id = r.user_id AND m.state = 'active' WHERE r.room_id = $1 ORDER BY r.user_id`, roomId)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	out := []Receipt{}
	for rows.Next() {
		var x Receipt
		if err := rows.Scan(&x.UserId, &x.Read, &x.ReadPrivate, &x.FullyRead); err != nil {
			return nil, wrap(err)
		}
		if x.UserId != userId {
			x.ReadPrivate, x.FullyRead = nil, nil
		}
		out = append(out, x)
	}
	return out, wrap(rows.Err())
}

// UnreadCount は未読の数（ページのアルゴリズムの表。第4章 4.2）。q は ctx のトランザクションか Pool。本人が active でなければ 0。
func (r *Receipts) UnreadCount(ctx context.Context, q store.Querier, userId, roomId uuid.UUID) (int, error) {
	row, ok, err := r.members.row(ctx, q, roomId, userId, false)
	if err != nil || !ok || row.State != MemberActive {
		return 0, err
	}
	n, err := r.unreadMany(ctx, q, userId, []memberRow{{room: Room{Id: roomId}, state: MemberActive, since: row.Since}})
	return n[roomId], err
}

// unreadMany は複数のトークの未読の数を 1 つの文で数える（トークごとの LATERAL。索引 events_room_ts）。数えるのは ft.event.mls/1 で、
// 削除済みの印、annotation と replace、自分の送信を除き、本人が active になった時刻（room_members.since）より後で、fully_read の
// (ts, id) より後（fully_read が無ければ後ろの条件を付けない）。追加待ちのトークは 0（地図に入れない）。
func (r *Receipts) unreadMany(ctx context.Context, q store.Querier, userId uuid.UUID, rows []memberRow) (map[uuid.UUID]int, error) {
	out := map[uuid.UUID]int{}
	var rooms []uuid.UUID
	var since []time.Time
	for _, m := range rows {
		if m.state == MemberActive {
			rooms, since = append(rooms, m.room.Id), append(since, m.since)
		}
	}
	if len(rooms) == 0 {
		return out, nil
	}
	res, err := q.Query(ctx, `SELECT x.room_id, (SELECT count(*) FROM events e WHERE e.room_id = x.room_id AND e.schema = 'ft.event.mls/1'
  AND e.redacted_at IS NULL AND e.sender_id <> $1 AND coalesce(e.plain->'relates_to'->>'rel_type', '') NOT IN ('annotation', 'replace')
  AND e.ts > x.since AND (fr.id IS NULL OR (e.ts, e.id) > (fr.ts, fr.id)))
FROM unnest($2::uuid[], $3::timestamptz[]) AS x(room_id, since)
LEFT JOIN receipts rc ON rc.user_id = $1 AND rc.room_id = x.room_id
LEFT JOIN events fr ON fr.id = rc.fully_read_event_id`, userId, rooms, since)
	if err != nil {
		return nil, wrap(err)
	}
	defer res.Close()
	for res.Next() {
		var id uuid.UUID
		var n int
		if err := res.Scan(&id, &n); err != nil {
			return nil, wrap(err)
		}
		out[id] = n
	}
	return out, wrap(res.Err())
}
