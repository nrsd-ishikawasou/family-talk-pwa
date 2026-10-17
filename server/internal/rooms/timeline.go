// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// EventSchema はイベントの種類（events.schema）。
type EventSchema string

// 種類の 4 つ（第2章 12）。
const (
	SchemaMls       EventSchema = "ft.event.mls/1"
	SchemaSystem    EventSchema = "ft.event.system/1"
	SchemaCall      EventSchema = "ft.event.call/1"
	SchemaRedaction EventSchema = "ft.event.redaction/1"
)

// Event はイベントの 1 行（events。不変。取消は削除済みの印。I-2）。pos は利用者ごと（feed）で、本体には持たない。
type Event struct {
	Id           uuid.UUID
	RoomId       uuid.UUID
	Ts           time.Time
	Schema       EventSchema
	Sender       *uuid.UUID
	SenderDevice *uuid.UUID
	Epoch        *int64
	// Ciphertext は ft.event.mls/1 の暗号文（削除済みの印で nil）。
	Ciphertext []byte
	// Plain は system・call・redaction の中身と、mls の平文の欄（mentions、relates_to、attachments、schema_inner、expires_at）。
	Plain         json.RawMessage
	RedactedAt    *time.Time
	RedactedBy    *uuid.UUID
	RedactReason  *string
	TransactionId *uuid.UUID
	Silent        bool
	// ScheduledAt は予約送信から来たイベントだけが持つ予約の時刻（ts は送った時刻。第4章 5.8）。
	ScheduledAt *time.Time
}

// Posted は送信の応答（本人の pos。Feed.Flush が入れた値）。TransactionId は要求の Idempotency-Key（送った端末のキューの
// transaction id。予約送信は無し）。
type Posted struct {
	EventId       uuid.UUID
	Pos           int64
	Ts            time.Time
	TransactionId *uuid.UUID
}

// EventSkeleton はイベントの線上の形（ft.room_event/1。削除済みの印は body が無く redacted がある）。
type EventSkeleton = gen.FtRoomEvent1

// SyncInitial は /sync の初回の一覧（snapshot と pos を 1 つの見え方で読む。第2章 5.9）。
type SyncInitial struct {
	Snapshot gen.SyncSnapshot
	Pos      int64
}

// Timeline はイベントの記録と読み（B-103）。srv-boundary の EventSource と Snapshotter の中身。
type Timeline struct {
	d       *Deps
	ports   *ports
	members *Members
	rooms   *Rooms
}

// redactionBatch は一括の取消の 1 回の束（I-16。MediaBinder.UnbindEvents の 1 回と FanoutRedactions の 1 回）。
const redactionBatch = 1000

// Post は POST /rooms/{id}/events。次の順に行う（第4章 5.1、第12章 5.2）。1. RoomLock。2. 構成員か（ROOM-002）。3. 大きさ 256 KiB
// と暗号文の形（ROOM-005）。4. srv-keys Groups.CheckSender（同じトランザクション。ROOM-005 か ROOM-006）。5. relates_to の確かめ
// （checkRelation）。6. MediaBinder.Bind（MEDIA-002、MEDIA-005）。7. events と event_relations に書く。8. active の構成員の feed に
// 溜め、Notifier.Notify（送信者を除く。Flush の前）。9. Feed.Flush（本人の pos を応答に入れる。この後は行の錠を取らない）。
// transactionId は要求の Idempotency-Key（送った端末へのエコー）。
func (t *Timeline) Post(ctx context.Context, tx *store.Tx, p boundary.Principal, roomId uuid.UUID, body gen.FtEventMls1, silent bool, transactionId uuid.UUID) (Posted, error) {
	me, device, err := self(p)
	if err != nil {
		return Posted{}, err
	}
	if _, err := t.d.lockRoom(ctx, tx, roomId); err != nil {
		return Posted{}, err
	}
	st, err := t.members.stateOf(ctx, tx, roomId, me, false)
	if err != nil {
		return Posted{}, err
	}
	if st != MemberActive {
		return Posted{}, ev(cgen.ROOM002)
	}
	if err := checkSize(body); err != nil {
		return Posted{}, err
	}
	if err := t.d.Keys.Groups.CheckSender(ctx, tx, roomId, device, body); err != nil {
		return Posted{}, err
	}
	now := t.d.now()
	if err := t.checkRelation(ctx, tx, roomId, me, body, now); err != nil {
		return Posted{}, err
	}
	tid := transactionId
	e, err := t.newMls(roomId, me, device, body, now)
	if err != nil {
		return Posted{}, err
	}
	e.Silent, e.TransactionId = silent, &tid
	if err := t.bind(ctx, tx, me, roomId, e.Id, body); err != nil {
		return Posted{}, err
	}
	pend, err := t.record(ctx, tx, e, body)
	if err != nil {
		return Posted{}, err
	}
	if err := t.d.Feed.Flush(ctx, tx); err != nil {
		return Posted{}, err
	}
	return Posted{EventId: e.Id, Pos: pend[me].Pos, Ts: now, TransactionId: &tid}, nil
}

// checkSize はイベントの大きさ（暗号文を含む本体の JSON が 256 KiB まで。ROOM-005）。
func checkSize(body gen.FtEventMls1) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return wrap(err)
	}
	if len(raw) > eventBodyMax {
		return ev(cgen.ROOM005)
	}
	return nil
}

// checkRelation は relates_to の確かめ（第4章 3.2、全体実装設計書 4.2）。対象が別のトークなら ROOM-012。replace は対象が同じトークに
// 無ければ ROOM-012（送信者と期限を確かめられない）。対象が ft.event.mls/1 で、それ自身が replace でなく（編集を連ねて期限を越えない）、
// 送信者が本人で、ts から room.edit_window（15 分）以内でなければ ROOM-009。reply・annotation・thread は対象が無くても受け付ける
// （相手が先に届く順序を許す）。
func (t *Timeline) checkRelation(ctx context.Context, q store.Querier, roomId, me uuid.UUID, body gen.FtEventMls1, now time.Time) error {
	if body.RelatesTo == nil {
		return nil
	}
	target, err := parseId(body.RelatesTo.EventId, "body/body/relates_to/event_id")
	if err != nil {
		return err
	}
	var room uuid.UUID
	var schema string
	var sender *uuid.UUID
	var ts time.Time
	var rel *string
	err = q.QueryRow(ctx, `SELECT room_id, schema, sender_id, ts, plain->'relates_to'->>'rel_type' FROM events WHERE id = $1`, target).
		Scan(&room, &schema, &sender, &ts, &rel)
	found := err == nil
	if err != nil && !noRows(err) {
		return wrap(err)
	}
	if found && room != roomId {
		return ev(cgen.ROOM012)
	}
	if body.RelatesTo.RelType != gen.FtEventMls1RelatesToRelTypeReplace {
		return nil
	}
	if !found {
		return ev(cgen.ROOM012)
	}
	if EventSchema(schema) != SchemaMls || sender == nil || *sender != me || (rel != nil && *rel == "replace") ||
		now.Sub(ts) > t.d.cfgDur("room.edit_window") {
		return ev(cgen.ROOM009)
	}
	return nil
}

// newMls は ft.event.mls/1 の行を組む（識別子は UUIDv7、ts は now）。
func (t *Timeline) newMls(roomId, sender, device uuid.UUID, body gen.FtEventMls1, now time.Time) (Event, error) {
	ct, epoch, plain, err := mlsParts(body)
	if err != nil {
		return Event{}, err
	}
	s, d := sender, device
	return Event{Id: t.d.Ids.NewV7(), RoomId: roomId, Ts: now, Schema: SchemaMls, Sender: &s, SenderDevice: &d, Epoch: &epoch,
		Ciphertext: ct, Plain: plain}, nil
}

// bind は添付をイベントに結び付ける（MediaBinder.Bind。添付が無ければ何もしない）。
func (t *Timeline) bind(ctx context.Context, tx *store.Tx, sender, roomId, eventId uuid.UUID, body gen.FtEventMls1) error {
	ids, err := attachmentIds(body)
	if err != nil || len(ids) == 0 {
		return err
	}
	return t.ports.media.Bind(ctx, tx, sender, roomId, eventId, ids)
}

// attachmentIds は本体の添付の識別子。
func attachmentIds(body gen.FtEventMls1) ([]uuid.UUID, error) {
	if body.Attachments == nil {
		return nil, nil
	}
	list := make([]string, len(*body.Attachments))
	for i, a := range *body.Attachments {
		list[i] = a.Id
	}
	return parseIds(list, "body/body/attachments")
}

// record は ft.event.mls/1 を書き、関係のコピーと last_event_at、active の構成員の feed、Notifier.Notify（送信者を除く）を行う。
// 戻りは宛先ごとの溜めた行。
func (t *Timeline) record(ctx context.Context, tx *store.Tx, e Event, body gen.FtEventMls1) (map[uuid.UUID]*store.FeedPending, error) {
	if err := insertEvent(ctx, tx, e); err != nil {
		return nil, err
	}
	if body.RelatesTo != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO event_relations (event_id, room_id, rel_type, target_event_id, key) VALUES ($1, $2, $3, $4, $5)`,
			e.Id, e.RoomId, string(body.RelatesTo.RelType), body.RelatesTo.EventId, body.RelatesTo.Key); err != nil {
			return nil, wrap(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE rooms SET last_event_at = $2 WHERE id = $1`, e.RoomId, e.Ts); err != nil {
		return nil, wrap(err)
	}
	return t.fanout(ctx, tx, e, true)
}

// fanout はイベントを active の構成員の feed に溜める。notify なら Notifier.Notify（送信者を除く。Flush の前。全体実装設計書 2.4）。
func (t *Timeline) fanout(ctx context.Context, tx *store.Tx, e Event, notify bool) (map[uuid.UUID]*store.FeedPending, error) {
	active, err := t.members.Of(ctx, tx, e.RoomId)
	if err != nil {
		return nil, err
	}
	to := memberIds(active)
	pend, err := t.d.emit(tx, to, kindRoomEvent, e.RoomId, e.Id, nil)
	if err != nil {
		return nil, err
	}
	if notify {
		if e.Sender != nil {
			to = without(to, *e.Sender)
		}
		if err := t.ports.notifier.Notify(ctx, tx, e, to); err != nil {
			return nil, err
		}
	}
	return pend, nil
}

// SystemEvent は ft.event.system/1（平文。本文なし）を記録し、active の構成員へ流して Notifier.Notify（通知するかは srv-push の規則）。
// kind が ft.event.system/1 の列挙の外ならプログラムの誤り。RoomLock（無いか削除済みは ROOM-001）。戻りはイベントの識別子。
func (t *Timeline) SystemEvent(ctx context.Context, tx *store.Tx, roomId uuid.UUID, kind string, params map[string]any) (uuid.UUID, error) {
	if !gen.FtEventSystem1Kind(kind).Valid() {
		return uuid.Nil, fault.Wrap(errors.New("rooms: システムの知らせの kind が列挙の外: " + kind))
	}
	if params == nil {
		params = map[string]any{}
	}
	plain, err := json.Marshal(map[string]any{"kind": kind, "params": params})
	if err != nil {
		return uuid.Nil, wrap(err)
	}
	return t.plainEvent(ctx, tx, roomId, SchemaSystem, plain, true)
}

// CallEvent は ft.event.call/1（srv-calls から。started、ended、missed、declined、cancelled）。RoomLock（呼ぶ側は calls の行の前に
// 取る）。Notifier は呼ばない（着信は srv-calls。第7章 7）。kind が列挙の外ならプログラムの誤り。戻りはイベントの識別子。
func (t *Timeline) CallEvent(ctx context.Context, tx *store.Tx, roomId uuid.UUID, body gen.FtEventCall1) (uuid.UUID, error) {
	if !body.Kind.Valid() {
		return uuid.Nil, fault.Wrap(errors.New("rooms: 通話の記録の kind が列挙の外"))
	}
	plain, err := json.Marshal(body)
	if err != nil {
		return uuid.Nil, wrap(err)
	}
	return t.plainEvent(ctx, tx, roomId, SchemaCall, plain, false)
}

// plainEvent は送信者の無い平文のイベント（system と call）を書く。
func (t *Timeline) plainEvent(ctx context.Context, tx *store.Tx, roomId uuid.UUID, schema EventSchema, plain json.RawMessage, notify bool) (uuid.UUID, error) {
	if _, err := t.d.lockRoom(ctx, tx, roomId); err != nil {
		return uuid.Nil, err
	}
	now := t.d.now()
	e := Event{Id: t.d.Ids.NewV7(), RoomId: roomId, Ts: now, Schema: schema, Plain: plain}
	if err := insertEvent(ctx, tx, e); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE rooms SET last_event_at = $2 WHERE id = $1`, roomId, now); err != nil {
		return uuid.Nil, wrap(err)
	}
	_, err := t.fanout(ctx, tx, e, notify)
	return e.Id, err
}

// Redact は POST /rooms/{id}/events/{eid}/redact（第4章 3.2、第12章 9）。RoomLock。本人が active でなければ ROOM-002。対象が無いか
// 別のトークか ft.event.mls/1 でなければ ROOM-007。既に削除済みの印なら ROOM-008。送信者は room.retract_window（7 日。null は無期限。
// 対象が annotation なら期限なし）を過ぎれば ROOM-009。送信者でない人は、グループの作成者か管理者なら期限なしで reason が要る（空白
// だけなら NET-001、path body/reason）。それ以外は ROOM-003。消すのは対象と、対象を replace する同じ送信者のイベントの全部（編集の
// 本文は取り消すメッセージの本文。第12章 9）。それぞれ暗号文を NULL にして削除済みの印、MediaBinder.Unbind、ft.event.redaction/1 を
// active の構成員へ（これが全員への知らせ。Notifier は呼ばない）。物理削除の猶予は置かない（I-2）。
func (t *Timeline) Redact(ctx context.Context, tx *store.Tx, p boundary.Principal, roomId, eventId uuid.UUID, reason *string) error {
	me, device, err := self(p)
	if err != nil {
		return err
	}
	room, err := t.d.lockRoom(ctx, tx, roomId)
	if err != nil {
		return err
	}
	st, err := t.members.stateOf(ctx, tx, roomId, me, false)
	if err != nil {
		return err
	}
	if st != MemberActive {
		return ev(cgen.ROOM002)
	}
	target, err := scanEvent(tx.QueryRow(ctx, `SELECT `+eventCols+` FROM events WHERE id = $1 FOR UPDATE`, eventId))
	if noRows(err) {
		return ev(cgen.ROOM007)
	}
	if err != nil {
		return wrap(err)
	}
	if target.RoomId != roomId || target.Schema != SchemaMls {
		return ev(cgen.ROOM007)
	}
	if target.RedactedAt != nil {
		return ev(cgen.ROOM008)
	}
	now := t.d.now()
	if target.Sender != nil && *target.Sender == me {
		var rel struct {
			RelatesTo *struct {
				RelType string `json:"rel_type"`
			} `json:"relates_to"`
		}
		_ = json.Unmarshal(target.Plain, &rel)
		w := t.d.Config.Get("room.retract_window")
		annotation := rel.RelatesTo != nil && rel.RelatesTo.RelType == "annotation"
		if !annotation && !w.IsNull() && now.Sub(target.Ts) > w.Duration() {
			return ev(cgen.ROOM009)
		}
		if blank(reason) {
			reason = nil
		}
	} else {
		creator := room.Kind == KindGroup && room.Creator != nil && *room.Creator == me
		if !creator && p.Role != boundary.RoleAdmin {
			return ev(cgen.ROOM003)
		}
		if blank(reason) {
			return netError("body/reason", "他の人の投稿の取消には理由が要る")
		}
	}
	ids, err := scanUuids(ctx, tx, `SELECT e.id FROM event_relations r JOIN events e ON e.id = r.event_id
WHERE r.target_event_id = $1 AND r.rel_type = 'replace' AND e.sender_id = $2 AND e.redacted_at IS NULL ORDER BY e.id`, eventId, target.Sender)
	if err != nil {
		return err
	}
	active, err := t.members.Of(ctx, tx, roomId)
	if err != nil {
		return err
	}
	to := memberIds(active)
	d := device
	for _, id := range append([]uuid.UUID{eventId}, ids...) {
		if _, err := tx.Exec(ctx, `UPDATE events SET ciphertext = NULL, redacted_at = $2, redacted_by = $3, redact_reason = $4 WHERE id = $1`,
			id, now, me, reason); err != nil {
			return wrap(err)
		}
		if err := t.ports.media.Unbind(ctx, tx, id); err != nil {
			return err
		}
		if err := t.redactionEvent(ctx, tx, roomId, id, me, &d, reason, now, to); err != nil {
			return err
		}
	}
	return nil
}

// redactionEvent は 1 件の取消のイベント（ft.event.redaction/1）を書いて宛先の feed に溜める（Notifier は呼ばない。last_event_at は
// 進めない）。
func (t *Timeline) redactionEvent(ctx context.Context, tx *store.Tx, roomId, target, by uuid.UUID, device *uuid.UUID, reason *string,
	now time.Time, to []uuid.UUID) error {
	plain, err := json.Marshal(gen.FtEventRedaction1{Redacts: target.String(), By: by.String(), Reason: reason})
	if err != nil {
		return wrap(err)
	}
	b := by
	e := Event{Id: t.d.Ids.NewV7(), RoomId: roomId, Ts: now, Schema: SchemaRedaction, Sender: &b, SenderDevice: device, Plain: plain}
	if err := insertEvent(ctx, tx, e); err != nil {
		return err
	}
	_, err = t.d.emit(tx, to, kindRoomEvent, roomId, e.Id, nil)
	return err
}

// redactAllOf は一括の取消（削除で消す・抹消の hook。同じトランザクション。第12章 9、全体実装設計書 I-16）。行うのは暗号文と添付の
// 参照を消すことだけで、feed には書かない。本人の送った ft.event.mls/1 で削除済みの印でない物をトークの識別子の順にまとめ、トーク
// ごとに RoomLock（無いか削除済みは飛ばす）の後に 1 つの UPDATE で削除済みの印にする（by は本人）。消したイベントを 1,000 件ずつ
// MediaBinder.UnbindEvents に渡し、送り残しの表 redaction_backlog に入れる。1 件以上なら最後に仕事 redaction_fanout を積む（取消の
// イベントは srv-worker の仕事が FanoutRedactions で出す）。
func (t *Timeline) redactAllOf(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	rooms, err := scanUuids(ctx, tx, `SELECT DISTINCT room_id FROM events WHERE sender_id = $1 AND schema = 'ft.event.mls/1' AND redacted_at IS NULL
ORDER BY room_id`, userId)
	if err != nil {
		return err
	}
	now, total := t.d.now(), 0
	for _, roomId := range rooms {
		room, ok, err := t.d.lockRoomAny(ctx, tx, roomId)
		if err != nil {
			return err
		}
		if !ok || room.State == RoomDeleted {
			continue
		}
		ids, err := scanUuids(ctx, tx, `UPDATE events SET ciphertext = NULL, redacted_at = $3, redacted_by = $1
WHERE room_id = $2 AND sender_id = $1 AND schema = 'ft.event.mls/1' AND redacted_at IS NULL RETURNING id`, userId, roomId, now)
		if err != nil {
			return err
		}
		ids = sortedIds(ids)
		for start := 0; start < len(ids); start += redactionBatch {
			batch := ids[start:min(start+redactionBatch, len(ids))]
			if err := t.ports.media.UnbindEvents(ctx, tx, batch); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO redaction_backlog (event_id, room_id, user_id, queued_at) SELECT unnest($1::uuid[]), $2, $3, $4`,
				batch, roomId, userId, now); err != nil {
				return wrap(err)
			}
		}
		total += len(ids)
	}
	if total == 0 {
		return nil
	}
	_, err = t.d.Jobs.Enqueue(ctx, tx, "redaction_fanout", map[string]string{"user_id": userId.String()}, now, "redaction_fanout:"+userId.String())
	return err
}

// FanoutRedactions は srv-worker RedactionFanoutHandler（仕事 redaction_fanout）から、1 つのトランザクションで本人の送り残しを limit
// 件まで処理する（I-16、I-10）。送り残しを錠なしで読んでトークを知り、トークごとに RoomLock の後にその行を FOR UPDATE で取り直す
// （錠の順は rooms が先。読めない行は他の処理が消した物で出さない）。トークが無いか削除済みなら行を消すだけ（受け手が残らない）。
// 他は各イベントに取消のイベント（sender は本人、端末は無し、by は本人）を active の構成員へ出し、行を消す。最後に Feed.Flush。
// Notifier は呼ばず、last_event_at は進めない。戻りは取った行の数（0 なら終わり）。
func (t *Timeline) FanoutRedactions(ctx context.Context, userId uuid.UUID, limit int) (int, error) {
	n := 0
	err := t.d.Pool.InTx(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query(ctx, `SELECT room_id, event_id FROM redaction_backlog WHERE user_id = $1 ORDER BY room_id, event_id LIMIT $2`, userId, limit)
		if err != nil {
			return wrap(err)
		}
		byRoom := map[uuid.UUID][]uuid.UUID{}
		var order []uuid.UUID
		for rows.Next() {
			var room, event uuid.UUID
			if err := rows.Scan(&room, &event); err != nil {
				rows.Close()
				return wrap(err)
			}
			if _, ok := byRoom[room]; !ok {
				order = append(order, room)
			}
			byRoom[room] = append(byRoom[room], event)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return wrap(err)
		}
		now := t.d.now()
		for _, roomId := range order {
			room, ok, err := t.d.lockRoomAny(ctx, tx, roomId)
			if err != nil {
				return err
			}
			got, err := scanUuids(ctx, tx, `SELECT event_id FROM redaction_backlog WHERE user_id = $1 AND room_id = $2 AND event_id = ANY($3)
ORDER BY event_id FOR UPDATE`, userId, roomId, byRoom[roomId])
			if err != nil {
				return err
			}
			n += len(got)
			if ok && room.State != RoomDeleted {
				active, err := t.members.Of(ctx, tx, roomId)
				if err != nil {
					return err
				}
				to := memberIds(active)
				for _, id := range got {
					if err := t.redactionEvent(ctx, tx, roomId, id, userId, nil, nil, now, to); err != nil {
						return err
					}
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM redaction_backlog WHERE event_id = ANY($1)`, got); err != nil {
				return wrap(err)
			}
		}
		return t.d.Feed.Flush(ctx, tx)
	})
	return n, err
}

// scanUuids は識別子の 1 列を読む。
func scanUuids(ctx context.Context, q store.Querier, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, wrap(err)
		}
		out = append(out, id)
	}
	return out, wrap(rows.Err())
}

// Get は GET /rooms/{id}/events/{eid}。このトークに無ければ ROOM-007。削除済みの印は骨（body なし、redacted あり）を 200 で返す。
func (t *Timeline) Get(ctx context.Context, p boundary.Principal, roomId, eventId uuid.UUID) (EventSkeleton, error) {
	_, device, err := self(p)
	if err != nil {
		return EventSkeleton{}, err
	}
	e, err := scanEvent(t.d.Pool.QueryRow(ctx, `SELECT `+eventCols+` FROM events WHERE id = $1 AND room_id = $2`, eventId, roomId))
	if noRows(err) {
		return EventSkeleton{}, ev(cgen.ROOM007)
	}
	if err != nil {
		return EventSkeleton{}, wrap(err)
	}
	return skeleton(e, device)
}

// ForFeed は srv-boundary の EventSource の中身（hub と /sync）：イベントの識別子から ft.room_event/1 を 1 回の読みで返す。利用者が
// 今の構成員でないトークと削除済みのトークの物は返さない（地図に入らない）。削除済みの印は骨。transaction_id は deviceId が送った
// 端末の時だけ。
func (t *Timeline) ForFeed(ctx context.Context, userId, deviceId uuid.UUID, eventIds []uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	out := map[uuid.UUID]json.RawMessage{}
	if len(eventIds) == 0 {
		return out, nil
	}
	cols := `e.id, e.room_id, e.ts, e.schema, e.sender_id, e.sender_device_id, e.epoch, e.ciphertext, e.plain, e.redacted_at, e.redacted_by,
e.redact_reason, e.transaction_id, e.silent, e.scheduled_at`
	evs, err := scanEvents(ctx, t.d.Pool, `SELECT `+cols+` FROM events e
JOIN room_members m ON m.room_id = e.room_id AND m.user_id = $1 AND m.state = 'active'
JOIN rooms r ON r.id = e.room_id AND r.state <> 'deleted' WHERE e.id = ANY($2)`, userId, eventIds)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		sk, err := skeleton(e, deviceId)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(sk)
		if err != nil {
			return nil, wrap(err)
		}
		out[e.Id] = raw
	}
	return out, nil
}

// Page は GET /rooms/{id}/events（第2章 3 の cursor、types の絞り）。戻りは (要素, has_more)。
func (t *Timeline) Page(ctx context.Context, p boundary.Principal, roomId uuid.UUID, c Cursor, types []string) ([]EventSkeleton, bool, error) {
	sql, args := `SELECT `+eventCols+` FROM events WHERE room_id = $1`, []any{roomId}
	if len(types) > 0 {
		args = append(args, types)
		sql += ` AND schema = ANY($2)`
	}
	return t.page(ctx, p, c, sql, `id`, args)
}

// page は cursor の付いたイベントの読みを骨にする。
func (t *Timeline) page(ctx context.Context, p boundary.Principal, c Cursor, sql, col string, args []any) ([]EventSkeleton, bool, error) {
	_, device, err := self(p)
	if err != nil {
		return nil, false, err
	}
	sql, args = whereUUID(c, sql, col, args)
	evs, err := scanEvents(ctx, t.d.Pool, sql, args...)
	if err != nil {
		return nil, false, err
	}
	evs, more := finish(evs, c)
	out := make([]EventSkeleton, len(evs))
	for i, e := range evs {
		if out[i], err = skeleton(e, device); err != nil {
			return nil, false, err
		}
	}
	return out, more, nil
}

// Relations は GET /rooms/{id}/events/{eid}/relations（元が同じトークに無ければ ROOM-007。rel_type の絞り、第2章 3 の cursor）。
func (t *Timeline) Relations(ctx context.Context, p boundary.Principal, roomId, eventId uuid.UUID, relType *string, c Cursor) ([]EventSkeleton, bool, error) {
	var one int
	err := t.d.Pool.QueryRow(ctx, `SELECT 1 FROM events WHERE id = $1 AND room_id = $2`, eventId, roomId).Scan(&one)
	if noRows(err) {
		return nil, false, ev(cgen.ROOM007)
	}
	if err != nil {
		return nil, false, wrap(err)
	}
	cols := `e.id, e.room_id, e.ts, e.schema, e.sender_id, e.sender_device_id, e.epoch, e.ciphertext, e.plain, e.redacted_at, e.redacted_by,
e.redact_reason, e.transaction_id, e.silent, e.scheduled_at`
	sql, args := `SELECT `+cols+` FROM event_relations r JOIN events e ON e.id = r.event_id WHERE r.target_event_id = $1 AND r.room_id = $2`,
		[]any{eventId, roomId}
	if relType != nil {
		args = append(args, *relType)
		sql += ` AND r.rel_type = $3`
	}
	return t.page(ctx, p, c, sql, `e.id`, args)
}

// Snapshot は /sync の since なし：srv-store Pool.BeginSnapshot の 1 つの見え方の中で、本ブロックのトーク（Rooms.List と同じ形）、
// srv-identity Users.List と Devices.List（本人の端末。100 件ずつ全部）、srv-store Feed.LastPos を同じトランザクションを q として読む
// （第2章 5.9）。読むだけなので最後に Rollback。
func (t *Timeline) Snapshot(ctx context.Context, p boundary.Principal) (SyncInitial, error) {
	me, device, err := self(p)
	if err != nil {
		return SyncInitial{}, err
	}
	snap, err := t.d.Pool.BeginSnapshot(ctx)
	if err != nil {
		return SyncInitial{}, err
	}
	defer snap.Rollback(ctx)
	rooms, err := t.rooms.listAll(ctx, snap, me)
	if err != nil {
		return SyncInitial{}, err
	}
	out := SyncInitial{Snapshot: gen.SyncSnapshot{Rooms: rooms, Users: []gen.User{}, Devices: []gen.Device{}}}
	for page := (identity.Page{Limit: 100}); ; {
		items, more, err := t.d.Identity.Users.List(ctx, snap, p, page)
		if err != nil {
			return SyncInitial{}, err
		}
		out.Snapshot.Users = append(out.Snapshot.Users, items...)
		if !more || len(items) == 0 {
			break
		}
		last := items[len(items)-1].Id
		page.After = &last
	}
	for page := (identity.Page{Limit: 100}); ; {
		items, more, err := t.d.Identity.Devices.List(ctx, snap, me, device, page)
		if err != nil {
			return SyncInitial{}, err
		}
		out.Snapshot.Devices = append(out.Snapshot.Devices, items...)
		if !more || len(items) == 0 {
			break
		}
		last := items[len(items)-1].Id
		page.After = &last
	}
	if out.Pos, err = t.d.Feed.LastPos(ctx, snap, me); err != nil {
		return SyncInitial{}, err
	}
	return out, nil
}

// Export は GET /rooms/{id}/export：骨の一括（本文の書き出しは端末。第2章 5.5）。第2章 3 の cursor（pos は利用者ごとで使わない）。
func (t *Timeline) Export(ctx context.Context, p boundary.Principal, roomId uuid.UUID, c Cursor) ([]EventSkeleton, bool, error) {
	return t.Page(ctx, p, roomId, c, nil)
}

// PostScheduled は srv-worker ScheduledSendHandler（仕事の payload は scheduled_id）から、予約送信を送る（第4章 5.8、第12章 5.7）。
// 送らなかった時の戻りは空の Posted と nil。1. 予約の行を錠なしで読みトークを知る（無ければ空）。2. RoomLock の後にトークを読む
// （無いか削除済みなら空：予約は削除の時に cancelled 済み）。3. 予約の行を FOR UPDATE、pending でなければ空（同じ仕事の 2 度目。
// I-10）。4. 送信者が active でなければ cancelled（abandon）。5. relates_to の確かめ（Post と同じ）に当たらなければ同じく cancelled
// （送る時の誤りは返す相手がいない）。6. ts は実際に送った時刻、予約の時刻は scheduled_at（ts を予約の時刻にすると遅れた予約が
// 履歴の過去に入る）。CheckSender は呼ばない（epoch が古くてよい）。添付は MediaBinder.Rebind（確かめは予約の時に済み）。
// 7. Post の 7〜8 と同じ。8. sent と room.scheduled{sent}。9. Feed.Flush。
func (t *Timeline) PostScheduled(ctx context.Context, tx *store.Tx, scheduledId uuid.UUID) (Posted, error) {
	var roomId uuid.UUID
	err := tx.QueryRow(ctx, `SELECT room_id FROM scheduled_events WHERE id = $1`, scheduledId).Scan(&roomId)
	if noRows(err) {
		return Posted{}, nil
	}
	if err != nil {
		return Posted{}, wrap(err)
	}
	room, ok, err := t.d.lockRoomAny(ctx, tx, roomId)
	if err != nil || !ok || room.State == RoomDeleted {
		return Posted{}, err
	}
	s, err := scanScheduled(tx.QueryRow(ctx, `SELECT `+scheduledCols+` FROM scheduled_events WHERE id = $1 FOR UPDATE`, scheduledId))
	if err != nil {
		return Posted{}, wrap(err)
	}
	if s.State != ScheduledPending {
		return Posted{}, nil
	}
	st, err := t.members.stateOf(ctx, tx, roomId, s.UserId, false)
	if err != nil {
		return Posted{}, err
	}
	now := t.d.now()
	if st != MemberActive {
		return Posted{}, t.abandonScheduled(ctx, tx, s)
	}
	if err := t.checkRelation(ctx, tx, roomId, s.UserId, s.Body, now); err != nil {
		var e *fault.Event
		if errors.As(err, &e) {
			return Posted{}, t.abandonScheduled(ctx, tx, s)
		}
		return Posted{}, err
	}
	e, err := t.newMls(roomId, s.UserId, s.DeviceId, s.Body, now)
	if err != nil {
		return Posted{}, err
	}
	at := s.SendAt
	e.ScheduledAt = &at
	ids, err := attachmentIds(s.Body)
	if err != nil {
		return Posted{}, err
	}
	if len(ids) > 0 {
		if err := t.ports.media.Rebind(ctx, tx, s.Id, e.Id); err != nil {
			return Posted{}, err
		}
	}
	pend, err := t.record(ctx, tx, e, s.Body)
	if err != nil {
		return Posted{}, err
	}
	if err := t.setScheduled(ctx, tx, s, ScheduledSent); err != nil {
		return Posted{}, err
	}
	if err := t.d.Feed.Flush(ctx, tx); err != nil {
		return Posted{}, err
	}
	return Posted{EventId: e.Id, Pos: pend[s.UserId].Pos, Ts: now}, nil
}

// abandonScheduled は送れない予約を cancelled にし、添付の結び付きを外し（外さないと I-5 の猶予に乗らない）、room.scheduled を本人へ。
func (t *Timeline) abandonScheduled(ctx context.Context, tx *store.Tx, s ScheduledEvent) error {
	if err := t.ports.media.UnbindScheduled(ctx, tx, s.Id); err != nil {
		return err
	}
	if err := t.setScheduled(ctx, tx, s, ScheduledCancelled); err != nil {
		return err
	}
	return t.d.Feed.Flush(ctx, tx)
}

// setScheduled は予約の状態を変え、room.scheduled{scheduled_id, state} を本人へ。
func (t *Timeline) setScheduled(ctx context.Context, tx *store.Tx, s ScheduledEvent, state ScheduledState) error {
	if _, err := tx.Exec(ctx, `UPDATE scheduled_events SET state = $2 WHERE id = $1`, s.Id, string(state)); err != nil {
		return wrap(err)
	}
	_, err := t.d.emit(tx, []uuid.UUID{s.UserId}, kindRoomScheduled, s.RoomId, t.d.Ids.NewV7(), roomScheduled{ScheduledId: s.Id, State: state})
	return err
}
