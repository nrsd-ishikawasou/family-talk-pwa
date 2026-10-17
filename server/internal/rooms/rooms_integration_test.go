// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package rooms

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// family3 は管理者と 2 人の構成員（全員が家族全員のトークの active）。
func family3(t *testing.T) (*env, *member, *member, *member) {
	t.Helper()
	t.Parallel()
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	e.joinFamily(b)
	e.joinFamily(c)
	return e, a, b, c
}

// create は Rooms.Create の結果。
func (e *env) create(by *member, kind RoomKind, others ...*member) (RoomView, error) {
	ids := make([]uuid.UUID, len(others))
	for i, m := range others {
		ids[i] = m.id
	}
	var v RoomView
	err := e.tx(func(tx *store.Tx) error {
		var err error
		v, err = e.b.Rooms.Create(e.ctx, tx, by.p(), kind, ids)
		return err
	})
	return v, err
}

// R: R-4-1-1
// 参加の hook（第4章 4.1、6）：最初の人で家族全員のトークを作り（作成者なし、管理者が active、leaf 0 は管理者の端末）、次の人は家族全員の
// 追加待ちで Commit の後に active と member_added。どちらも自分用メモ（本人だけが active、leaf 0 は最初の端末）。家族全員は 1 つ。
func TestOnUserCreatedFamilyAndSelf(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	fam := e.family()
	if e.members(fam)[a.id] != MemberActive || e.count(`SELECT count(*) FROM rooms WHERE kind = 'family'`) != 1 {
		t.Fatal("家族全員のトークの作成")
	}
	if e.count(`SELECT count(*) FROM mls_leaves WHERE room_id = $1 AND device_id = $2 AND leaf_index = 0`, fam, a.dev) != 1 {
		t.Fatal("家族全員のトークの leaf 0 が管理者の端末でない")
	}
	b := e.member("太郎", boundary.RoleMember)
	if e.members(fam)[b.id] != MemberPendingAdd || slices.Contains(e.systemKinds(fam), systemMemberAdded) {
		t.Fatal("参加は家族全員の追加待ちで、知らせは Commit の後")
	}
	e.joinFamily(b)
	if e.members(fam)[b.id] != MemberActive || !slices.Equal(e.systemKinds(fam), []string{systemMemberAdded}) {
		t.Fatalf("Commit の後に active と member_added: %v", e.systemKinds(fam))
	}
	for _, m := range []*member{a, b} {
		s := e.selfRoom(m)
		if ms := e.members(s); len(ms) != 1 || ms[m.id] != MemberActive {
			t.Fatalf("自分用メモの構成員: %v", ms)
		}
		if e.count(`SELECT count(*) FROM mls_leaves WHERE room_id = $1 AND device_id = $2 AND leaf_index = 0`, s, m.dev) != 1 {
			t.Fatal("自分用メモの leaf 0 が最初の端末でない")
		}
	}
	// 家族全員のトークは参加の hook だけが足す。家族全員からの退出は ROOM-011。
	e.wantCode(e.tx(func(tx *store.Tx) error { by := a.id; _, err := e.b.Members.Add(e.ctx, tx, fam, &by, b.id); return err }), cgen.ROOM003)
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, fam, b.id, false, b.id) }), cgen.ROOM011)
}

// R: R-4-1-2
// トークの作成の規則（第4章 4.1、DD-4-1、第2章 11）：1 対 1 は同じ 2 人で 1 つ（どちらの向きでも ROOM-004、params.room_id は先の
// トーク）、同時の 2 つの作成は一方だけ通る。グループは 2 人以上、本人と重なりは NET-001。無い人は MEM-001、停止中は MEM-002。
// 作成者は active、他は追加待ち。room.settings と room.member を全員へ、グループは room_created。
func TestCreateRules(t *testing.T) {
	e, a, b, c := family3(t)
	v, err := e.create(a, KindDm, b)
	e.must(err)
	for _, by := range [][2]*member{{a, b}, {b, a}} {
		_, err := e.create(by[0], KindDm, by[1])
		if ev := e.wantCode(err, cgen.ROOM004); ev.Params["room_id"] != v.Id {
			t.Fatalf("ROOM-004 の params.room_id: %v", ev.Params)
		}
	}
	errs := make(chan error, 2)
	for _, by := range [][2]*member{{b, c}, {c, b}} {
		go func() { _, err := e.create(by[0], KindDm, by[1]); errs <- err }()
	}
	got := []error{<-errs, <-errs}
	okN := 0
	for _, err := range got {
		if err == nil {
			okN++
		} else {
			e.wantCode(err, cgen.ROOM004)
		}
	}
	if okN != 1 {
		t.Fatalf("同時の 2 つの 1 対 1 の作成: %v", got)
	}
	_, err = e.create(a, KindGroup, b)
	e.wantCode(err, cgen.NET001)
	_, err = e.create(a, KindGroup, a, b)
	e.wantCode(err, cgen.NET001)
	_, err = e.create(a, KindGroup, b, b)
	e.wantCode(err, cgen.NET001)
	ghost := &member{id: uuid.Must(uuid.NewV7())}
	_, err = e.create(a, KindGroup, b, ghost)
	e.wantCode(err, cgen.MEM001)
	d := e.member("三郎", boundary.RoleMember)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, d.id, a.id, "") }))
	_, err = e.create(a, KindGroup, b, d)
	e.wantCode(err, cgen.MEM002)
	g, err := e.create(a, KindGroup, b, c)
	e.must(err)
	room := uuid.FromStringOrNil(g.Id)
	if g.Kind != gen.RoomSummaryKindGroup || !slices.Equal(g.Members, []string{a.id.String()}) || g.CreatorId == nil || *g.CreatorId != a.id.String() {
		t.Fatalf("作成の応答: %+v", g)
	}
	ms := e.members(room)
	if ms[a.id] != MemberActive || ms[b.id] != MemberPendingAdd || ms[c.id] != MemberPendingAdd {
		t.Fatalf("作成の構成員: %v", ms)
	}
	for _, m := range []*member{a, b, c} {
		if n := len(e.feedOf(m.id, kindRoomSettings)); n == 0 {
			t.Fatal("room.settings が全員に届かない")
		}
		if ch := e.memberChanges(m.id, room, b.id); !slices.Equal(ch, []string{changePendingAdd}) {
			t.Fatalf("room.member（b の追加待ち）: %v", ch)
		}
	}
	if !slices.Equal(e.systemKinds(room), []string{systemRoomCreated}) {
		t.Fatalf("room_created: %v", e.systemKinds(room))
	}
}

// R: R-4-1-3
// 一覧（第4章 4.2、第2章 5.5）：未読は編集・annotation・自分の送信・削除済みの印・構成員になる前のイベントを数えず、追加待ちは 0。
// fully_read の後だけ数える。last_pos は本人の feed のそのトークの最新の room.event の pos。epoch は今の epoch。cursor は識別子の順。
func TestListUnreadLastPosEpoch(t *testing.T) {
	e, a, b, x := family3(t)
	room := e.group(a, b, x)
	c := e.member("三郎", boundary.RoleMember)
	e.joinFamily(c)
	m1 := e.send(room, a, msgOpt{})
	m2 := e.send(room, a, msgOpt{})
	e.send(room, a, msgOpt{rel: gen.FtEventMls1RelatesToRelTypeReplace, target: m2})
	e.send(room, b, msgOpt{rel: gen.FtEventMls1RelatesToRelTypeAnnotation, target: m1})
	e.send(room, b, msgOpt{})
	m3 := e.send(room, a, msgOpt{})
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Timeline.Redact(e.ctx, tx, a.p(), room, m3, nil) }))
	view := func(m *member) RoomView {
		v, err := e.b.Rooms.View(e.ctx, e.pool, m.id, room)
		e.must(err)
		return v
	}
	if v := view(b); v.Unread != 2 {
		t.Fatalf("未読（a の 2 件。編集・リアクション・自分・取消を除く）: %d", v.Unread)
	}
	if v := view(a); v.Unread != 1 {
		t.Fatalf("a の未読（b の 1 件）: %d", v.Unread)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Receipts.Put(e.ctx, tx, b.id, room, nil, nil, &m1) }))
	if v := view(b); v.Unread != 1 {
		t.Fatalf("fully_read の後: %d", v.Unread)
	}
	e.must(e.tx(func(tx *store.Tx) error {
		by := a.id
		_, err := e.b.Members.Add(e.ctx, tx, room, &by, c.id)
		return err
	}))
	if v := view(c); v.Unread != 0 {
		t.Fatalf("追加待ちの未読: %d", v.Unread)
	}
	e.clk.Advance(time.Second)
	e.must(e.commit(room, []uuid.UUID{c.dev}, nil))
	e.clk.Advance(time.Second)
	e.send(room, a, msgOpt{})
	vc := view(c)
	if vc.Unread != 1 {
		t.Fatalf("構成員になった後だけ数える: %d", vc.Unread)
	}
	rows := e.feedOf(c.id, kindRoomEvent)
	if vc.LastPos == nil || int64(*vc.LastPos) != rows[len(rows)-1].pos {
		t.Fatalf("last_pos: %v %v", vc.LastPos, rows[len(rows)-1].pos)
	}
	_, ep := e.epoch(room)
	if int64(vc.Epoch) != ep || ep == 0 {
		t.Fatalf("epoch: %d %d", vc.Epoch, ep)
	}
	// 一覧：家族全員、自分用メモ、グループの 3 つ（識別子の順）。2 件ずつの頁。
	all, more, err := e.b.Rooms.List(e.ctx, e.pool, c.id, Cursor{Limit: 10})
	e.must(err)
	if more || len(all) != 3 {
		t.Fatalf("一覧: %d %v", len(all), more)
	}
	first, more, err := e.b.Rooms.List(e.ctx, e.pool, c.id, Cursor{Limit: 2})
	e.must(err)
	if !more || len(first) != 2 || first[0].Id != all[1].Id {
		t.Fatalf("新しい側の頁: %v", first)
	}
	after := uuid.FromStringOrNil(all[0].Id)
	rest, more, err := e.b.Rooms.List(e.ctx, e.pool, c.id, Cursor{After: &after, Limit: 2})
	e.must(err)
	if more || len(rest) != 2 || rest[0].Id != all[1].Id {
		t.Fatalf("after の頁: %v", rest)
	}
}

// R: R-4-1-2
// 構成員の追加と外し（第4章 4.1、第12章 5.2）：追加は追加待ちで、Commit の後に active と member_added。追加待ちと削除待ちの人の再追加は
// 今の行を返して何も変えない。外しは leaf を持つ端末ごとに削除待ちを立て（room.member{device_id, pending_remove} は srv-keys）、その人は
// 直ちに構成員でない（ROOM-002）。Remove の Commit で removed と member_removed。退出は left と member_left。leaf の無い人（追加待ちの
// まま）はその場で外れる。構成員でない人は MEM-001、作成者・管理者・本人以外は ROOM-003、1 対 1 は ROOM-003。
func TestMembersAddRemoveResolve(t *testing.T) {
	e, a, b, c := family3(t)
	d := e.member("三郎", boundary.RoleMember)
	e.joinFamily(d)
	room := e.group(a, b, c)
	b2 := e.addDevice(b, "太郎のタブレット")
	e.must(e.commit(room, []uuid.UUID{b2}, nil))
	addBy := func(by, m *member) (Member, error) {
		var out Member
		err := e.tx(func(tx *store.Tx) error {
			x := by.id
			var err error
			out, err = e.b.Members.Add(e.ctx, tx, room, &x, m.id)
			return err
		})
		return out, err
	}
	remove := func(by, m *member, admin bool) error {
		return e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, room, by.id, admin, m.id) })
	}
	// 作成者・管理者・本人以外は外せない。構成員でない人は MEM-001。
	e.wantCode(remove(c, b, false), cgen.ROOM003)
	e.wantCode(remove(a, d, false), cgen.MEM001)
	// 外す：b は 2 台の leaf を持つ。
	e.must(remove(a, b, false))
	if e.members(room)[b.id] != MemberPendingRemove || e.count(`SELECT count(*) FROM mls_pending WHERE room_id = $1 AND kind = 'remove'`, room) != 2 {
		t.Fatal("2 台の削除待ち")
	}
	if ok, _ := e.b.Members.IsMember(e.ctx, b.id, room); ok {
		t.Fatal("削除待ちの人が構成員のまま")
	}
	if cur, err := addBy(c, b); err != nil || cur.State != MemberPendingRemove {
		t.Fatalf("削除待ちの人の再追加は何も変えない: %+v %v", cur, err)
	}
	e.must(remove(a, b, false)) // 削除待ちの人をもう一度：何もしない
	e.must(e.commit(room, nil, []uuid.UUID{b.dev}))
	if e.members(room)[b.id] != MemberPendingRemove {
		t.Fatal("1 台目の Remove の後は削除待ちのまま")
	}
	e.must(e.commit(room, nil, []uuid.UUID{b2}))
	if e.members(room)[b.id] != MemberRemoved || !slices.Contains(e.systemKinds(room), systemMemberRemoved) {
		t.Fatalf("最後の leaf の Remove で removed と member_removed: %v", e.systemKinds(room))
	}
	if ch := e.memberChanges(b.id, room, b.id); ch[len(ch)-1] != changeRemoved {
		t.Fatalf("外された本人への room.member: %v", ch)
	}
	// 退出：本人の Remove は他の端末の Commit で left と member_left。
	e.must(remove(c, c, false))
	e.must(e.commit(room, nil, []uuid.UUID{c.dev}))
	if e.members(room)[c.id] != MemberLeft || !slices.Contains(e.systemKinds(room), systemMemberLeft) {
		t.Fatal("退出は left と member_left")
	}
	// 追加待ちのまま（leaf が無い）の人はその場で外れる（知らせは出さない：active でなかった）。
	if cur, err := addBy(a, d); err != nil || cur.State != MemberPendingAdd {
		t.Fatalf("追加: %+v %v", cur, err)
	}
	if cur, err := addBy(a, d); err != nil || cur.State != MemberPendingAdd {
		t.Fatalf("追加待ちの人の再追加: %+v %v", cur, err)
	}
	sysBefore := len(e.systemKinds(room))
	e.must(remove(a, d, false))
	if e.members(room)[d.id] != MemberRemoved || len(e.systemKinds(room)) != sysBefore {
		t.Fatal("leaf の無い人はその場で removed、知らせなし")
	}
	// 外れた人は再追加できる（追加待ちから）。
	if cur, err := addBy(a, b); err != nil || cur.State != MemberPendingAdd {
		t.Fatalf("外れた人の再追加: %+v %v", cur, err)
	}
	// 1 対 1 は追加も外しもできない。
	dm := e.dm(a, d)
	e.wantCode(e.tx(func(tx *store.Tx) error { x := a.id; _, err := e.b.Members.Add(e.ctx, tx, dm, &x, c.id); return err }), cgen.ROOM003)
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, dm, a.id, true, d.id) }), cgen.ROOM003)
}

// R: R-4-1-2
// 作成者の外しと最後の構成員の退出（第4章 4.1）：作成者が外れると creator_id が外れ room.settings、以後は管理者と本人だけが外せる。
// 他に active の構成員が残らなければ、Commit を作れる端末が無いのでグループの記録を消し（mls_* の行が無い）、削除待ちの人を外し、
// トークを空にする。30 日の後に Gc.DeleteEmptyGroups が削除済みにし、7 日の後に PurgeDeletedRooms が行を消す。
func TestCreatorLeavesAndLastLeaveEmptiesGroup(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(b, a, c)
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, room, b.id, false, b.id) }))
	var creator *uuid.UUID
	e.scalar(&creator, `SELECT creator_id FROM rooms WHERE id = $1`, room)
	if creator != nil {
		t.Fatal("作成者の退出で creator_id が残る")
	}
	e.must(e.commit(room, nil, []uuid.UUID{b.dev}))
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, room, c.id, false, a.id) }), cgen.ROOM003)
	// a が c を外し（削除待ち）、続けて a が退出すると active が残らない。
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, room, a.id, true, c.id) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, room, a.id, false, a.id) }))
	ms := e.members(room)
	if ms[a.id] != MemberLeft || ms[c.id] != MemberRemoved {
		t.Fatalf("最後の退出の後の構成員: %v", ms)
	}
	for _, tbl := range []string{"mls_groups", "mls_leaves", "mls_pending"} {
		if e.count(`SELECT count(*) FROM `+tbl+` WHERE room_id = $1`, room) != 0 {
			t.Fatalf("%s の行が残る", tbl)
		}
	}
	var st string
	e.scalar(&st, `SELECT state FROM rooms WHERE id = $1`, room)
	if st != string(RoomEmpty) {
		t.Fatalf("空にならない: %s", st)
	}
	if n, err := e.b.Gc.DeleteEmptyGroups(e.ctx, e.clk.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("30 日の前に消した: %d %v", n, err)
	}
	e.clk.Advance(31 * 24 * time.Hour)
	if n, err := e.b.Gc.DeleteEmptyGroups(e.ctx, e.clk.Now().Add(-30*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("30 日の後に消さない: %d %v", n, err)
	}
	e.scalar(&st, `SELECT state FROM rooms WHERE id = $1`, room)
	if st != string(RoomDeleted) {
		t.Fatalf("削除済みにならない: %s", st)
	}
	if n, _ := e.b.Gc.PurgeDeletedRooms(e.ctx, e.clk.Now().Add(-7*24*time.Hour)); n != 0 {
		t.Fatal("7 日の前に行を消した")
	}
	e.clk.Advance(8 * 24 * time.Hour)
	if n, err := e.b.Gc.PurgeDeletedRooms(e.ctx, e.clk.Now().Add(-7*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("7 日の後に行を消さない: %d %v", n, err)
	}
	if e.count(`SELECT count(*) FROM events WHERE room_id = $1`, room)+e.count(`SELECT count(*) FROM rooms WHERE id = $1`, room) != 0 {
		t.Fatal("行が残る")
	}
}

// R: R-12-6-1
// グループの削除（第4章 4.1、第12章 9、第2章 5.5）：作成者と構成員の管理者だけ通り、他の構成員は ROOM-003、家族全員は ROOM-011、
// 1 対 1 は ROOM-003。全構成員（削除待ち・追加待ちを含む）に room.member{removed}、mls_* の行が消え Groups.Epoch は ROOM-001、
// 全イベントが削除済みの印（暗号文 NULL、媒体の結び付きを外す）、pending の予約送信が cancelled（仕事も消える）、通話が終わる。
// /admin/rooms/{id}（empty_group）は構成員が残れば ROOM-003。
func TestDeleteRoom(t *testing.T) {
	e, a, b, c := family3(t)
	del := func(room uuid.UUID, rule DeleteRule, by *member, admin bool) error {
		return e.tx(func(tx *store.Tx) error { return e.b.Rooms.Delete(e.ctx, tx, room, rule, by.id, admin) })
	}
	e.wantCode(del(e.family(), RuleRoomDelete, a, true), cgen.ROOM011)
	e.wantCode(del(e.dm(a, b), RuleRoomDelete, a, true), cgen.ROOM003)
	room := e.group(b, a, c)
	e.wantCode(del(room, RuleRoomDelete, c, false), cgen.ROOM003)
	e.wantCode(del(room, RuleEmptyGroup, a, true), cgen.ROOM003)
	d := e.member("三郎", boundary.RoleMember)
	e.joinFamily(d)
	e.must(e.tx(func(tx *store.Tx) error { x := a.id; _, err := e.b.Members.Add(e.ctx, tx, room, &x, d.id); return err }))
	m1 := e.send(room, a, msgOpt{media: []uuid.UUID{uuid.Must(uuid.NewV7())}})
	var sched ScheduledView
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		sched, err = e.b.Scheduled.Create(e.ctx, tx, c.p(), room, e.clk.Now().Add(time.Hour), e.body(room, msgOpt{}))
		return err
	}))
	e.must(del(room, RuleRoomDelete, a, true)) // 構成員の管理者（作成者は b）
	for _, m := range []*member{a, b, c, d} {
		if ch := e.memberChanges(m.id, room, m.id); len(ch) == 0 || ch[len(ch)-1] != changeRemoved {
			t.Fatalf("room.member{removed} が %s に届かない: %v", m.id, ch)
		}
	}
	for _, tbl := range []string{"mls_groups", "mls_leaves", "mls_pending"} {
		if e.count(`SELECT count(*) FROM `+tbl+` WHERE room_id = $1`, room) != 0 {
			t.Fatalf("%s の行が残る", tbl)
		}
	}
	_, err := e.k.Groups.Epoch(e.ctx, room)
	e.wantCode(err, cgen.ROOM001)
	if e.count(`SELECT count(*) FROM events WHERE room_id = $1 AND (redacted_at IS NULL OR ciphertext IS NOT NULL)`, room) != 0 {
		t.Fatal("削除済みの印でないイベントが残る")
	}
	// 偽物は UnbindRoom を記録だけする（そのトークの結び付きを外す中身は srv-media の試験）。
	if len(e.media.boundEvent(m1)) != 1 || e.media.logged("unbind_room:"+room.String()) != 1 {
		t.Fatalf("UnbindRoom: %d", e.media.logged("unbind_room:"+room.String()))
	}
	var st string
	e.scalar(&st, `SELECT state FROM scheduled_events WHERE id = $1`, uuid.FromStringOrNil(sched.Id))
	if st != string(ScheduledCancelled) || e.count(`SELECT count(*) FROM jobs WHERE dedupe_key = $1`, "scheduled_send:"+sched.Id) != 0 {
		t.Fatalf("予約送信が取り消されない: %s", st)
	}
	if !slices.Contains(e.calls.ended, room) {
		t.Fatal("通話を終えない")
	}
	e.wantCode(del(room, RuleRoomDelete, a, true), cgen.ROOM001)
	if ok, _ := e.b.Members.IsMember(e.ctx, a.id, room); ok {
		t.Fatal("削除したトークの構成員のまま")
	}
	// 空のグループ（構成員が残らない）は /admin/rooms/{id} の規則 empty_group で消せる。
	empty := e.group(a, b, c)
	for _, m := range []*member{a, b, c} {
		e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, empty, m.id, false, m.id) }))
	}
	e.must(del(empty, RuleEmptyGroup, a, true))
	e.scalar(&st, `SELECT state FROM rooms WHERE id = $1`, empty)
	if st != string(RoomDeleted) {
		t.Fatalf("空のグループが消えない: %s", st)
	}
}

// R: R-12-6-1
// 停止・再開・削除・抹消の hook（第4章 6、第12章 9、第3章 8）：停止は家族全員のトークに member_suspended（削除の停止では出ない）、再開は
// member_unsuspended。削除は本人が active か追加待ちのトーク（家族全員を含む）から外し（by 無し、left にしない）、member_deleted。
// 抹消は member_erased。自分用メモはグループの記録が消え、トークの行は残る。
func TestLifecycleHooks(t *testing.T) {
	e, a, b, c := family3(t)
	fam := e.family()
	room := e.group(b, a, c)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, c.id, a.id, "") }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Unsuspend(e.ctx, tx, c.id, a.id) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, b.id, a.id, false) }))
	kinds := e.systemKinds(fam)
	if !slices.Contains(kinds, systemMemberSuspended) || !slices.Contains(kinds, systemMemberUnsuspended) || !slices.Contains(kinds, systemMemberDeleted) {
		t.Fatalf("家族全員のトークの知らせ: %v", kinds)
	}
	n := 0
	for _, k := range kinds {
		if k == systemMemberSuspended {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("削除の停止で member_suspended が出た: %v", kinds)
	}
	for _, r := range []uuid.UUID{fam, room} {
		if st := e.members(r)[b.id]; st != MemberPendingRemove && st != MemberRemoved {
			t.Fatalf("削除した人が外れない: %s", st)
		}
	}
	var by *uuid.UUID
	e.scalar(&by, `SELECT by FROM room_members WHERE room_id = $1 AND user_id = $2`, room, b.id)
	if by != nil {
		t.Fatal("サーバーの外しの by が残る")
	}
	s := e.selfRoom(b)
	if e.count(`SELECT count(*) FROM mls_groups WHERE room_id = $1`, s) != 0 || e.count(`SELECT count(*) FROM rooms WHERE id = $1`, s) != 1 {
		t.Fatal("自分用メモの記録")
	}
	// 抹消（猶予の 24 時間の後）：まだ外れていないトークから外し、member_erased。
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, c.id); return err }))
	e.clk.Advance(25 * time.Hour)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Erase(e.ctx, tx, c.id) }))
	if !slices.Contains(e.systemKinds(fam), systemMemberErased) {
		t.Fatal("member_erased")
	}
	for _, r := range []uuid.UUID{fam, room} {
		if st := e.members(r)[c.id]; st != MemberPendingRemove && st != MemberRemoved {
			t.Fatalf("抹消した人が外れない: %s", st)
		}
	}
}

// R: R-12-6-1
// 削除の錠の順（全体実装設計書 2.4、I-18、ページの錠の順）：hook OnBeforeUserChange が、外すトークと一括の取消のトークを含む和を、
// 始めに識別子の順に RoomLock で取る。最も小さいトークの錠を試験が持つ間、削除はそこで待ち、それより大きいトークの錠をまだ持たない
// （試験が NOWAIT で取れる）。
func TestErasureHookLocksRoomsInIdOrder(t *testing.T) {
	e, a, b, c := family3(t)
	r1 := e.group(a, b, c)
	r2 := e.group(a, b, c)
	e.send(r1, b, msgOpt{})
	e.send(r2, b, msgOpt{})
	// b は r1 から退出済み（送ったイベントは残る）：外しの対象は r2 と家族全員と自分用メモ、一括の取消の対象は r1 と r2。
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, r1, b.id, false, b.id) }))
	e.must(e.commit(r1, nil, []uuid.UUID{b.dev}))
	ids := sortedIds([]uuid.UUID{r1, r2, e.family(), e.selfRoom(b)})
	lowest, rest := ids[0], ids[1:]
	hold, err := e.pool.Begin(e.ctx)
	e.must(err)
	t.Cleanup(func() { hold.Rollback(e.ctx) })
	e.must(store.RoomLock{}.Lock(e.ctx, hold, lowest))
	ch := within(func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, b.id, a.id, true) })
	})
	e.waiting(ch, 300*time.Millisecond)
	for _, r := range rest {
		probe, err := e.pool.Begin(e.ctx)
		e.must(err)
		if _, err := probe.Exec(e.ctx, `SELECT 1 FROM rooms WHERE id = $1 FOR UPDATE NOWAIT`, r); err != nil {
			t.Fatalf("最も小さいトークで待つ間に大きいトークの錠を持っている: %v", err)
		}
		probe.Rollback(e.ctx)
	}
	hold.Rollback(e.ctx)
	e.must(<-ch)
	if e.count(`SELECT count(*) FROM events WHERE sender_id = $1 AND schema = 'ft.event.mls/1' AND ciphertext IS NOT NULL`, b.id) != 0 {
		t.Fatal("一括の取消が済んでいない")
	}
}

// swapLeaf は b の端末 d2 の leaf を、端末の識別子の大小に応じて ry か rx に置く。小さい方の端末の leaf を大きいトーク rx に、大きい方
// の端末の leaf を小さいトーク ry に置く（b の元の端末は group で両方に leaf を持つので、置く方のトークで元の端末と入れ替える）。
// 抹消は端末を識別子の順に 1 台ずつ取り消すので、端末ごとに leaf のトークを取ると rx の後に ry を取る形になる。
func (e *env) swapLeaf(b *member, d2, ry, rx uuid.UUID) {
	e.t.Helper()
	room := ry
	if bytes.Compare(d2[:], b.dev[:]) < 0 {
		room = rx
	}
	e.must(e.commit(room, []uuid.UUID{d2}, []uuid.UUID{b.dev}))
}

// R: R-12-6-1
// 抹消の錠の順（全体実装設計書 2.4、I-18。T-L1-09）：本人の 2 台の端末が別々のトークに leaf を持つ（識別子の小さい端末が大きいトーク
// Rx に、大きい端末が小さいトーク Ry に）。試験が Ry の錠を持つ間、抹消は Ry で待ち、Rx の錠をまだ持たない。
func TestEraseLocksRoomsInIdOrder(t *testing.T) {
	e, a, b, c := family3(t)
	ry := e.group(a, b, c)
	rx := e.group(a, b, c)
	e.swapLeaf(b, e.addDevice(b, "太郎のタブレット"), ry, rx)
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, b.id); return err }))
	e.clk.Advance(25 * time.Hour)
	if e.heldWhileWaiting(ry, rx, func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Users.Erase(e.ctx, tx, b.id) })
	}) {
		t.Fatal("抹消が小さいトーク Ry で待つ間に大きいトーク Rx の錠を持っている（複数のトークを識別子の順に取っていない）")
	}
}

// R: R-12-6-1
// 削除の錠の順を hook をまたいで（全体実装設計書 2.4、I-18。T-L1-09）：srv-keys の hook が取る leaf のトーク（Ry、Rx）の間の識別子に、
// 本ブロックの hook だけが取る送ったイベントだけのトーク Rz（退出済み）がある（Ry < Rz < Rx）。試験が Rz の錠を持つ間、削除で消すは
// Rz で待ち、Rx の錠をまだ持たない。
func TestDeleteLocksRoomsInIdOrderAcrossBlocks(t *testing.T) {
	e, a, b, c := family3(t)
	e.group(a, b, c)
	rz := e.group(a, b, c)
	rx := e.group(a, b, c)
	e.send(rz, b, msgOpt{})
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, rz, b.id, false, b.id) }))
	e.must(e.commit(rz, nil, []uuid.UUID{b.dev}))
	if e.heldWhileWaiting(rz, rx, func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, b.id, a.id, true) })
	}) {
		t.Fatal("削除が小さいトーク Rz で待つ間に大きいトーク Rx の錠を持っている（hook をまたいで識別子の順でない）")
	}
}

// R: R-12-6-1
// 停止の錠の順（全体実装設計書 2.4、I-18。T-L1-09）：本人の端末の追加待ちのトーク Rx（srv-keys の hook が取る）と、家族全員のトーク
// （本ブロックの hook が知らせを書く。識別子が最も小さい）。試験が家族全員のトークの錠を持つ間、停止はそこで待ち、Rx の錠をまだ持たない。
func TestSuspendLocksFamilyRoomInIdOrder(t *testing.T) {
	e, a, b, c := family3(t)
	rx := e.group(a, b, c)
	d2 := e.addDevice(b, "太郎のタブレット")
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.k.Groups.RequestAdd(e.ctx, tx, d2); return err }))
	if e.count(`SELECT count(*) FROM mls_pending WHERE room_id = $1 AND device_id = $2 AND kind = 'add'`, rx, d2) != 1 {
		t.Fatal("端末の追加待ちが立たない（試験の前提）")
	}
	e.must(e.commit(e.family(), []uuid.UUID{d2}, nil))
	e.must(e.commit(e.selfRoom(b), []uuid.UUID{d2}, nil))
	if e.heldWhileWaiting(e.family(), rx, func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, b.id, a.id, "") })
	}) {
		t.Fatal("停止が家族全員のトークで待つ間に大きいトーク Rx の錠を持っている")
	}
}

// R: R-12-6-1
// 管理者の端末の取り消しの錠の順（全体実装設計書 2.4、I-18。T-L1-09）：端末が leaf を持つトーク Rx と、取り消しの後の監査の記録が書く
// 家族全員のトーク（識別子が小さい）。端末は家族全員のトークに leaf を持たない。試験が家族全員のトークの錠を持つ間、取り消しと
// 監査の記録（srv-admin の Audit.Record と同じく同じトランザクションで FamilyRoom.Flow）は家族全員のトークで待ち、Rx の錠をまだ持たない。
func TestAdminDeviceRevokeLocksFamilyRoomFirst(t *testing.T) {
	e, a, b, c := family3(t)
	rx := e.group(a, b, c)
	d2 := e.addDevice(b, "太郎のタブレット")
	e.must(e.commit(rx, []uuid.UUID{d2}, nil))
	if e.heldWhileWaiting(e.family(), rx, func() error {
		return e.tx(func(tx *store.Tx) error {
			if err := e.id.Devices.Revoke(e.ctx, tx, d2, a.id, false); err != nil {
				return err
			}
			return e.b.FamilyRoom.Flow(e.ctx, tx, string(gen.FtEventSystem1KindAdminAction), map[string]any{"action": "device_revoke", "user_id": b.id.String()})
		})
	}) {
		t.Fatal("管理者の端末の取り消しが家族全員のトークで待つ間に大きいトーク Rx の錠を持っている")
	}
	if e.pendingKind(rx, d2) != "remove" {
		t.Fatal("取り消した端末の削除待ちが立たない")
	}
}

// R: R-12-6-1
// 抹消と、複数のトークを識別子の順に取る別のトランザクション（他の構成員の端末の取り消しと同じ順。Ry、Rx）を重ねても待ち合わない
// （全体実装設計書 2.4、I-18。T-L1-09）：別のトランザクションが Ry を持つ間に抹消を始め、続けて Rx を取る。40P01 が出ず両方が通る。
func TestEraseAndRevokeDoNotDeadlock(t *testing.T) {
	e, a, b, c := family3(t)
	ry := e.group(a, b, c)
	rx := e.group(a, b, c)
	e.swapLeaf(b, e.addDevice(b, "太郎のタブレット"), ry, rx)
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, b.id); return err }))
	e.clk.Advance(25 * time.Hour)
	other, err := e.pool.Begin(e.ctx)
	e.must(err)
	t.Cleanup(func() { other.Rollback(context.Background()) })
	e.must(store.RoomLock{}.Lock(e.ctx, other, ry))
	ch := within(func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Users.Erase(e.ctx, tx, b.id) })
	})
	e.waiting(ch, 500*time.Millisecond)
	oerr := store.RoomLock{}.Lock(e.ctx, other, rx)
	e.must(other.Commit(e.ctx))
	if oerr != nil {
		t.Fatalf("別のトランザクションが落ちた: %v", oerr)
	}
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("抹消が落ちた: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("抹消が終わらない")
	}
	if e.count(`SELECT count(*) FROM users WHERE id = $1 AND state = 'erased'`, b.id) != 1 {
		t.Fatal("抹消が済んでいない")
	}
}

// R: R-4-1-2
// 停止と同時の構成員の追加とトークの作成（全体実装設計書 2.4、I-18。T-L1-03）：別のトランザクションが停止を行い users の行の錠を持つ間、
// Members.Add と Rooms.Create はその人の users の行の共有の錠で待ち、停止のコミットの後に MEM-002 で返る（停止の直前に読んだ「通常」で
// 停止中の人を入れない）。
func TestAddAndCreateWaitForSuspend(t *testing.T) {
	e, a, b, c := family3(t)
	d := e.member("三郎", boundary.RoleMember)
	e.joinFamily(d)
	room := e.group(a, b, d)
	hold, err := e.pool.Begin(e.ctx)
	e.must(err)
	t.Cleanup(func() { hold.Rollback(context.Background()) })
	e.must(e.id.Users.Suspend(e.ctx, hold, c.id, a.id, ""))
	add := within(func() error {
		return e.tx(func(tx *store.Tx) error { _, err := e.b.Members.Add(e.ctx, tx, room, &a.id, c.id); return err })
	})
	create := within(func() error { _, err := e.create(a, KindGroup, b, c); return err })
	e.waiting(add, 500*time.Millisecond)
	e.waiting(create, 100*time.Millisecond)
	e.must(hold.Commit(e.ctx))
	for name, ch := range map[string]chan error{"構成員の追加": add, "トークの作成": create} {
		select {
		case err := <-ch:
			e.wantCode(err, cgen.MEM002)
		case <-time.After(10 * time.Second):
			t.Fatalf("%s が終わらない", name)
		}
	}
	if _, ok := e.members(room)[c.id]; ok {
		t.Fatal("停止中の人がトークに入った")
	}
}
