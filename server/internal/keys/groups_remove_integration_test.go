// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package keys

import (
	"slices"
	"testing"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// pendingOf はトークのその端末の待ちの種類（無ければ空）。
func (e *env) pendingOf(room, dev uuid.UUID) string {
	e.t.Helper()
	var kind string
	if e.count(`SELECT count(*) FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, dev) == 0 {
		return ""
	}
	e.scalar(&kind, `SELECT kind FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, dev)
	return kind
}

// removeUser は RequestRemoveUser を 1 つのトランザクションで呼ぶ。
func (e *env) removeUser(room, user uuid.UUID) int {
	e.t.Helper()
	var n int
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		n, err = e.k.Groups.RequestRemoveUser(e.ctx, tx, room, user)
		return err
	}))
	return n
}

// pendingRemoves は利用者の feed の room.member のうち、端末つきの pending_remove の端末（流れた順）。
func (e *env) pendingRemoves(user uuid.UUID) []uuid.UUID {
	e.t.Helper()
	var out []uuid.UUID
	for _, p := range payloads[roomMember](e.t, e.feedOf(user, "room.member")) {
		if p.Change == memberPendingRemove && p.DeviceId != nil {
			out = append(out, *p.DeviceId)
		}
	}
	return out
}

// R: R-12-3-2
// 退出・外しの端末ごとの削除待ち（第12章 5.2「構成員の追加と退出」。T-L1-04）：leaf を持つ端末だけに remove（readd は remove に
// 替わる）、leaf の無い端末の add と readd は消える、戻りは leaf を持つ端末の数、room.member{device_id, pending_remove} は行を
// 入れたか替えた時だけ（2 回目は何も流さない）。leaf の無い人は 0。他の構成員の待ちは触らない。
func TestRequestRemoveUser(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	b2 := e.addDevice(b, "太郎のタブレット", true)
	room := e.room(a.dev(), a, b, c)
	_, err := e.addAs(room, a.dev(), 0, b.dev(), b2)
	e.must(err)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Reject(e.ctx, tx, room, b2.deviceId, e.mustGen(room), nil, nil, "other")
		return err
	}))
	b3 := e.addDevice(b, "太郎のパソコン", true) // 承認で追加待ち（leaf なし）
	var cAdds int
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		cAdds, err = e.k.Groups.RequestAdd(e.ctx, tx, c.dev().deviceId)
		return err
	}))
	if e.pendingOf(room, b2.deviceId) != "readd" || e.pendingOf(room, b3.deviceId) != "add" || cAdds != 1 {
		t.Fatalf("前提の待ち: %q %q %d", e.pendingOf(room, b2.deviceId), e.pendingOf(room, b3.deviceId), cAdds)
	}
	before := len(e.pendingRemoves(a.id))
	if n := e.removeUser(room, b.id); n != 2 {
		t.Fatalf("leaf を持つ端末の数: %d", n)
	}
	if e.pendingOf(room, b.dev().deviceId) != "remove" || e.pendingOf(room, b2.deviceId) != "remove" || e.pendingOf(room, b3.deviceId) != "" {
		t.Fatalf("待ち: %q %q %q", e.pendingOf(room, b.dev().deviceId), e.pendingOf(room, b2.deviceId), e.pendingOf(room, b3.deviceId))
	}
	got := sortedIds(e.pendingRemoves(a.id)[before:])
	if want := sortedIds([]uuid.UUID{b.dev().deviceId, b2.deviceId}); !slices.Equal(got, want) {
		t.Fatalf("room.member{pending_remove}: %v", got)
	}
	if e.pendingOf(room, c.dev().deviceId) != "add" {
		t.Fatal("他の構成員の追加待ちが変わった")
	}
	if n := e.removeUser(room, b.id); n != 2 || len(e.pendingRemoves(a.id)) != before+2 {
		t.Fatalf("2 回目：数 %d、room.member が増えた %d", n, len(e.pendingRemoves(a.id))-before)
	}
	info, err := e.k.Groups.Epoch(e.ctx, room)
	e.must(err)
	if !slices.Equal(info.PendingRemove, sortedIds([]uuid.UUID{b.dev().deviceId, b2.deviceId})) || len(info.PendingReadd) != 0 ||
		!slices.Equal(info.PendingAddDevices, []uuid.UUID{c.dev().deviceId}) || !info.CommitPossible {
		t.Fatalf("Epoch: %+v", info)
	}
	if n := e.removeUser(room, c.id); n != 0 || e.pendingOf(room, c.dev().deviceId) != "" {
		t.Fatalf("leaf の無い人：数 %d、待ち %q", n, e.pendingOf(room, c.dev().deviceId))
	}
	bare := uuid.Must(uuid.NewV7())
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO rooms (id, kind, state, last_event_at, created_at) VALUES ($1, 'group', 'normal', now(), now())`, bare); err != nil {
		t.Fatal(err)
	}
	if n := e.removeUser(bare, b.id); n != 0 {
		t.Fatalf("グループの無いトーク: %d", n)
	}
}

// R: R-12-3-2
// Commit を作れる端末が残っているか（第12章 5.2 の (d)。T-L1-04）：leaf を持つ端末の全部が削除待ちか再参加待ちなら偽（leaf が 0 でも偽）。
// 真の間は再参加待ちでない端末の再初期化は KEY-013 not_pending。偽なら leaf の無い構成員の端末でも直ちに通り（(a)〜(c) を見ない）、
// 世代が進み呼んだ端末が leaf 0。24 時間 2 回の上限は (d) にも掛かる。Commit を作れる端末は承認済みの端末だけで、停止で未承認に戻った
// 端末の leaf は数えない：承認済みの leaf が 1 つあれば真、停止中の構成員の端末の leaf だけが残れば偽で (d) の再初期化が通り、停止中の
// 端末は入るべき端末（追加待ち）に入らない（第3章 8.1）。
func TestCommitPossibleAndReinitWithoutCommitter(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	x := e.member("三郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b, x)
	_, err := e.addAs(room, a.dev(), 0, b.dev())
	e.must(err)
	possible := func() bool {
		t.Helper()
		info, err := e.k.Groups.Epoch(e.ctx, room)
		e.must(err)
		return info.CommitPossible
	}
	reinit := func(dk *devKeys) (ReinitResult, error) {
		var r ReinitResult
		err := e.tx(func(tx *store.Tx) error {
			var err error
			r, err = e.k.Groups.Reinit(e.ctx, tx, room, dk.deviceId)
			return err
		})
		return r, err
	}
	if !possible() {
		t.Fatal("待ちの無い leaf があるのに偽")
	}
	_, err = reinit(x.dev())
	if ev := e.wantCode(err, cgen.KEY013); ev.Params["reason"] != "not_pending" {
		t.Fatalf("真の間の再初期化: %+v", ev)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.RemoveLeafsOfDevice(e.ctx, tx, a.dev().deviceId) }))
	if !possible() {
		t.Fatal("待ちの無い leaf（太郎）が残るのに偽")
	}
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Reject(e.ctx, tx, room, b.dev().deviceId, e.mustGen(room), nil, nil, "other")
		return err
	}))
	if possible() {
		t.Fatal("全 leaf が削除待ちか再参加待ちなのに真")
	}
	r, err := reinit(x.dev())
	e.must(err)
	if l := e.leaves(room); r.Generation != 1 || len(l) != 1 || l[0] != x.dev().deviceId {
		t.Fatalf("(d) の再初期化: %+v %v", r, l)
	}
	for i := 0; i < 2; i++ {
		e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.RemoveLeafsOfDevice(e.ctx, tx, x.dev().deviceId) }))
		if possible() {
			t.Fatal("唯一の leaf が削除待ちなのに真")
		}
		_, err = reinit(x.dev())
		if i == 0 {
			e.must(err)
			continue
		}
		if ev := e.wantCode(err, cgen.KEY013); ev.Params["reason"] != "limit" {
			t.Fatalf("(d) の上限: %+v", ev)
		}
	}
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM mls_pending WHERE room_id = $1`, room); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM mls_leaves WHERE room_id = $1`, room); err != nil {
		t.Fatal(err)
	}
	if possible() {
		t.Fatal("leaf が 0 なのに真")
	}

	// 停止中の構成員の端末の leaf。
	s := e.member("四郎", boundary.RoleMember)
	y := e.member("五郎", boundary.RoleMember)
	room2 := e.room(a.dev(), a, s, y)
	_, err = e.addAs(room2, a.dev(), 0, s.dev())
	e.must(err)
	possible2 := func() bool {
		t.Helper()
		info, err := e.k.Groups.Epoch(e.ctx, room2)
		e.must(err)
		return info.CommitPossible
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.RemoveLeafsOfDevice(e.ctx, tx, a.dev().deviceId) }))
	if !possible2() {
		t.Fatal("待ちの無い承認済みの leaf（四郎）が 1 つあるのに偽")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, s.id, a.id, "") }))
	if possible2() {
		t.Fatal("停止中の構成員の端末の leaf だけが残るのに真")
	}
	var r2 ReinitResult
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		r2, err = e.k.Groups.Reinit(e.ctx, tx, room2, y.dev().deviceId)
		return err
	}))
	if l := e.leaves(room2); r2.Generation != 1 || len(l) != 1 || l[0] != y.dev().deviceId {
		t.Fatalf("停止中の leaf だけの時の (d) の再初期化: %+v %v", r2, l)
	}
	if e.pendingOf(room2, a.dev().deviceId) != "add" || e.pendingOf(room2, s.dev().deviceId) != "" {
		t.Fatalf("入るべき端末：花子 %q、停止中の四郎 %q", e.pendingOf(room2, a.dev().deviceId), e.pendingOf(room2, s.dev().deviceId))
	}
}

// R: R-12-6-1
// グループの記録の削除（第12章 9 のトークの削除と最後の退出。T-L1-04）：Drop の後は mls_* にそのトークの行が無く、Epoch は ROOM-001。
// 2 回目とグループの無いトークは何もしない。他のトークの記録は残る。
func TestDropGroup(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b)
	other := e.room(a.dev(), a, b)
	_, err := e.addAs(room, a.dev(), 0, b.dev())
	e.must(err)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Reject(e.ctx, tx, room, b.dev().deviceId, e.mustGen(room), nil, nil, "other")
		return err
	}))
	e.clk.Advance(reinitWait)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Reinit(e.ctx, tx, room, b.dev().deviceId)
		return err
	}))
	drop := func(r uuid.UUID) {
		t.Helper()
		e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.Drop(e.ctx, tx, r) }))
	}
	drop(room)
	for _, tbl := range []string{"mls_groups", "mls_leaves", "mls_pending", "mls_commits", "mls_rejections", "mls_reinits"} {
		if n := e.count(`SELECT count(*) FROM `+tbl+` WHERE room_id = $1`, room); n != 0 {
			t.Fatalf("%s に %d 行が残る", tbl, n)
		}
	}
	_, err = e.k.Groups.Epoch(e.ctx, room)
	e.wantCode(err, cgen.ROOM001)
	drop(room)
	bare := uuid.Must(uuid.NewV7())
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO rooms (id, kind, state, last_event_at, created_at) VALUES ($1, 'group', 'normal', now(), now())`, bare); err != nil {
		t.Fatal(err)
	}
	drop(bare)
	if _, err := e.k.Groups.Epoch(e.ctx, other); err != nil {
		t.Fatalf("他のトークの記録が消えた: %v", err)
	}
}

// R: R-1-6-2
// Groups.RoomsOfDevices（全体実装設計書 2.4、I-18。T-L1-09）：端末の列のどれかが leaf か待ち（add、remove、readd）を持つトークを、
// 重なりを除き識別子の順に返す。leaf だけ、待ちだけ、両方（同じトークに 1 台の leaf と別の 1 台の待ち）のトークは入り、どちらも無い
// トークと列に無い端末の leaf のトークは入らない。leaf も待ちも無い端末だけの列は空。
func TestRoomsOfDevices(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	b2 := e.addDevice(b, "太郎のタブレット", true)
	idle := e.addDevice(b, "太郎のパソコン", false)
	both := e.room(b.dev(), a, b)
	none := e.room(a.dev(), a, b)
	pend := e.room(a.dev(), a, b)
	leaf := e.room(b.dev(), a, b)
	other := e.room(c.dev(), a, c)
	pendRemove := e.room(a.dev(), a, b)
	for _, p := range []struct {
		room uuid.UUID
		dev  uuid.UUID
		kind string
	}{{both, b2.deviceId, "readd"}, {pend, b2.deviceId, "add"}, {pendRemove, b.dev().deviceId, "remove"}} {
		_, err := e.pool.Exec(e.ctx, `INSERT INTO mls_pending (room_id, device_id, kind, since) VALUES ($1, $2, $3, now())`, p.room, p.dev, p.kind)
		e.must(err)
	}
	got, err := e.k.Groups.RoomsOfDevices(e.ctx, e.pool, []uuid.UUID{b2.deviceId, b.dev().deviceId, idle.deviceId})
	e.must(err)
	want := sortedIds([]uuid.UUID{both, pend, leaf, pendRemove})
	if !slices.Equal(got, want) {
		t.Fatalf("leaf か待ちのあるトークの識別子の順でない: %v（期待 %v。none %v、other %v）", got, want, none, other)
	}
	got, err = e.k.Groups.RoomsOfDevices(e.ctx, e.pool, []uuid.UUID{idle.deviceId})
	e.must(err)
	if len(got) != 0 {
		t.Fatalf("leaf も待ちも無い端末のトークが返った: %v", got)
	}
}
