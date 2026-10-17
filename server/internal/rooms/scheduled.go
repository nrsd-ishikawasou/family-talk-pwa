// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// ScheduledState は予約送信の状態（scheduled_events.state。api の Scheduled.state と同じ）。
type ScheduledState string

// 状態の 3 つ。
const (
	ScheduledPending   ScheduledState = "pending"
	ScheduledSent      ScheduledState = "sent"
	ScheduledCancelled ScheduledState = "cancelled"
)

// ScheduledView は予約送信の線上の形（api の Scheduled）。
type ScheduledView = gen.Scheduled

// ScheduledEvent は予約送信の 1 行（scheduled_events）。
type ScheduledEvent struct {
	Id       uuid.UUID
	RoomId   uuid.UUID
	UserId   uuid.UUID
	DeviceId uuid.UUID
	SendAt   time.Time
	Body     gen.FtEventMls1
	State    ScheduledState
	JobId    *uuid.UUID
}

// Scheduled は予約送信（B-106。第4章 5.8）。送るのは Timeline.PostScheduled（srv-worker）。
type Scheduled struct {
	d        *Deps
	ports    *ports
	timeline *Timeline
	members  *Members
}

// scheduledCols は scheduled_events の読みの列（scanScheduled の順）。
const scheduledCols = `id, room_id, user_id, device_id, send_at, body, state, job_id`

func scanScheduled(row pgx.Row) (ScheduledEvent, error) {
	var s ScheduledEvent
	var body []byte
	var state string
	if err := row.Scan(&s.Id, &s.RoomId, &s.UserId, &s.DeviceId, &s.SendAt, &body, &state, &s.JobId); err != nil {
		return ScheduledEvent{}, err
	}
	s.State = ScheduledState(state)
	return s, json.Unmarshal(body, &s.Body)
}

// view は線の形（api の Scheduled。body はそのまま）。
func (s ScheduledEvent) view() ScheduledView {
	return ScheduledView{Id: s.Id.String(), SendAt: s.SendAt.UTC(), Body: s.Body, State: gen.ScheduledState(s.State)}
}

// Create は POST /rooms/{id}/scheduled（第4章 5.8、第12章 5.7）。1. RoomLock（ROOM-001）、本人が active でなければ ROOM-002。
// 2. 過去か 30 日（room.schedule_horizon）より先は ROOM-010。大きさ 256 KiB、暗号文の形、group_id がこのトークの物（20 バイトで先頭
// 16 バイトがトークの識別子）でなければ ROOM-005（epoch と世代は見ない：送る時に古くてよい）。3. relates_to は予約の時に確かめる
// （checkRelation。送る時には確かめない）。4. 添付があれば MediaBinder.BindScheduled（MEDIA-002、MEDIA-005。予約の時に誤りを返す）。
// 5. Jobs.Enqueue(scheduled_send、run_at = sendAt、重複の鍵 scheduled_send:<予約の識別子>)、行（device_id は要求の端末）、
// room.scheduled{pending} を本人へ。
func (s *Scheduled) Create(ctx context.Context, tx *store.Tx, p boundary.Principal, roomId uuid.UUID, sendAt time.Time, body gen.FtEventMls1) (ScheduledView, error) {
	me, device, err := self(p)
	if err != nil {
		return ScheduledView{}, err
	}
	if _, err := s.d.lockRoom(ctx, tx, roomId); err != nil {
		return ScheduledView{}, err
	}
	st, err := s.members.stateOf(ctx, tx, roomId, me, false)
	if err != nil {
		return ScheduledView{}, err
	}
	if st != MemberActive {
		return ScheduledView{}, ev(cgen.ROOM002)
	}
	now := s.d.now()
	if !sendAt.After(now) || sendAt.After(now.Add(s.d.cfgDur("room.schedule_horizon"))) {
		return ScheduledView{}, ev(cgen.ROOM010)
	}
	if err := checkSize(body); err != nil {
		return ScheduledView{}, err
	}
	if _, _, _, err := mlsParts(body); err != nil {
		return ScheduledView{}, err
	}
	gid, err := base64.RawURLEncoding.DecodeString(body.GroupId)
	if err != nil || len(gid) != 20 || !bytes.Equal(gid[:16], roomId.Bytes()) {
		return ScheduledView{}, ev(cgen.ROOM005)
	}
	if err := s.checkRelation(ctx, tx, roomId, me, body, now); err != nil {
		return ScheduledView{}, err
	}
	id := s.d.Ids.NewV7()
	ids, err := attachmentIds(body)
	if err != nil {
		return ScheduledView{}, err
	}
	if len(ids) > 0 {
		if err := s.ports.media.BindScheduled(ctx, tx, me, roomId, id, ids); err != nil {
			return ScheduledView{}, err
		}
	}
	job, err := s.d.Jobs.Enqueue(ctx, tx, "scheduled_send", map[string]string{"scheduled_id": id.String()}, sendAt, scheduledJobKey(id))
	if err != nil {
		return ScheduledView{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ScheduledView{}, wrap(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO scheduled_events (id, room_id, user_id, device_id, send_at, body, state, job_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8)`, id, roomId, me, device, sendAt.UTC(), raw, job, now); err != nil {
		return ScheduledView{}, wrap(err)
	}
	row := ScheduledEvent{Id: id, RoomId: roomId, UserId: me, DeviceId: device, SendAt: sendAt.UTC(), Body: body, State: ScheduledPending, JobId: &job}
	if _, err := s.d.emit(tx, []uuid.UUID{me}, kindRoomScheduled, roomId, s.d.Ids.NewV7(), roomScheduled{ScheduledId: id, State: ScheduledPending}); err != nil {
		return ScheduledView{}, err
	}
	return row.view(), nil
}

// checkRelation は予約送信の relates_to の確かめ（第4章 5.8）。付けられる関係は返信（reply。thread も同じ）だけで、編集（replace）と
// リアクション・票・出欠（annotation）は ROOM-013（編集の 15 分の期限は予約の時刻へ持ち越せない）。返信は Timeline.checkRelation と同じ
// （対象が別のトークなら ROOM-012、無ければ受け付ける）。
func (s *Scheduled) checkRelation(ctx context.Context, q store.Querier, roomId, me uuid.UUID, body gen.FtEventMls1, now time.Time) error {
	if body.RelatesTo == nil {
		return nil
	}
	switch body.RelatesTo.RelType {
	case gen.FtEventMls1RelatesToRelTypeReplace, gen.FtEventMls1RelatesToRelTypeAnnotation:
		return ev(cgen.ROOM013)
	}
	return s.timeline.checkRelation(ctx, q, roomId, me, body, now)
}

// scheduledJobKey は予約送信の仕事の重複の鍵（取り消しは同じ鍵で消す）。
func scheduledJobKey(id uuid.UUID) string {
	return "scheduled_send:" + id.String()
}

// Cancel は DELETE /rooms/{id}/scheduled/{sid}（規則 own_scheduled：他人の物と無い物は ROOM-007。在ることを漏らさない）。RoomLock
// （ROOM-001。錠の順は rooms が先：トークの削除が RoomLock の後に媒体の参照と予約の行を取るので、予約の行を先に取ると逆向きに待ち合う）。
// pending でなければ何もしない。cancelled、Jobs.Cancel、MediaBinder.UnbindScheduled（参照が 0 なら 7 日の猶予の後に消える。I-5）、
// room.scheduled{cancelled} を本人へ。
func (s *Scheduled) Cancel(ctx context.Context, tx *store.Tx, p boundary.Principal, roomId, id uuid.UUID) error {
	me, _, err := self(p)
	if err != nil {
		return err
	}
	if _, err := s.d.lockRoom(ctx, tx, roomId); err != nil {
		return err
	}
	row, err := scanScheduled(tx.QueryRow(ctx, `SELECT `+scheduledCols+` FROM scheduled_events WHERE id = $1 AND room_id = $2 AND user_id = $3 FOR UPDATE`,
		id, roomId, me))
	if noRows(err) {
		return ev(cgen.ROOM007)
	}
	if err != nil {
		return wrap(err)
	}
	if row.State != ScheduledPending {
		return nil
	}
	if _, err := s.d.Jobs.Cancel(ctx, tx, scheduledJobKey(id)); err != nil {
		return err
	}
	if err := s.ports.media.UnbindScheduled(ctx, tx, id); err != nil {
		return err
	}
	return s.timeline.setScheduled(ctx, tx, row, ScheduledCancelled)
}

// cancelAllOf はトークの削除（Rooms.drop の 4）：pending の予約送信を全部 cancelled にし、仕事を消し、room.scheduled を本人へ。添付の
// 結び付きは drop の MediaBinder.UnbindRoom で外れている。
func (s *Scheduled) cancelAllOf(ctx context.Context, tx *store.Tx, roomId uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT `+scheduledCols+` FROM scheduled_events WHERE room_id = $1 AND state = 'pending' ORDER BY id FOR UPDATE`, roomId)
	if err != nil {
		return wrap(err)
	}
	var list []ScheduledEvent
	for rows.Next() {
		r, err := scanScheduled(rows)
		if err != nil {
			rows.Close()
			return wrap(err)
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrap(err)
	}
	for _, r := range list {
		if _, err := s.d.Jobs.Cancel(ctx, tx, scheduledJobKey(r.Id)); err != nil {
			return err
		}
		if err := s.timeline.setScheduled(ctx, tx, r, ScheduledCancelled); err != nil {
			return err
		}
	}
	return nil
}

// List は GET /rooms/{id}/scheduled（本人の分だけ。3 つの状態。予約の識別子の cursor）。戻りは (要素, has_more)。
func (s *Scheduled) List(ctx context.Context, p boundary.Principal, roomId uuid.UUID, c Cursor) ([]ScheduledView, bool, error) {
	me, _, err := self(p)
	if err != nil {
		return nil, false, err
	}
	sql, args := whereUUID(c, `SELECT `+scheduledCols+` FROM scheduled_events WHERE room_id = $1 AND user_id = $2`, `id`, []any{roomId, me})
	rows, err := s.d.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, wrap(err)
	}
	defer rows.Close()
	var out []ScheduledView
	for rows.Next() {
		r, err := scanScheduled(rows)
		if err != nil {
			return nil, false, wrap(err)
		}
		out = append(out, r.view())
	}
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	items, more := finish(out, c)
	return items, more, nil
}
