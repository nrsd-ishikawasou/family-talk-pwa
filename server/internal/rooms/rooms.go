// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package rooms はトーク、構成員、イベント、既読、入力中、予約送信、家族全員のトークを持つ（srv-rooms。
// 基本設計書 第4章、第2章 5.5、6、11、12、第12章 5.2、5.7、9）。本文は暗号文のまま記録し、平文を持たない（I-1）。
// MLS のグループの記録は srv-keys の Groups が持ち、本ブロックは /rooms/{id}/mls/* を受けて Groups を呼び、構成員の待ちを解く。
// 予期した失敗はエラーコード表の *fault.Event、予期しない失敗は *fault.Fault を error で返す（全体実装設計書 2.7）。
// 書き込みは呼ぶ側が渡す *store.Tx の中で行う（I-8）。feed への書き込みは store.Feed.Append で、Feed.Flush を自分で呼ぶのは
// Timeline.Post（応答の pos）と、裏の処理の Timeline.PostScheduled・FanoutRedactions だけ（他は Tx.Commit が呼ぶ。全体実装設計書 2.4）。
package rooms

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/keys"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 固定の値（設定の項目が無い物。第2章 6.1、api の maxItems）。
const (
	eventBodyMax   = 256 << 10 // イベントの大きさ（暗号文を含む本体の JSON。ROOM-005）
	attachmentsMax = 50        // 1 つのイベントの添付の件数
	pageDefault    = 50        // 一覧の件数の既定（第2章 3）
)

// Deps は本ブロックが使う下のブロック。
type Deps struct {
	Pool     *store.Pool
	Feed     *store.Feed
	RoomLock store.RoomLock
	Jobs     *store.Jobs
	Ids      *ids.Ids
	Clock    clock.Clock
	Config   *config.Config
	// Identity は srv-identity（構成員と端末の読み、役割、hook の登録と知らせ）。
	Identity *identity.Identity
	// Keys は srv-keys（Groups と端末宛メールボックス）。
	Keys *keys.Keys
}

// Block は本ブロックの公開操作（B-100〜B-110）を束ねる。Notifier、MediaBinder、CallEnder と入力中の送り先は、上のブロックと
// srv-boundary が組まれた後に Set の 4 つで渡し、hook の登録は Rooms.Register、最後に Ready を呼ぶ。
type Block struct {
	d          *Deps
	ports      *ports
	Rooms      *Rooms
	Members    *Members
	RoomMe     *RoomMeStore
	Timeline   *Timeline
	Receipts   *Receipts
	Typing     *Typing
	Scheduled  *Scheduled
	FamilyRoom *FamilyRoom
	Gc         *Gc
}

// ports は組み立ての後に渡す上のブロックの実装（依存の逆転。B-109）と入力中の送り先。
type ports struct {
	notifier  Notifier
	media     MediaBinder
	calls     CallEnder
	broadcast func(userIds []uuid.UUID, kind string, payload any)
}

// New は本ブロックを組む。必須の欄が無いのはプログラムの誤り。
func New(d Deps) *Block {
	if d.Pool == nil || d.Feed == nil || d.Jobs == nil || d.Ids == nil || d.Clock == nil || d.Config == nil || d.Identity == nil || d.Keys == nil {
		panic("rooms: Deps の必須の欄が無い")
	}
	dp := &d
	pt := &ports{}
	b := &Block{d: dp, ports: pt}
	b.Members = &Members{d: dp}
	b.RoomMe = &RoomMeStore{d: dp}
	b.Timeline = &Timeline{d: dp, ports: pt, members: b.Members}
	b.Receipts = &Receipts{d: dp, members: b.Members}
	b.Typing = &Typing{d: dp, ports: pt, members: b.Members}
	b.Scheduled = &Scheduled{d: dp, ports: pt, timeline: b.Timeline, members: b.Members}
	b.FamilyRoom = &FamilyRoom{d: dp, timeline: b.Timeline}
	b.Rooms = &Rooms{d: dp, ports: pt, members: b.Members, timeline: b.Timeline, receipts: b.Receipts, roomMe: b.RoomMe, family: b.FamilyRoom,
		scheduled: b.Scheduled}
	b.Members.rooms = b.Rooms
	b.Members.timeline = b.Timeline
	b.Timeline.rooms = b.Rooms
	b.Gc = &Gc{d: dp, rooms: b.Rooms}
	return b
}

// SetNotifier は srv-push の Notifier を渡す（srv-push は本ブロックの上なので組み立ての後）。
func (b *Block) SetNotifier(n Notifier) {
	b.ports.notifier = n
}

// SetMediaBinder は srv-media の MediaBinder を渡す（srv-media は本ブロックの上）。
func (b *Block) SetMediaBinder(m MediaBinder) {
	b.ports.media = m
}

// SetCallEnder は srv-calls の CallEnder を渡す（srv-calls は本ブロックの上）。
func (b *Block) SetCallEnder(c CallEnder) {
	b.ports.calls = c
}

// SetBroadcast は入力中（room.typing。feed に書かない）の送り先を渡す。srv-boundary の Hub.Broadcast で、hub は本ブロックの
// TypingSink を受けて組まれるので、hub の後に渡す（第2章 6.2 の room.typing）。
func (b *Block) SetBroadcast(fn func(userIds []uuid.UUID, kind string, payload any)) {
	b.ports.broadcast = fn
}

// 組み立ての誤り（起動しない）。
var (
	errNoNotifier  = errors.New("rooms: Notifier が渡されていない")
	errNoMedia     = errors.New("rooms: MediaBinder が渡されていない")
	errNoCalls     = errors.New("rooms: CallEnder が渡されていない")
	errNoBroadcast = errors.New("rooms: 入力中の送り先が渡されていない")
)

// Ready は起動の最後に呼ぶ。上のブロックの実装か入力中の送り先が無い組み立ては Fault（起動しない）。
func (b *Block) Ready() error {
	switch {
	case b.ports.notifier == nil:
		return fault.Wrap(errNoNotifier)
	case b.ports.media == nil:
		return fault.Wrap(errNoMedia)
	case b.ports.calls == nil:
		return fault.Wrap(errNoCalls)
	case b.ports.broadcast == nil:
		return fault.Wrap(errNoBroadcast)
	}
	return nil
}

// Apis は生成のタグ rooms の 25 操作の実装を返す（gen.Implementations の Rooms の欄）。
func (b *Block) Apis() *Apis {
	return &Apis{b: b}
}

// Membership は srv-boundary の RoomMembership の実装を返す（Verifiers の欄）。
func (b *Block) Membership() boundary.RoomMembership {
	return membership{members: b.Members}
}

// Snapshotter は srv-boundary の Snapshotter の実装を返す（Links の欄）。
func (b *Block) Snapshotter() boundary.Snapshotter {
	return snapshotter{timeline: b.Timeline}
}

// EventSource は srv-boundary の EventSource の実装を返す（Links の欄）。
func (b *Block) EventSource() boundary.EventSource {
	return eventSource{timeline: b.Timeline}
}

// TypingSink は srv-boundary の TypingSink の実装を返す（Links の欄）。
func (b *Block) TypingSink() boundary.TypingSink {
	return typingSink{typing: b.Typing}
}

// Reader は srv-keys の RoomReader の実装を返す（Keys.SetRooms に渡す）。
func (b *Block) Reader() keys.RoomReader {
	return roomReader{members: b.Members}
}

// RoomKind はトークの種類（rooms.kind。第4章 4.1）。
type RoomKind string

// 種類の 4 つ。
const (
	KindDm     RoomKind = "dm"
	KindGroup  RoomKind = "group"
	KindSelf   RoomKind = "self"
	KindFamily RoomKind = "family"
)

// RoomState はトークの状態（rooms.state）。
type RoomState string

// 状態の 3 つ。
const (
	RoomNormal  RoomState = "normal"
	RoomEmpty   RoomState = "empty"
	RoomDeleted RoomState = "deleted"
)

// Room はトークの 1 行（rooms）。
type Room struct {
	Id   uuid.UUID
	Kind RoomKind
	// Creator はグループの作成者（作成者の退出・外し・削除で nil。他の種類は nil）。
	Creator     *uuid.UUID
	State       RoomState
	EmptySince  *time.Time
	LastEventAt time.Time
	CreatedAt   time.Time
	DeletedAt   *time.Time
}

// Rooms はトークの作成、読み、削除と、srv-identity の hook の処理（B-100）。
type Rooms struct {
	d         *Deps
	ports     *ports
	members   *Members
	timeline  *Timeline
	receipts  *Receipts
	roomMe    *RoomMeStore
	family    *FamilyRoom
	scheduled *Scheduled
}

// DeleteRule はトークの削除の規則（第2章 10 の x-authz-rule）。
type DeleteRule string

// 規則の 2 つ（gc は Gc の中で同じ処理を直に呼ぶ）。
const (
	// RuleRoomDelete は DELETE /rooms/{id}：作成者か構成員の管理者。
	RuleRoomDelete DeleteRule = "room_delete"
	// RuleEmptyGroup は DELETE /admin/rooms/{id}：構成員の残らないグループだけ。
	RuleEmptyGroup DeleteRule = "empty_group"
)

// Create は POST /rooms（第4章 4.1、DD-4-1）。memberIds は作成者を含めない。dm は 1 人で本人でない、group は 2 人以上。重なりと本人と
// 人数の誤りは NET-001（path body/members）。memberIds の users の行を srv-identity Users.LockShared で識別子の順に共有の錠で読み、
// 人が無いか抹消済みは MEM-001、停止中か削除済みは MEM-002。dm は同じ 2 人の dm_key が
// あれば ROOM-004（params.room_id）。同時の 2 つの作成は ON CONFLICT で後の方が先のコミットを待ち、先の行を読んで ROOM-004
// （第2章 11）。作成者は active、他は pending_add。Groups.Create（leaf 0 は作成者の端末。同じ tx）。room.settings と各構成員の
// room.member（作成者は added、他は pending_add）を Audience へ。group は room_created。
func (r *Rooms) Create(ctx context.Context, tx *store.Tx, p boundary.Principal, kind RoomKind, memberIds []uuid.UUID) (RoomView, error) {
	me, device, err := self(p)
	if err != nil {
		return RoomView{}, err
	}
	if kind != KindDm && kind != KindGroup {
		return RoomView{}, netError("body/kind", "dm か group")
	}
	seen := map[uuid.UUID]bool{me: true}
	for _, id := range memberIds {
		if seen[id] {
			return RoomView{}, netError("body/members", "本人か重なりを含む")
		}
		seen[id] = true
	}
	if (kind == KindDm && len(memberIds) != 1) || (kind == KindGroup && len(memberIds) < 2) {
		return RoomView{}, netError("body/members", "人数が種類に合わない")
	}
	// 入れる人の users の行を共有の錠で押さえ、読んだ状態で断る（新しいトークの行と RoomLock の前。停止・削除・抹消と直列にし、それらが
	// 読んだトークの和を後から増やさない。全体実装設計書 2.4、I-18）。
	users, err := r.d.Identity.Users.LockShared(ctx, tx, memberIds)
	if err != nil {
		return RoomView{}, err
	}
	for _, usr := range users {
		switch usr.State {
		case identity.UserErased:
			return RoomView{}, ev(cgen.MEM001)
		case identity.UserSuspended, identity.UserDeleted:
			return RoomView{}, ev(cgen.MEM002)
		}
	}
	id, now := r.d.Ids.NewV7(), r.d.now()
	var dmKey, creator *string
	if kind == KindDm {
		k := dmKeyOf(me, memberIds[0])
		dmKey = &k
	} else {
		c := me.String()
		creator = &c
	}
	var got uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO rooms (id, kind, creator_id, state, last_event_at, created_at, dm_key)
VALUES ($1, $2, $3, 'normal', $4, $4, $5) ON CONFLICT (dm_key) DO NOTHING RETURNING id`, id, string(kind), creator, now, dmKey).Scan(&got)
	if noRows(err) {
		var prior uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM rooms WHERE dm_key = $1`, *dmKey).Scan(&prior); err != nil {
			return RoomView{}, wrap(err)
		}
		return RoomView{}, fault.New(cgen.ROOM004, map[string]any{"room_id": prior.String()})
	}
	if err != nil {
		return RoomView{}, wrap(err)
	}
	all := sortedIds(append([]uuid.UUID{me}, memberIds...))
	for _, u := range all {
		state := MemberPendingAdd
		if u == me {
			state = MemberActive
		}
		if _, err := tx.Exec(ctx, `INSERT INTO room_members (room_id, user_id, state, since, by) VALUES ($1, $2, $3, $4, $5)`,
			id, u, string(state), now, me); err != nil {
			return RoomView{}, wrap(err)
		}
	}
	if err := r.d.Keys.Groups.Create(ctx, tx, id, device); err != nil {
		return RoomView{}, err
	}
	var by *uuid.UUID
	if kind == KindGroup {
		by = &me
	}
	if err := r.emitSettings(tx, id, kind, by, all); err != nil {
		return RoomView{}, err
	}
	for _, u := range all {
		change := changePendingAdd
		if u == me {
			change = changeAdded
		}
		if err := r.emitMember(tx, id, all, u, change, &me); err != nil {
			return RoomView{}, err
		}
	}
	if kind == KindGroup {
		if _, err := r.timeline.SystemEvent(ctx, tx, id, systemRoomCreated, map[string]any{"by": me.String()}); err != nil {
			return RoomView{}, err
		}
	}
	return r.View(ctx, tx, me, id)
}

// dmKeyOf は 1 対 1 の 2 人の識別子を小さい順に : で連ねた一意の鍵（rooms.dm_key）。
func dmKeyOf(a, b uuid.UUID) string {
	ids := sortedIds([]uuid.UUID{a, b})
	return ids[0].String() + ":" + ids[1].String()
}

// emitSettings は room.settings{kind, creator_id?} を宛先へ。
func (r *Rooms) emitSettings(tx *store.Tx, roomId uuid.UUID, kind RoomKind, creator *uuid.UUID, to []uuid.UUID) error {
	var p roomSettings
	p.Settings.Kind, p.Settings.CreatorId = kind, creator
	_, err := r.d.emit(tx, to, kindRoomSettings, roomId, r.d.Ids.NewV7(), p)
	return err
}

// emitMember は 1 人の変化の room.member を宛先へ（1 人に 1 フレーム）。
func (r *Rooms) emitMember(tx *store.Tx, roomId uuid.UUID, to []uuid.UUID, userId uuid.UUID, change string, by *uuid.UUID) error {
	_, err := r.d.emit(tx, to, kindRoomMember, roomId, r.d.Ids.NewV7(), roomMember{UserId: userId, Change: change, By: by})
	return err
}

// Get はトークの行（無いか削除済みは ROOM-001）。q は ctx のトランザクションか Pool。
func (r *Rooms) Get(ctx context.Context, q store.Querier, roomId uuid.UUID) (Room, error) {
	room, err := scanRoom(q.QueryRow(ctx, `SELECT `+roomCols+` FROM rooms WHERE id = $1`, roomId))
	if noRows(err) {
		return Room{}, ev(cgen.ROOM001)
	}
	if err != nil {
		return Room{}, wrap(err)
	}
	if room.State == RoomDeleted {
		return Room{}, ev(cgen.ROOM001)
	}
	return room, nil
}

// View は GET /rooms/{id} と Create の応答（List の 1 件と同じ形。作成のトランザクションを q に）。本人が active か pending_add で
// なければ ROOM-002（境界が先に確かめるので通常は起きない）。
func (r *Rooms) View(ctx context.Context, q store.Querier, userId, roomId uuid.UUID) (RoomView, error) {
	rows, err := r.memberRows(ctx, q, userId, `AND r.id = $2`, []any{userId, roomId})
	if err != nil {
		return RoomView{}, err
	}
	if len(rows) == 0 {
		return RoomView{}, ev(cgen.ROOM002)
	}
	views, err := r.views(ctx, q, userId, rows)
	if err != nil {
		return RoomView{}, err
	}
	return views[0], nil
}

// List は GET /rooms：本人が active か pending_add のトーク（トークの識別子の順。第2章 3 の cursor）。戻りは (要素, has_more)。
// 1 つのページの読みは 6 つの SQL 文（トーク、構成員、自分の設定、未読、pos、epoch）で、トークの数に比例しない。
// q は ctx のトランザクションか Pool（Timeline.Snapshot は Pool.BeginSnapshot のトランザクション）。
func (r *Rooms) List(ctx context.Context, q store.Querier, userId uuid.UUID, c Cursor) ([]RoomView, bool, error) {
	sql, args := whereUUID(c, ``, `r.id`, []any{userId})
	rows, err := r.memberRows(ctx, q, userId, sql, args)
	if err != nil {
		return nil, false, err
	}
	rows, more := finish(rows, c)
	views, err := r.views(ctx, q, userId, rows)
	return views, more, err
}

// listAll は Snapshot の全部のトーク（cursor なし）。
func (r *Rooms) listAll(ctx context.Context, q store.Querier, userId uuid.UUID) ([]RoomView, error) {
	rows, err := r.memberRows(ctx, q, userId, ` ORDER BY r.id`, []any{userId})
	if err != nil {
		return nil, err
	}
	return r.views(ctx, q, userId, rows)
}

// memberRow は本人から見たトークの 1 行（一覧の元）。
type memberRow struct {
	room  Room
	state MemberState
	since time.Time
}

// memberRows は本人が active か pending_add で削除済みでないトーク（tail は条件と並び）。
func (r *Rooms) memberRows(ctx context.Context, q store.Querier, userId uuid.UUID, tail string, args []any) ([]memberRow, error) {
	cols := `r.id, r.kind, r.creator_id, r.state, r.empty_since, r.last_event_at, r.created_at, r.deleted_at`
	rows, err := q.Query(ctx, `SELECT `+cols+`, m.state, m.since FROM room_members m JOIN rooms r ON r.id = m.room_id
WHERE m.user_id = $1 AND m.state IN ('active', 'pending_add') AND r.state <> 'deleted'`+tail, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []memberRow
	for rows.Next() {
		var m memberRow
		var kind, state, mstate string
		if err := rows.Scan(&m.room.Id, &kind, &m.room.Creator, &state, &m.room.EmptySince, &m.room.LastEventAt, &m.room.CreatedAt,
			&m.room.DeletedAt, &mstate, &m.since); err != nil {
			return nil, wrap(err)
		}
		m.room.Kind, m.room.State, m.state = RoomKind(kind), RoomState(state), MemberState(mstate)
		out = append(out, m)
	}
	return out, wrap(rows.Err())
}

// views は行から RoomView を組む（構成員、自分の設定、未読、pos、epoch をそれぞれ 1 つの文で）。
func (r *Rooms) views(ctx context.Context, q store.Querier, userId uuid.UUID, rows []memberRow) ([]RoomView, error) {
	out := make([]RoomView, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(rows))
	for i, m := range rows {
		ids[i] = m.room.Id
	}
	members := map[uuid.UUID][]string{}
	mr, err := q.Query(ctx, `SELECT room_id, user_id FROM room_members WHERE room_id = ANY($1) AND state = 'active' ORDER BY room_id, user_id`, ids)
	if err != nil {
		return nil, wrap(err)
	}
	for mr.Next() {
		var room, user uuid.UUID
		if err := mr.Scan(&room, &user); err != nil {
			mr.Close()
			return nil, wrap(err)
		}
		members[room] = append(members[room], user.String())
	}
	mr.Close()
	if err := mr.Err(); err != nil {
		return nil, wrap(err)
	}
	me, err := r.roomMe.getMany(ctx, q, userId, ids)
	if err != nil {
		return nil, err
	}
	unread, err := r.receipts.unreadMany(ctx, q, userId, rows)
	if err != nil {
		return nil, err
	}
	pos, err := r.d.Feed.LastRoomEventPos(ctx, q, userId, ids)
	if err != nil {
		return nil, err
	}
	epochs, err := r.d.Keys.Groups.EpochsOf(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		v := RoomView{Id: m.room.Id.String(), Kind: gen.RoomSummaryKind(m.room.Kind), Members: members[m.room.Id], Me: me[m.room.Id].toGen(),
			Unread: unread[m.room.Id], Epoch: int(epochs[m.room.Id]), CreatorId: optId(m.room.Creator)}
		if v.Members == nil {
			v.Members = []string{}
		}
		if p, ok := pos[m.room.Id]; ok {
			n := int(p)
			v.LastPos = &n
		}
		out = append(out, v)
	}
	return out, nil
}

// AdminList は /admin/rooms（srv-admin）：識別子、種類、人数（active）、空になった時刻。容量は srv-admin が srv-media から足す。
func (r *Rooms) AdminList(ctx context.Context, c Cursor) ([]AdminRoom, bool, error) {
	sql, args := whereUUID(c, `SELECT r.id, r.kind, r.empty_since, (SELECT count(*) FROM room_members m WHERE m.room_id = r.id AND m.state = 'active')
FROM rooms r WHERE r.state <> 'deleted'`, `r.id`, nil)
	rows, err := r.d.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, wrap(err)
	}
	defer rows.Close()
	var out []AdminRoom
	for rows.Next() {
		var id uuid.UUID
		var kind string
		var empty *time.Time
		var n int
		if err := rows.Scan(&id, &kind, &empty, &n); err != nil {
			return nil, false, wrap(err)
		}
		a := AdminRoom{Id: id.String(), Kind: gen.AdminRoomKind(kind), MemberCount: n}
		if empty != nil {
			t := empty.UTC()
			a.EmptySince = &t
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	items, more := finish(out, c)
	return items, more, nil
}

// CreateFamilyRoom は設置の時の家族全員のトーク（hook OnUserCreated の中。作成者は無し、管理者は active（by 無し）。Groups.Create の
// leaf 0 は管理者の端末。room.settings）。2 つ目は一意の部分索引 rooms_family の違反（設置は 1 回。Fault）。
func (r *Rooms) CreateFamilyRoom(ctx context.Context, tx *store.Tx, adminId, adminDevice uuid.UUID) (uuid.UUID, error) {
	return r.createSingle(ctx, tx, KindFamily, adminId, adminDevice)
}

// CreateSelfRoom は設置と参加の時の自分用メモ（本人だけが active。Groups.Create の leaf 0 は最初の端末。room.settings）。
func (r *Rooms) CreateSelfRoom(ctx context.Context, tx *store.Tx, userId, device uuid.UUID) (uuid.UUID, error) {
	return r.createSingle(ctx, tx, KindSelf, userId, device)
}

// createSingle は 1 人で始まるトーク（family と self）。
func (r *Rooms) createSingle(ctx context.Context, tx *store.Tx, kind RoomKind, userId, device uuid.UUID) (uuid.UUID, error) {
	id, now := r.d.Ids.NewV7(), r.d.now()
	if _, err := tx.Exec(ctx, `INSERT INTO rooms (id, kind, state, last_event_at, created_at) VALUES ($1, $2, 'normal', $3, $3)`,
		id, string(kind), now); err != nil {
		return uuid.Nil, wrap(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO room_members (room_id, user_id, state, since) VALUES ($1, $2, 'active', $3)`, id, userId, now); err != nil {
		return uuid.Nil, wrap(err)
	}
	if err := r.d.Keys.Groups.Create(ctx, tx, id, device); err != nil {
		return uuid.Nil, err
	}
	return id, r.emitSettings(tx, id, kind, nil, []uuid.UUID{userId})
}

// Delete はグループの削除（rule は room_delete か empty_group。by は要求の主体、byAdmin はその役割が管理者か）。判定は RoomLock の
// 後（判定の後に構成員が変わらない）。1 対 1・自分用メモは ROOM-003、家族全員は ROOM-011。room_delete は by が active でなければ
// ROOM-002、作成者でも管理者でもなければ ROOM-003。empty_group は active・pending_add・pending_remove の構成員が 1 人でもいれば
// ROOM-003。処理は drop（第4章 4.1、第12章 9）。
func (r *Rooms) Delete(ctx context.Context, tx *store.Tx, roomId uuid.UUID, rule DeleteRule, by uuid.UUID, byAdmin bool) error {
	room, err := r.d.lockRoom(ctx, tx, roomId)
	if err != nil {
		return err
	}
	switch room.Kind {
	case KindFamily:
		return ev(cgen.ROOM011)
	case KindDm, KindSelf:
		return ev(cgen.ROOM003)
	}
	switch rule {
	case RuleRoomDelete:
		st, err := r.members.stateOf(ctx, tx, roomId, by, false)
		if err != nil {
			return err
		}
		if st != MemberActive {
			return ev(cgen.ROOM002)
		}
		if (room.Creator == nil || *room.Creator != by) && !byAdmin {
			return ev(cgen.ROOM003)
		}
	case RuleEmptyGroup:
		n, err := r.members.countLive(ctx, tx, roomId)
		if err != nil {
			return err
		}
		if n > 0 {
			return ev(cgen.ROOM003)
		}
	default:
		return fault.Wrap(errors.New("rooms: 削除の規則が列挙の外"))
	}
	b := by
	return r.drop(ctx, tx, room, &b)
}

// drop はトークの削除の本体（Delete と Gc。判定は呼ぶ側。RoomLock は取り済み）。次の順に行う。1. srv-keys Groups.Drop（Remove の
// Commit は待たない）。2. active・pending_add・pending_remove の構成員ごとに room.member{removed, by} を全員へ流し、行を removed に。
// 3. イベントを全部削除済みの印に（1 つの UPDATE と MediaBinder.UnbindRoom。取消のイベントは出さない：受け手が残らない。I-2）。
// 4. pending の予約送信を cancelled（Jobs.Cancel、room.scheduled{cancelled} を本人へ。添付は 3 の UnbindRoom で外れている）。
// 5. CallEnder.EndAll。6. state = deleted、deleted_at、creator_id と empty_since を外す。
func (r *Rooms) drop(ctx context.Context, tx *store.Tx, room Room, by *uuid.UUID) error {
	if err := r.d.Keys.Groups.Drop(ctx, tx, room.Id); err != nil {
		return err
	}
	now := r.d.now()
	live, err := r.members.liveRows(ctx, tx, room.Id)
	if err != nil {
		return err
	}
	ids := memberIds(live)
	for _, m := range live {
		if err := r.emitMember(tx, room.Id, ids, m.UserId, changeRemoved, by); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE room_members SET state = 'removed', since = $2, by = $3
WHERE room_id = $1 AND state IN ('active', 'pending_add', 'pending_remove')`, room.Id, now, by); err != nil {
		return wrap(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE events SET ciphertext = NULL, redacted_at = $2, redacted_by = $3 WHERE room_id = $1 AND redacted_at IS NULL`,
		room.Id, now, by); err != nil {
		return wrap(err)
	}
	if err := r.ports.media.UnbindRoom(ctx, tx, room.Id); err != nil {
		return err
	}
	if err := r.scheduled.cancelAllOf(ctx, tx, room.Id); err != nil {
		return err
	}
	if err := r.ports.calls.EndAll(ctx, tx, room.Id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE rooms SET state = 'deleted', deleted_at = $2, creator_id = NULL, empty_since = NULL WHERE id = $1`, room.Id, now)
	return wrap(err)
}

// MarkEmptyIfNoMembers は active・pending_add・pending_remove の構成員が残らない normal のグループを state = empty、empty_since = now
// にする（Members.Remove の 7。30 日の後に Gc.DeleteEmptyGroups が削除済みに）。他の種類は何もしない。
func (r *Rooms) MarkEmptyIfNoMembers(ctx context.Context, tx *store.Tx, roomId uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE rooms SET state = 'empty', empty_since = $2 WHERE id = $1 AND kind = 'group' AND state = 'normal'
  AND NOT EXISTS (SELECT 1 FROM room_members WHERE room_id = $1 AND state IN ('active', 'pending_add', 'pending_remove'))`, roomId, r.d.now())
	return wrap(err)
}

// Register は srv-identity の hook の 7 つに本ブロックの処理を登録する（組み立ての中で 1 回）。
func (r *Rooms) Register(id *identity.Identity) {
	h := id.Hooks
	h.RegisterOnBeforeUserChange(r.onBeforeUserChange)
	h.RegisterOnBeforeDeviceRevoke(r.onBeforeDeviceRevoke)
	h.RegisterOnUserCreated(r.onUserCreated)
	h.RegisterOnUserSuspended(r.onUserSuspended)
	h.RegisterOnUserUnsuspended(r.onUserUnsuspended)
	h.RegisterOnUserDeleted(r.onUserDeleted)
	h.RegisterOnUserErased(r.onUserErased)
}

// LockAll は複数のトークの RoomLock を識別子の順に取る（重なりを除く。行の無いトークは飛ばす。削除済みは取る）。1 つのトランザクション
// で 2 つ以上のトークの錠を取る処理の最初の 1 回（全体実装設計書 2.4、I-18）。本ブロックの hook OnBeforeUserChange・
// OnBeforeDeviceRevoke と、srv-admin の空のグループの削除（消すトークと家族全員のトーク）が呼ぶ。
func (r *Rooms) LockAll(ctx context.Context, tx *store.Tx, roomIds []uuid.UUID) error {
	for _, id := range sortedIds(roomIds) {
		if _, _, err := r.d.lockRoomAny(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// onBeforeUserChange は hook OnBeforeUserChange（停止・削除・抹消の始め。srv-identity が users、devices、sessions の行の錠の後、他の
// hook の前に呼ぶ）：そのトランザクションが触れ得る全部のトークの和を識別子の順に取る。和は本人が active か pending_add のトーク
// （外す対象）、本人の端末が leaf か待ちを持つトーク（srv-keys Groups.RoomsOfDevices。srv-keys の hook が取る集合）、本人が送った
// ft.event.mls/1 で削除済みの印でない物のあるトーク（一括の取消。退出済みを含む）、家族全員のトーク（停止・削除・抹消の知らせと
// 監査の記録）。以後の srv-keys・本ブロック・srv-push・srv-admin の RoomLock は全部この和の中で、2 度目なので待たない。和は増えない：
// 構成員の追加とトークの作成は Users.LockShared、端末の leaf と待ちは Devices.LockShared で直列、送信は構成員のトークだけ
// （全体実装設計書 2.4、I-18）。
func (r *Rooms) onBeforeUserChange(ctx context.Context, tx *store.Tx, userId uuid.UUID, deviceIds []uuid.UUID) error {
	ids, err := r.members.RoomsOf(ctx, tx, userId)
	if err != nil {
		return err
	}
	leafs, err := r.d.Keys.Groups.RoomsOfDevices(ctx, tx, deviceIds)
	if err != nil {
		return err
	}
	sent, err := scanUuids(ctx, tx, `SELECT DISTINCT room_id FROM events WHERE sender_id = $1 AND schema = 'ft.event.mls/1' AND redacted_at IS NULL`, userId)
	if err != nil {
		return err
	}
	ids = append(append(ids, leafs...), sent...)
	if ids, err = r.withFamily(ctx, tx, ids); err != nil {
		return err
	}
	return r.LockAll(ctx, tx, ids)
}

// onBeforeDeviceRevoke は hook OnBeforeDeviceRevoke（端末の取り消しの始め。devices と sessions の行の錠の後、OnDeviceRevoked の前）：
// その端末が leaf か待ちを持つトーク（srv-keys Groups.RoomsOfDevices）と家族全員のトークの和を識別子の順に取る。家族全員のトークは
// 管理者の取り消しの後の監査の記録が書く（本人の取り消しでも取る）。抹消の中の端末ごとの呼び出しは OnBeforeUserChange の和の中で
// 待たない（全体実装設計書 2.4、I-18）。
func (r *Rooms) onBeforeDeviceRevoke(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	ids, err := r.d.Keys.Groups.RoomsOfDevices(ctx, tx, []uuid.UUID{deviceId})
	if err != nil {
		return err
	}
	if ids, err = r.withFamily(ctx, tx, ids); err != nil {
		return err
	}
	return r.LockAll(ctx, tx, ids)
}

// withFamily は ids に家族全員のトークを足す（設置の前で無ければ足さない）。
func (r *Rooms) withFamily(ctx context.Context, tx *store.Tx, ids []uuid.UUID) ([]uuid.UUID, error) {
	fam, err := r.family.Id(ctx, tx)
	switch {
	case isEvent(err, cgen.ROOM001):
		return ids, nil
	case err != nil:
		return nil, err
	}
	return append(ids, fam), nil
}

// onUserCreated は hook OnUserCreated：家族全員のトークが無ければ CreateFamilyRoom（設置）、あれば Members.Add（参加。by 無し）。
// どちらも CreateSelfRoom。参加の知らせ（member_added）は Commit の後の Resolve が出す。
func (r *Rooms) onUserCreated(ctx context.Context, tx *store.Tx, userId, firstDeviceId uuid.UUID) error {
	fam, err := r.family.Id(ctx, tx)
	switch {
	case isEvent(err, cgen.ROOM001):
		if _, err := r.CreateFamilyRoom(ctx, tx, userId, firstDeviceId); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := r.members.Add(ctx, tx, fam, nil, userId); err != nil {
			return err
		}
	}
	_, err = r.CreateSelfRoom(ctx, tx, userId, firstDeviceId)
	return err
}

// onUserSuspended は hook OnUserSuspended：reason が suspended の時だけ家族全員のトークに member_suspended（削除は停止の処理を通り、
// その時の reason は deleted で流さない）。
func (r *Rooms) onUserSuspended(ctx context.Context, tx *store.Tx, userId uuid.UUID, reason identity.RevokeReason) error {
	if reason != identity.RevokeSuspended {
		return nil
	}
	return r.family.Flow(ctx, tx, systemMemberSuspended, map[string]any{"user_id": userId.String()})
}

// onUserUnsuspended は hook OnUserUnsuspended：家族全員のトークに member_unsuspended。
func (r *Rooms) onUserUnsuspended(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	return r.family.Flow(ctx, tx, systemMemberUnsuspended, map[string]any{"user_id": userId.String()})
}

// onUserDeleted は hook OnUserDeleted：本人が active か pending_add のトーク（家族全員を含む）をトークの識別子の順に外す（サーバーの
// 処理なので by は無く、left にしない。Members.removeByServer）。eraseContent なら本人の送ったイベントを一括の取消（Timeline.redactAllOf）。
// 最後に家族全員のトークに member_deleted（外れた本人には届かない）。自分用メモは他の構成員がいないのでグループの記録が消え、
// トークの行は残る（本文は既定で残す。第12章 9）。トークの錠は hook OnBeforeUserChange で和として取り済み（全体実装設計書 2.4、I-18）。
func (r *Rooms) onUserDeleted(ctx context.Context, tx *store.Tx, userId uuid.UUID, eraseContent bool) error {
	if err := r.removeEverywhere(ctx, tx, userId); err != nil {
		return err
	}
	if eraseContent {
		if err := r.timeline.redactAllOf(ctx, tx, userId); err != nil {
			return err
		}
	}
	return r.family.Flow(ctx, tx, systemMemberDeleted, map[string]any{"user_id": userId.String()})
}

// onUserErased は hook OnUserErased：本人の送ったイベントを一括の取消、まだ外れていないトークの構成員の行を外し、家族全員のトークに
// member_erased（第12章 9）。トークの錠は hook OnBeforeUserChange で和として取り済み（全体実装設計書 2.4、I-18）。
func (r *Rooms) onUserErased(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	if err := r.timeline.redactAllOf(ctx, tx, userId); err != nil {
		return err
	}
	if err := r.removeEverywhere(ctx, tx, userId); err != nil {
		return err
	}
	return r.family.Flow(ctx, tx, systemMemberErased, map[string]any{"user_id": userId.String()})
}

// removeEverywhere は本人が active か pending_add の全部のトークから、トークの識別子の順に外す（hook）。
func (r *Rooms) removeEverywhere(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	rooms, err := r.members.RoomsOf(ctx, tx, userId)
	if err != nil {
		return err
	}
	for _, room := range rooms {
		if err := r.members.removeByServer(ctx, tx, room, userId); err != nil {
			return err
		}
	}
	return nil
}

// blank は無いか空白だけの文字列か。
func blank(s *string) bool {
	return s == nil || strings.TrimSpace(*s) == ""
}
