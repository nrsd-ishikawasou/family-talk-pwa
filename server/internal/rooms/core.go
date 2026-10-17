// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Cursor は一覧の位置（第2章 3：before か after の要素の識別子と件数。結果は常に古い順）。
type Cursor struct {
	Before *uuid.UUID
	After  *uuid.UUID
	Limit  int
}

// RoomView はトークの一覧の 1 件（api の RoomSummary。rooms.last_event_at は応答に含めない）。
type RoomView = gen.RoomSummary

// AdminRoom は管理者のトークの一覧の 1 件（api の AdminRoom。容量は srv-admin が srv-media から足す）。
type AdminRoom = gen.AdminRoom

// wrap は DB と部品の誤りを Fault にする。既に Event か Fault ならそのまま返す。
func wrap(err error) error {
	if err == nil {
		return nil
	}
	var e *fault.Event
	var f *fault.Fault
	if errors.As(err, &e) || errors.As(err, &f) {
		return err
	}
	return fault.Wrap(err)
}

// noRows は 1 行の読みで行が無いこと。
func noRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// ev は番号の Event（params なし）。
func ev(code string) *fault.Event {
	return fault.New(code, nil)
}

// now は記録の時刻（Deps.Clock）。
func (d *Deps) now() time.Time {
	return d.Clock.Now()
}

// emit は 1 件のイベントを宛先の構成員ごとの feed に同じトランザクションで溜める（全体実装設計書 2.4）。宛先の重複を除き識別子の
// 順に並べる。payload は room.event では nil（srv-boundary が Timeline.ForFeed で中身を引く）。戻りは宛先ごとの溜めた行
// （Timeline.Post が Flush の後に本人の pos を読む）。
func (d *Deps) emit(tx *store.Tx, users []uuid.UUID, kind string, room uuid.UUID, id uuid.UUID, payload any) (map[uuid.UUID]*store.FeedPending, error) {
	var body json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, wrap(err)
		}
		body = b
	}
	ts := d.now()
	out := map[uuid.UUID]*store.FeedPending{}
	for _, u := range sortedIds(users) {
		out[u] = d.Feed.Append(tx, u, store.FeedRef{Kind: kind, Id: id, RoomId: room, Payload: body, Ts: ts})
	}
	return out, nil
}

// sortedIds は識別子の重複を除き、バイトの順（UUID の文字列の順と同じ）に並べた写し。
func sortedIds(ids []uuid.UUID) []uuid.UUID {
	list := slices.Clone(ids)
	slices.SortFunc(list, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(list)
}

// isEvent は番号 code の Event か。
func isEvent(err error, code string) bool {
	var e *fault.Event
	return errors.As(err, &e) && e.Code == code
}

// netError は NET-001（params.errors の 1 件。path は要求の位置。第2章 9.2）。
func netError(path, message string) error {
	return fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": path, "message": message}}})
}

// inTx は ctx のトランザクション（冪等の middleware）があればそれに乗り、無ければ開いて fn を走らせる。
func (d *Deps) inTx(ctx context.Context, fn func(tx *store.Tx) error) error {
	return d.Pool.InTx(ctx, fn)
}

// cfgDur は設定の時間（config.yaml は秒）。
func (d *Deps) cfgDur(key string) time.Duration {
	return d.Config.Get(key).Duration()
}

// self は主体の構成員と端末（承認の後の要求だけが本ブロックに来るので、無ければ AUTH-001）。
func self(p boundary.Principal) (uuid.UUID, uuid.UUID, error) {
	if p.UserId == nil || p.DeviceId == nil {
		return uuid.Nil, uuid.Nil, ev(cgen.AUTH001)
	}
	return *p.UserId, *p.DeviceId, nil
}

// parseId は要求の識別子（形は srv-boundary の検証が済ませている。読めなければ NET-001）。
func parseId(s, path string) (uuid.UUID, error) {
	id, err := uuid.FromString(s)
	if err != nil {
		return uuid.Nil, netError(path, "UUID でない")
	}
	return id, nil
}

// parseIds は識別子の並び（path は位置の前置き。i 番目は path/i）。
func parseIds(list []string, path string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, len(list))
	for i, s := range list {
		id, err := parseId(s, path+"/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

// optId は任意の識別子の写し。
func optId(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// idStr は識別子の文字列の並び。
func idStr(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// newCursor は要求の引数から一覧の位置を作る（limit が無ければ 50。範囲は境界の検証）。
func newCursor(limit *int, before, after *string) (Cursor, error) {
	c := Cursor{Limit: pageDefault}
	if limit != nil {
		c.Limit = *limit
	}
	for _, pair := range []struct {
		s    *string
		dst  **uuid.UUID
		path string
	}{{before, &c.Before, "query.before"}, {after, &c.After, "query.after"}} {
		if pair.s == nil {
			continue
		}
		id, err := parseId(*pair.s, pair.path)
		if err != nil {
			return Cursor{}, err
		}
		*pair.dst = &id
	}
	return c, nil
}

// ascending は after から進む向きか。無い時は最新の側から古い側へ読み、返す前に古い順に並べ直す（第2章 3）。
func (c Cursor) ascending() bool {
	return c.After != nil
}

// whereUUID は識別子の列 col に cursor の条件と並びと件数（limit + 1）を足す。
func whereUUID(c Cursor, sql, col string, args []any) (string, []any) {
	if c.After != nil {
		args = append(args, *c.After)
		sql += " AND " + col + " > $" + strconv.Itoa(len(args))
	}
	if c.Before != nil {
		args = append(args, *c.Before)
		sql += " AND " + col + " < $" + strconv.Itoa(len(args))
	}
	order := " DESC"
	if c.ascending() {
		order = " ASC"
	}
	args = append(args, c.Limit+1)
	return sql + " ORDER BY " + col + order + " LIMIT $" + strconv.Itoa(len(args)), args
}

// finish は limit + 1 件まで読んだ結果を limit に切り、has_more と古い順の並びにする。
func finish[T any](items []T, c Cursor) ([]T, bool) {
	more := len(items) > c.Limit
	if more {
		items = items[:c.Limit]
	}
	if !c.ascending() {
		slices.Reverse(items)
	}
	if items == nil {
		items = []T{}
	}
	return items, more
}

// lockRoom はトークの錠（srv-store RoomLock）の後に行を読む。無いか削除済みなら ROOM-001。
func (d *Deps) lockRoom(ctx context.Context, tx *store.Tx, roomId uuid.UUID) (Room, error) {
	r, ok, err := d.lockRoomAny(ctx, tx, roomId)
	if err != nil {
		return Room{}, err
	}
	if !ok || r.State == RoomDeleted {
		return Room{}, ev(cgen.ROOM001)
	}
	return r, nil
}

// lockRoomAny はトークの錠の後に行を読む（削除済みも返す）。行が無ければ偽。裏の処理（一括の取消、予約送信、掃除）が使う。
func (d *Deps) lockRoomAny(ctx context.Context, tx *store.Tx, roomId uuid.UUID) (Room, bool, error) {
	if err := d.RoomLock.Lock(ctx, tx, roomId); err != nil {
		if isEvent(err, cgen.ROOM001) {
			return Room{}, false, nil
		}
		return Room{}, false, err
	}
	r, err := scanRoom(tx.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms WHERE id = $1`, roomId))
	if err != nil {
		return Room{}, false, wrap(err)
	}
	return r, true, nil
}

// roomCols は rooms の読みの列（scanRoom の順）。
const roomCols = `id, kind, creator_id, state, empty_since, last_event_at, created_at, deleted_at`

func scanRoom(row pgx.Row) (Room, error) {
	var r Room
	var kind, state string
	err := row.Scan(&r.Id, &kind, &r.Creator, &state, &r.EmptySince, &r.LastEventAt, &r.CreatedAt, &r.DeletedAt)
	r.Kind, r.State = RoomKind(kind), RoomState(state)
	return r, err
}

// eventCols は events の読みの列（scanEvent の順）。
const eventCols = `id, room_id, ts, schema, sender_id, sender_device_id, epoch, ciphertext, plain, redacted_at, redacted_by, redact_reason,
transaction_id, silent, scheduled_at`

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	var schema string
	var plain []byte
	err := row.Scan(&e.Id, &e.RoomId, &e.Ts, &schema, &e.Sender, &e.SenderDevice, &e.Epoch, &e.Ciphertext, &plain, &e.RedactedAt,
		&e.RedactedBy, &e.RedactReason, &e.TransactionId, &e.Silent, &e.ScheduledAt)
	e.Schema = EventSchema(schema)
	if plain != nil {
		e.Plain = json.RawMessage(plain)
	}
	return e, err
}

// scanEvents は複数のイベントの行。
func scanEvents(ctx context.Context, q store.Querier, sql string, args ...any) ([]Event, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, wrap(err)
		}
		out = append(out, e)
	}
	return out, wrap(rows.Err())
}

// insertEvent はイベントの行を書く（不変。I-2）。
func insertEvent(ctx context.Context, tx *store.Tx, e Event) error {
	var plain any
	if e.Plain != nil {
		plain = []byte(e.Plain)
	}
	_, err := tx.Exec(ctx, `INSERT INTO events (id, room_id, ts, schema, sender_id, sender_device_id, epoch, ciphertext, plain, transaction_id, silent,
  scheduled_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		e.Id, e.RoomId, e.Ts, string(e.Schema), e.Sender, e.SenderDevice, e.Epoch, e.Ciphertext, plain, e.TransactionId, e.Silent, e.ScheduledAt)
	return wrap(err)
}

// mlsParts は ft.event.mls/1 を行の欄に分ける：暗号文のバイト、epoch、平文の欄（本体から ciphertext と epoch を除いた物）。
// 暗号文が base64url（詰め物なし）でなければ ROOM-005。
func mlsParts(body gen.FtEventMls1) ([]byte, int64, json.RawMessage, error) {
	ct, err := base64.RawURLEncoding.DecodeString(body.Ciphertext)
	if err != nil {
		return nil, 0, nil, ev(cgen.ROOM005)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, nil, wrap(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, 0, nil, wrap(err)
	}
	delete(m, "ciphertext")
	delete(m, "epoch")
	plain, err := json.Marshal(m)
	if err != nil {
		return nil, 0, nil, wrap(err)
	}
	return ct, int64(body.Epoch), plain, nil
}

// skeleton はイベントの行を線の形（ft.room_event/1）にする。transaction_id は deviceId が送った端末の時だけ。削除済みの印は body を
// 付けず redacted だけ（取り消した人の無い行は gc のトークの削除だけで、そのトークに構成員がいないので返る経路が無い。返せばプログラムの誤り）。
func skeleton(e Event, deviceId uuid.UUID) (EventSkeleton, error) {
	out := EventSkeleton{Id: e.Id.String(), RoomId: e.RoomId.String(), Ts: e.Ts.UTC(), Schema: gen.FtRoomEvent1Schema(e.Schema),
		SenderId: optId(e.Sender), SenderDeviceId: optId(e.SenderDevice)}
	if e.Silent {
		t := true
		out.Silent = &t
	}
	if e.TransactionId != nil && e.SenderDevice != nil && deviceId != uuid.Nil && *e.SenderDevice == deviceId {
		out.TransactionId = optId(e.TransactionId)
	}
	if e.ScheduledAt != nil {
		at := e.ScheduledAt.UTC()
		out.ScheduledAt = &at
	}
	if e.RedactedAt != nil {
		if e.RedactedBy == nil {
			return EventSkeleton{}, fault.Wrap(errors.New("rooms: 取り消した人の無い削除済みの印を返そうとした"))
		}
		out.Redacted = &struct {
			At     gen.CommonRfc33391 `json:"at"`
			By     gen.CommonUuidv71  `json:"by"`
			Reason *string            `json:"reason,omitempty"`
		}{At: e.RedactedAt.UTC(), By: e.RedactedBy.String(), Reason: e.RedactReason}
		return out, nil
	}
	raw := []byte(e.Plain)
	if e.Schema == SchemaMls {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e.Plain, &m); err != nil {
			return EventSkeleton{}, wrap(err)
		}
		epoch, _ := json.Marshal(e.Epoch)
		ct, _ := json.Marshal(base64.RawURLEncoding.EncodeToString(e.Ciphertext))
		m["epoch"], m["ciphertext"] = epoch, ct
		b, err := json.Marshal(m)
		if err != nil {
			return EventSkeleton{}, wrap(err)
		}
		raw = b
	}
	var body gen.FtRoomEvent1_Body
	if err := body.UnmarshalJSON(raw); err != nil {
		return EventSkeleton{}, wrap(err)
	}
	out.Body = &body
	return out, nil
}

// memberIds は構成員の行の識別子。
func memberIds(ms []Member) []uuid.UUID {
	out := make([]uuid.UUID, len(ms))
	for i, m := range ms {
		out[i] = m.UserId
	}
	return out
}

// without は ids から x を除いた写し。
func without(ids []uuid.UUID, x uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id != x {
			out = append(out, id)
		}
	}
	return out
}
