// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// MemberState は構成員の状態（room_members.state。第4章 4.1）。api の Member.state の member は active に当たる。
type MemberState string

// 状態の 5 つ。
const (
	MemberPendingAdd    MemberState = "pending_add"
	MemberActive        MemberState = "active"
	MemberPendingRemove MemberState = "pending_remove"
	MemberRemoved       MemberState = "removed"
	MemberLeft          MemberState = "left"
)

// Member は構成員の 1 行（room_members）。
type Member struct {
	UserId uuid.UUID
	State  MemberState
	Since  time.Time
	// By は最後に状態を変えた人（家族全員の参加とサーバーの処理は nil）。
	By *uuid.UUID
}

// PendingMember は残っている構成員の追加待ち（srv-worker monitor が Since で 7 日を判定する）。
type PendingMember struct {
	RoomId uuid.UUID
	UserId uuid.UUID
	Since  time.Time
}

// Members は構成員の変化と読み（B-101）。srv-boundary の RoomMembership と srv-keys の RoomReader の中身。
type Members struct {
	d        *Deps
	rooms    *Rooms
	timeline *Timeline
}

// Add は POST /rooms/{id}/members と家族全員の参加の hook（by が nil は hook だけ）。最初に srv-identity Users.LockShared で入れる人の
// users の行を共有の錠で押さえ（RoomLock より先。停止・削除・抹消と直列にし、読んだ状態で断る。全体実装設計書 2.4、I-18）、次に
// RoomLock。グループだけ（1 対 1・自分用メモは
// ROOM-003。家族全員は参加の hook だけが by 無しで足し、by があれば ROOM-003）。by が active でなければ ROOM-002。人が無いか抹消済みは
// MEM-001、停止中か削除済みは MEM-002。既に active・pending_add・pending_remove なら今の行を返して何も変えない（pending_remove を
// 取り下げない。第4章 4.1）。無いか removed・left なら pending_add（since now、by）。room.member(pending_add) を Audience（本人を含む）へ。
func (m *Members) Add(ctx context.Context, tx *store.Tx, roomId uuid.UUID, by *uuid.UUID, userId uuid.UUID) (Member, error) {
	users, err := m.d.Identity.Users.LockShared(ctx, tx, []uuid.UUID{userId})
	if err != nil {
		return Member{}, err
	}
	usr := users[0]
	room, err := m.d.lockRoom(ctx, tx, roomId)
	if err != nil {
		return Member{}, err
	}
	switch {
	case room.Kind == KindDm || room.Kind == KindSelf:
		return Member{}, ev(cgen.ROOM003)
	case room.Kind == KindFamily && by != nil:
		return Member{}, ev(cgen.ROOM003)
	case room.Kind == KindGroup && by == nil:
		return Member{}, fault.Wrap(errors.New("rooms: グループの追加に操作した人が無い"))
	}
	if by != nil {
		st, err := m.stateOf(ctx, tx, roomId, *by, false)
		if err != nil {
			return Member{}, err
		}
		if st != MemberActive {
			return Member{}, ev(cgen.ROOM002)
		}
	}
	switch usr.State {
	case identity.UserErased:
		return Member{}, ev(cgen.MEM001)
	case identity.UserSuspended, identity.UserDeleted:
		return Member{}, ev(cgen.MEM002)
	}
	cur, ok, err := m.row(ctx, tx, roomId, userId, true)
	if err != nil {
		return Member{}, err
	}
	if ok && (cur.State == MemberActive || cur.State == MemberPendingAdd || cur.State == MemberPendingRemove) {
		return cur, nil
	}
	audience, err := m.Audience(ctx, tx, roomId)
	if err != nil {
		return Member{}, err
	}
	row := Member{UserId: userId, State: MemberPendingAdd, Since: m.d.now(), By: by}
	if _, err := tx.Exec(ctx, `INSERT INTO room_members (room_id, user_id, state, since, by) VALUES ($1, $2, 'pending_add', $3, $4)
ON CONFLICT (room_id, user_id) DO UPDATE SET state = 'pending_add', since = EXCLUDED.since, by = EXCLUDED.by`, roomId, userId, row.Since, by); err != nil {
		return Member{}, wrap(err)
	}
	return row, m.rooms.emitMember(tx, roomId, append(audience, userId), userId, changePendingAdd, by)
}

// Remove は DELETE /rooms/{id}/members/{uid}（外す・退出。規則 room_member_remove：by = userId の退出、by が作成者、byAdmin）。判定は
// RoomLock の後で、by が active でなければ ROOM-002、規則に当たらなければ ROOM-003。家族全員は ROOM-011、1 対 1 と自分用メモは
// ROOM-003。相手が pending_remove なら何もしない。removed・left・行なしは MEM-001。外し方は外す処理（detach）。
func (m *Members) Remove(ctx context.Context, tx *store.Tx, roomId, by uuid.UUID, byAdmin bool, userId uuid.UUID) error {
	room, err := m.d.lockRoom(ctx, tx, roomId)
	if err != nil {
		return err
	}
	switch room.Kind {
	case KindFamily:
		return ev(cgen.ROOM011)
	case KindDm, KindSelf:
		return ev(cgen.ROOM003)
	}
	st, err := m.stateOf(ctx, tx, roomId, by, false)
	if err != nil {
		return err
	}
	if st != MemberActive {
		return ev(cgen.ROOM002)
	}
	if by != userId && (room.Creator == nil || *room.Creator != by) && !byAdmin {
		return ev(cgen.ROOM003)
	}
	cur, ok, err := m.row(ctx, tx, roomId, userId, true)
	if err != nil {
		return err
	}
	if ok && cur.State == MemberPendingRemove {
		return nil
	}
	if !ok || cur.State == MemberRemoved || cur.State == MemberLeft {
		return ev(cgen.MEM001)
	}
	b := by
	return m.detach(ctx, tx, room, cur, &b, by == userId)
}

// removeByServer はサーバーの外し（削除・抹消の hook。トークの種類を問わない。by は無く、left にしない）。本人が active か
// pending_add でなければ何もしない。
func (m *Members) removeByServer(ctx context.Context, tx *store.Tx, roomId, userId uuid.UUID) error {
	room, err := m.d.lockRoom(ctx, tx, roomId)
	if err != nil {
		return err
	}
	cur, ok, err := m.row(ctx, tx, roomId, userId, true)
	if err != nil || !ok || (cur.State != MemberActive && cur.State != MemberPendingAdd) {
		return err
	}
	return m.detach(ctx, tx, room, cur, nil, false)
}

// detach は外す処理の本体（Remove と removeByServer。RoomLock と行の錠は取り済み）。次の順に行う（第4章 4.1、第12章 5.2）。
// 1. 宛先 = Audience（変える前）。srv-keys Groups.RequestRemoveUser（leaf を持つ端末の数 n。端末の待ちの room.member は srv-keys が流す）。
// 2. n が 0 なら removed（本人の退出なら left）と room.member、前の状態が active のグループなら member_removed か member_left。
// n が 1 以上なら pending_remove と room.member{pending_remove, by}（Remove の Commit は他の構成員の端末が行い、Resolve が外す）。
// 3. 作成者なら creator_id を外し room.settings。4. active の構成員が残らなければ settle（Commit を作れる端末が無い）。
func (m *Members) detach(ctx context.Context, tx *store.Tx, room Room, cur Member, by *uuid.UUID, leave bool) error {
	audience, err := m.Audience(ctx, tx, room.Id)
	if err != nil {
		return err
	}
	n, err := m.d.Keys.Groups.RequestRemoveUser(ctx, tx, room.Id, cur.UserId)
	if err != nil {
		return err
	}
	now := m.d.now()
	if n == 0 {
		if err := m.finishRemove(ctx, tx, room, cur.UserId, by, leave, cur.State == MemberActive, audience, now); err != nil {
			return err
		}
	} else {
		if err := m.setState(ctx, tx, room.Id, cur.UserId, MemberPendingRemove, by, now); err != nil {
			return err
		}
		if err := m.rooms.emitMember(tx, room.Id, audience, cur.UserId, changePendingRemove, by); err != nil {
			return err
		}
	}
	if room.Creator != nil && *room.Creator == cur.UserId {
		if _, err := tx.Exec(ctx, `UPDATE rooms SET creator_id = NULL WHERE id = $1`, room.Id); err != nil {
			return wrap(err)
		}
		if err := m.rooms.emitSettings(tx, room.Id, room.Kind, nil, audience); err != nil {
			return err
		}
	}
	return m.settle(ctx, tx, room)
}

// finishRemove は 1 人を removed か left にし、room.member と、前の状態が active のグループなら知らせ（member_left か member_removed）。
func (m *Members) finishRemove(ctx context.Context, tx *store.Tx, room Room, userId uuid.UUID, by *uuid.UUID, leave, wasActive bool,
	to []uuid.UUID, now time.Time) error {
	state, change, sys := MemberRemoved, changeRemoved, systemMemberRemoved
	if leave {
		state, change, sys = MemberLeft, changeLeft, systemMemberLeft
	}
	if err := m.setState(ctx, tx, room.Id, userId, state, by, now); err != nil {
		return err
	}
	if err := m.rooms.emitMember(tx, room.Id, append(to, userId), userId, change, by); err != nil {
		return err
	}
	if !wasActive || room.Kind != KindGroup {
		return nil
	}
	params := map[string]any{"user_id": userId.String()}
	if !leave && by != nil {
		params["by"] = by.String()
	}
	_, err := m.timeline.SystemEvent(ctx, tx, room.Id, sys, params)
	return err
}

// settle は active の構成員が残らないトークを片づける（Commit を作れる端末が無い）。srv-keys Groups.Drop、pending_remove の人を
// removed か left（行の by が本人なら left）、pending_add の人を removed にして room.member を流し（受け手が残らないので知らせは
// 出さない）、MarkEmptyIfNoMembers（グループだけ空に）。active が残れば何もしない。
func (m *Members) settle(ctx context.Context, tx *store.Tx, room Room) error {
	active, err := m.Of(ctx, tx, room.Id)
	if err != nil || len(active) > 0 {
		return err
	}
	if err := m.d.Keys.Groups.Drop(ctx, tx, room.Id); err != nil {
		return err
	}
	rest, err := m.liveRows(ctx, tx, room.Id)
	if err != nil {
		return err
	}
	ids := memberIds(rest)
	now := m.d.now()
	for _, r := range rest {
		state, change := MemberRemoved, changeRemoved
		if r.State == MemberPendingRemove && r.By != nil && *r.By == r.UserId {
			state, change = MemberLeft, changeLeft
		}
		if err := m.setState(ctx, tx, room.Id, r.UserId, state, r.By, now); err != nil {
			return err
		}
		if err := m.rooms.emitMember(tx, room.Id, ids, r.UserId, change, r.By); err != nil {
			return err
		}
	}
	return m.rooms.MarkEmptyIfNoMembers(ctx, tx, room.Id)
}

// Resolve は /mls/commit の受けが Groups.Commit の後に（Groups.Commit が返した端末で）、/mls/reinit の受けが Groups.Reinit の後に
// （空の引数で）、同じトランザクションで呼ぶ。pending_add の人はその人の端末が 1 つでも leaf を得たら active（since now、by は
// NULL。room.member added（by は行の by）、グループと家族全員は member_added）。pending_remove の人は Groups.Leaves で残りの leaf を
// 読み（端末の申告ではなく記録。再初期化は空の引数）、その人の端末の leaf が無くなったら removed か left（行の by が本人なら left）。
// room.member を Audience と本人へ。グループは member_left か member_removed（家族全員は member_deleted が出るので出さない）。
func (m *Members) Resolve(ctx context.Context, tx *store.Tx, roomId uuid.UUID, addedDevices, removedDevices []uuid.UUID) error {
	pending, err := m.rowsWhere(ctx, tx, `room_id = $1 AND state IN ('pending_add', 'pending_remove') ORDER BY user_id FOR UPDATE`, roomId)
	if err != nil || len(pending) == 0 {
		return err
	}
	room, err := m.rooms.Get(ctx, tx, roomId)
	if err != nil {
		return err
	}
	owners := map[uuid.UUID]bool{}
	for _, d := range addedDevices {
		dv, err := m.d.Identity.Devices.Get(ctx, tx, d)
		if err != nil {
			return err
		}
		owners[dv.UserId] = true
	}
	leaves, err := m.d.Keys.Groups.Leaves(ctx, tx, roomId)
	if err != nil {
		return err
	}
	inTree := map[uuid.UUID]bool{}
	for _, l := range leaves {
		inTree[l.DeviceId] = true
	}
	audience, err := m.Audience(ctx, tx, roomId)
	if err != nil {
		return err
	}
	now := m.d.now()
	for _, r := range pending {
		switch r.State {
		case MemberPendingAdd:
			if !owners[r.UserId] {
				continue
			}
			if err := m.setState(ctx, tx, roomId, r.UserId, MemberActive, nil, now); err != nil {
				return err
			}
			if err := m.rooms.emitMember(tx, roomId, audience, r.UserId, changeAdded, r.By); err != nil {
				return err
			}
			if room.Kind == KindGroup || room.Kind == KindFamily {
				params := map[string]any{"user_id": r.UserId.String()}
				if r.By != nil {
					params["by"] = r.By.String()
				}
				if _, err := m.timeline.SystemEvent(ctx, tx, roomId, systemMemberAdded, params); err != nil {
					return err
				}
			}
		case MemberPendingRemove:
			devs, err := m.d.Identity.Devices.OfUser(ctx, tx, r.UserId)
			if err != nil {
				return err
			}
			left := false
			for _, d := range devs {
				if inTree[d.Id] {
					left = true
					break
				}
			}
			if left {
				continue
			}
			leave := r.By != nil && *r.By == r.UserId
			wasGroup := room.Kind == KindGroup
			if err := m.finishRemove(ctx, tx, room, r.UserId, r.By, leave, wasGroup, audience, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// row は 1 人の構成員の行（lock なら FOR UPDATE）。無ければ偽。
func (m *Members) row(ctx context.Context, q store.Querier, roomId, userId uuid.UUID, lock bool) (Member, bool, error) {
	sql := `room_id = $1 AND user_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := m.rowsWhere(ctx, q, sql, roomId, userId)
	if err != nil || len(rows) == 0 {
		return Member{}, false, err
	}
	return rows[0], true, nil
}

// stateOf は 1 人の状態（行が無ければ空）。
func (m *Members) stateOf(ctx context.Context, q store.Querier, roomId, userId uuid.UUID, lock bool) (MemberState, error) {
	r, ok, err := m.row(ctx, q, roomId, userId, lock)
	if err != nil || !ok {
		return "", err
	}
	return r.State, nil
}

// liveRows は active・pending_add・pending_remove の行（利用者の識別子の順。行の錠）。
func (m *Members) liveRows(ctx context.Context, tx *store.Tx, roomId uuid.UUID) ([]Member, error) {
	return m.rowsWhere(ctx, tx, `room_id = $1 AND state IN ('active', 'pending_add', 'pending_remove') ORDER BY user_id FOR UPDATE`, roomId)
}

// countLive は active・pending_add・pending_remove の人数。
func (m *Members) countLive(ctx context.Context, q store.Querier, roomId uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM room_members WHERE room_id = $1 AND state IN ('active', 'pending_add', 'pending_remove')`, roomId).Scan(&n)
	return n, wrap(err)
}

// setState は 1 人の状態を変える（since は now、by は変えた人）。
func (m *Members) setState(ctx context.Context, tx *store.Tx, roomId, userId uuid.UUID, state MemberState, by *uuid.UUID, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE room_members SET state = $3, since = $4, by = $5 WHERE room_id = $1 AND user_id = $2`,
		roomId, userId, string(state), now, by)
	return wrap(err)
}

// rowsWhere は条件の行（where は room_members の WHERE の後ろ）。
func (m *Members) rowsWhere(ctx context.Context, q store.Querier, where string, args ...any) ([]Member, error) {
	rows, err := q.Query(ctx, `SELECT user_id, state, since, by FROM room_members WHERE `+where, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var r Member
		var st string
		if err := rows.Scan(&r.UserId, &st, &r.Since, &r.By); err != nil {
			return nil, wrap(err)
		}
		r.State = MemberState(st)
		out = append(out, r)
	}
	return out, wrap(rows.Err())
}

// IsMember は現在の構成員か（active だけで削除済みでないトーク。srv-boundary RoomMembership。退出・外しの後は過去のイベントも取れない：
// 第2章 4.3）。
func (m *Members) IsMember(ctx context.Context, userId, roomId uuid.UUID) (bool, error) {
	role, err := m.RoomRole(ctx, userId, roomId)
	return role != boundary.RoomRoleNone, err
}

// RoomRole はトークの中の役割（srv-boundary RoomMembership）。active で rooms.creator_id が本人なら creator、active なら member、他は空。
func (m *Members) RoomRole(ctx context.Context, userId, roomId uuid.UUID) (boundary.RoomRole, error) {
	var creator *uuid.UUID
	err := m.d.Pool.QueryRow(ctx, `SELECT r.creator_id FROM room_members m JOIN rooms r ON r.id = m.room_id
WHERE m.room_id = $1 AND m.user_id = $2 AND m.state = 'active' AND r.state <> 'deleted'`, roomId, userId).Scan(&creator)
	if noRows(err) {
		return boundary.RoomRoleNone, nil
	}
	if err != nil {
		return boundary.RoomRoleNone, wrap(err)
	}
	if creator != nil && *creator == userId {
		return boundary.RoomRoleCreator, nil
	}
	return boundary.RoomRoleMember, nil
}

// IsMemberOrPending は active か pending_add の構成員か（srv-keys RoomReader。Commit の Add の端末の本人の確かめ）。
func (m *Members) IsMemberOrPending(ctx context.Context, q store.Querier, roomId, userId uuid.UUID) (bool, error) {
	st, err := m.stateOf(ctx, q, roomId, userId, false)
	return st == MemberActive || st == MemberPendingAdd, err
}

// Of は active の構成員（利用者の識別子の順。room.event の宛先、srv-push の通知の受け手、srv-calls の参加の資格、RoomView の members）。
func (m *Members) Of(ctx context.Context, q store.Querier, roomId uuid.UUID) ([]Member, error) {
	return m.rowsWhere(ctx, q, `room_id = $1 AND state = 'active' ORDER BY user_id`, roomId)
}

// Audience は active、pending_add、pending_remove の構成員（room.member・room.settings の宛先。srv-keys RoomReader.MembersOf）。
func (m *Members) Audience(ctx context.Context, q store.Querier, roomId uuid.UUID) ([]uuid.UUID, error) {
	rows, err := m.rowsWhere(ctx, q, `room_id = $1 AND state IN ('active', 'pending_add', 'pending_remove') ORDER BY user_id`, roomId)
	return memberIds(rows), err
}

// RoomsOf は本人が active か pending_add で削除済みでないトーク（識別子の順。srv-keys RoomReader.RoomsOf。端末の追加待ちを立てる先）。
func (m *Members) RoomsOf(ctx context.Context, q store.Querier, userId uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `SELECT m.room_id FROM room_members m JOIN rooms r ON r.id = m.room_id
WHERE m.user_id = $1 AND m.state IN ('active', 'pending_add') AND r.state <> 'deleted' ORDER BY m.room_id`, userId)
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

// List は GET /rooms/{id}/members：active、pending_add、pending_remove（利用者の識別子の順。第2章 3 の cursor）。戻りは (要素, has_more)。
func (m *Members) List(ctx context.Context, roomId uuid.UUID, c Cursor) ([]Member, bool, error) {
	sql, args := whereUUID(c, `room_id = $1 AND state IN ('active', 'pending_add', 'pending_remove')`, `user_id`, []any{roomId})
	rows, err := m.rowsWhere(ctx, m.d.Pool, sql, args...)
	if err != nil {
		return nil, false, err
	}
	items, more := finish(rows, c)
	return items, more, nil
}

// PendingAddUsers は GET /rooms/{id}/mls/epoch の pending_add（構成員の追加待ち。利用者の識別子の順）。
func (m *Members) PendingAddUsers(ctx context.Context, q store.Querier, roomId uuid.UUID) ([]uuid.UUID, error) {
	rows, err := m.rowsWhere(ctx, q, `room_id = $1 AND state = 'pending_add' ORDER BY user_id`, roomId)
	return memberIds(rows), err
}

// PendingAdds は削除済みでないトークに残っている構成員の追加待ちと立てた時刻（立てた時刻の順。srv-worker monitor が 7 日を判定し、
// member_add_stalled を出す。第7章 9）。
func (m *Members) PendingAdds(ctx context.Context) ([]PendingMember, error) {
	rows, err := m.d.Pool.Query(ctx, `SELECT m.room_id, m.user_id, m.since FROM room_members m JOIN rooms r ON r.id = m.room_id
WHERE m.state = 'pending_add' AND r.state <> 'deleted' ORDER BY m.since, m.room_id, m.user_id`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []PendingMember
	for rows.Next() {
		var p PendingMember
		if err := rows.Scan(&p.RoomId, &p.UserId, &p.Since); err != nil {
			return nil, wrap(err)
		}
		out = append(out, p)
	}
	return out, wrap(rows.Err())
}
