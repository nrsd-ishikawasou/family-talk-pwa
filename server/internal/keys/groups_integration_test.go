// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package keys

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// group は試験のグループの今の世代と epoch。
func (e *env) group(room uuid.UUID) (int, int64) {
	e.t.Helper()
	var g int
	var ep int64
	if err := e.pool.QueryRow(e.ctx, `SELECT generation, epoch FROM mls_groups WHERE room_id = $1`, room).Scan(&g, &ep); err != nil {
		e.t.Fatal(err)
	}
	return g, ep
}

// leaves は leaf の番号から端末。
func (e *env) leaves(room uuid.UUID) map[int]uuid.UUID {
	e.t.Helper()
	list, err := e.k.Groups.Leaves(e.ctx, e.pool, room)
	e.must(err)
	out := map[int]uuid.UUID{}
	for _, l := range list {
		out[l.Index] = l.DeviceId
	}
	return out
}

// kp は端末の KeyPackage（Add に入れる）。
func (e *env) kp(dk *devKeys, o kpOpts) []byte {
	return buildKeyPackage(e.t, dk, e.clk.Now(), o)
}

// commitAs は by の端末で今の世代と epoch に対する Commit を送る。spec の group_id と epoch が空なら今の物を入れる。
func (e *env) commitAs(room uuid.UUID, by *devKeys, spec commitSpec, welcome []byte) (CommitResult, error) {
	e.t.Helper()
	g, ep := e.group(room)
	if spec.groupId == nil {
		spec.groupId = groupIdOf(room, g)
	}
	if spec.epoch == 0 {
		spec.epoch = uint64(ep)
	}
	raw := buildCommit(e.t, spec)
	var res CommitResult
	err := e.tx(func(tx *store.Tx) error {
		var err error
		res, err = e.k.Groups.Commit(e.ctx, tx, room, by.deviceId, ep, raw, welcome)
		return err
	})
	return res, err
}

// addAs は by の端末（leaf の番号 sender）で dks を Add する Commit（Welcome つき）。
func (e *env) addAs(room uuid.UUID, by *devKeys, sender uint32, dks ...*devKeys) (CommitResult, error) {
	var adds, refs [][]byte
	for _, dk := range dks {
		k := e.kp(dk, kpOpts{})
		adds = append(adds, k)
		refs = append(refs, refOf(e.t, k))
	}
	return e.commitAs(room, by, commitSpec{sender: sender, adds: adds}, buildWelcome(mlsSuite, refs...))
}

// R: R-12-3-2
// Commit の受け付け（第12章 5.2、I-4）：leaf の番号（Add は左端の空き、無ければ幅の番号、Remove の後の切り詰め）、epoch が 1 進む、
// commit_ref、Welcome を Add の端末のメールボックスへ、待ちが解ける、room.mls（Welcome の有無で payload を分ける）と room.member。古い epoch は
// KEY-007。
func TestCommitAcceptsAndNumbers(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	d := e.member("三郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b, c, d)
	// 承認の後の追加待ちと同じく、待ちを立てる。
	e.must(e.tx(func(tx *store.Tx) error {
		for _, m := range []*member{b, c, d} {
			if _, err := e.k.Groups.RequestAdd(e.ctx, tx, m.dev().deviceId); err != nil {
				return err
			}
		}
		return nil
	}))
	info, err := e.k.Groups.Epoch(e.ctx, room)
	e.must(err)
	if info.Generation != 0 || info.Epoch != 0 || len(info.PendingAddDevices) != 3 || !bytes.Equal(info.GroupId, groupIdOf(room, 0)) {
		t.Fatalf("Epoch: %+v", info)
	}
	res, err := e.addAs(room, a.dev(), 0, b.dev(), c.dev())
	e.must(err)
	if res.Epoch != 1 || len(res.Added) != 2 || len(res.Removed) != 0 {
		t.Fatalf("CommitResult: %+v", res)
	}
	// 一覧の epoch（Rooms.List）：トークごとの今の値。グループの無いトークは地図に入らない。
	other := e.room(b.dev(), b)
	none := uuid.Must(uuid.NewV7())
	if got, err := e.k.Groups.EpochsOf(e.ctx, e.pool, []uuid.UUID{room, other, none}); err != nil || len(got) != 2 || got[room] != 1 || got[other] != 0 {
		t.Fatalf("EpochsOf: %v %v", got, err)
	}
	if l := e.leaves(room); l[0] != a.dev().deviceId || l[1] != b.dev().deviceId || l[2] != c.dev().deviceId {
		t.Fatalf("leaf の番号: %v", l)
	}
	var ref string
	e.scalar(&ref, `SELECT commit_ref FROM mls_commits WHERE room_id = $1 AND epoch = 1`, room)
	if ref != res.CommitRef || len(ref) != 43 {
		t.Fatalf("commit_ref: %s %s", ref, res.CommitRef)
	}
	if n := e.count(`SELECT count(*) FROM device_inbox WHERE kind = 'welcome' AND to_device = ANY($1)`, []uuid.UUID{b.dev().deviceId, c.dev().deviceId}); n != 2 {
		t.Fatalf("Welcome の数: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM mls_pending WHERE room_id = $1`, room); n != 1 {
		t.Fatalf("解けた待ちが残る: %d", n)
	}
	for m, want := range map[*member]bool{a: false, b: true, c: true, d: false} {
		rm := payloads[roomMls](t, e.feedOf(m.id, "room.mls"))
		if len(rm) != 1 || rm[0].HasWelcomeForMe != want || rm[0].Epoch != 1 || rm[0].CommitRef != res.CommitRef {
			t.Fatalf("room.mls: %+v", rm)
		}
	}
	added := 0
	for _, p := range payloads[roomMember](t, e.feedOf(d.id, "room.member")) {
		if p.Change == memberAdded && *p.By == a.id {
			added++
		}
	}
	if added != 2 {
		t.Fatalf("room.member added: %d", added)
	}
	// 古い epoch。
	_, err = func() (CommitResult, error) {
		raw := buildCommit(t, commitSpec{groupId: groupIdOf(room, 0), epoch: 0, sender: 0, adds: [][]byte{e.kp(d.dev(), kpOpts{})}})
		var r CommitResult
		err := e.tx(func(tx *store.Tx) error {
			var err error
			r, err = e.k.Groups.Commit(e.ctx, tx, room, a.dev().deviceId, 0, raw, nil)
			return err
		})
		return r, err
	}()
	e.wantCode(err, cgen.KEY007)
	// Remove（path が要る）の後、右端が空なら切り詰めて、Add は左端の空きへ。
	res, err = e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{2}, path: pathLeaf(t, a.dev(), groupIdOf(room, 0), 0, nil)}, nil)
	e.must(err)
	if res.Removed[0] != c.dev().deviceId {
		t.Fatalf("Removed: %+v", res)
	}
	res, err = e.addAs(room, a.dev(), 0, d.dev(), c.dev())
	e.must(err)
	if l := e.leaves(room); l[2] != d.dev().deviceId || l[3] != c.dev().deviceId || len(l) != 4 {
		t.Fatalf("Remove の後の番号: %v", l)
	}
	// 幅の拡張：4 つが埋まっている時の Add は 4 番。
	e2 := e.addDevice(a, "花子のタブレット", true)
	_, err = e.addAs(room, a.dev(), 0, e2)
	e.must(err)
	if l := e.leaves(room); l[4] != e2.deviceId {
		t.Fatalf("幅の拡張: %v", l)
	}
	page, err := e.k.Groups.CommitsSince(e.ctx, room, 1, 2)
	e.must(err)
	if page.Generation != 0 || len(page.Items) != 2 || !page.HasMore || page.Items[0].Epoch != 2 {
		t.Fatalf("CommitsSince: %+v", page)
	}
	_, err = e.k.Groups.CommitsSince(e.ctx, uuid.Must(uuid.NewV7()), 0, 10)
	e.wantCode(err, cgen.ROOM001)
}

// R: R-12-3-2
// Commit の確かめの 9 つ（第12章 5.2）：1 つずつ壊した Commit は KEY-009 で epoch が進まない。別のトークのグループの識別子も KEY-009。
func TestCommitNineChecks(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	out := e.member("外の人", boundary.RoleMember)
	room := e.room(a.dev(), a, b, c)
	_, err := e.addAs(room, a.dev(), 0, b.dev())
	e.must(err)
	// a の leaf の encryption_key を path で置く。
	aPath := pathLeaf(t, a.dev(), groupIdOf(room, 0), 0, nil)
	_, err = e.commitAs(room, a.dev(), commitSpec{sender: 0, path: aPath}, nil)
	e.must(err)
	gid := groupIdOf(room, 0)
	un := e.addDevice(c, "次郎の新しい電話", false)
	other := e.room(out.dev(), out)
	cases := map[string]func() (CommitResult, error){
		"1 送信者の leaf": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 1, path: pathLeaf(t, a.dev(), gid, 1, nil)}, nil)
		},
		"2 記録に無い Remove": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{5}, path: pathLeaf(t, a.dev(), gid, 0, nil)}, nil)
		},
		"2 送信者の Remove": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{0}, path: pathLeaf(t, a.dev(), gid, 0, nil)}, nil)
		},
		"2 同じ Remove が 2 つ": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{1, 1}, path: pathLeaf(t, a.dev(), gid, 0, nil)}, nil)
		},
		"3 値の Update": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, updates: 1, path: pathLeaf(t, a.dev(), gid, 0, nil)}, nil)
		},
		"4 KeyPackage の署名": func() (CommitResult, error) {
			k := e.kp(c.dev(), kpOpts{after: func(k *keyPackage) { k.signature = []byte{0x30, 0} }})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(mlsSuite, refOf(t, k)))
		},
		"4 有効期間の外（今）": func() (CommitResult, error) {
			now := e.clk.Now()
			k := e.kp(c.dev(), kpOpts{before: func(k *keyPackage) {
				k.leaf.notBefore, k.leaf.notAfter = uint64(now.Add(-100*24*time.Hour).Unix()), uint64(now.Add(-time.Second).Unix())
			}})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(mlsSuite, refOf(t, k)))
		},
		"4 鍵の登録の無い端末": func() (CommitResult, error) {
			k := buildKeyPackage(t, newDevKeys(t, c.id, uuid.Must(uuid.NewV7())), e.clk.Now(), kpOpts{})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(mlsSuite, refOf(t, k)))
		},
		"5 未承認の端末":      func() (CommitResult, error) { return e.addAs(room, a.dev(), 0, un) },
		"5 構成員でない人":     func() (CommitResult, error) { return e.addAs(room, a.dev(), 0, out.dev()) },
		"5 既に leaf を持つ": func() (CommitResult, error) { return e.addAs(room, a.dev(), 0, b.dev()) },
		"5 Add の中の重なり":  func() (CommitResult, error) { return e.addAs(room, a.dev(), 0, c.dev(), c.dev()) },
		"6 記録と同じ encryption_key": func() (CommitResult, error) {
			k := e.kp(c.dev(), kpOpts{before: func(k *keyPackage) { k.leaf.encryptionKey = aPath.leaf.encryptionKey }})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(mlsSuite, refOf(t, k)))
		},
		"7 Remove に path が無い": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{1}}, nil)
		},
		"7 空の Commit に path が無い": func() (CommitResult, error) { return e.commitAs(room, a.dev(), commitSpec{sender: 0}, nil) },
		"8 path の credential": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: pathLeaf(t, a.dev(), gid, 0, func(l *leafNode) {
				l.credential = basicCredential(a.id, b.dev().deviceId)
			})}, nil)
		},
		"8 path の署名鍵": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: pathLeaf(t, a.dev(), gid, 0, func(l *leafNode) { l.signatureKey = sec1(t, b.dev().sig) })}, nil)
		},
		"8 path の source": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: pathLeaf(t, a.dev(), gid, 0, func(l *leafNode) { l.source = leafSourceUpdate })}, nil)
		},
		"8 path の能力": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: pathLeaf(t, a.dev(), gid, 0, func(l *leafNode) { l.capabilities.suites = []uint16{1} })}, nil)
		},
		"8 path の署名の文脈": func() (CommitResult, error) {
			p := pathLeaf(t, a.dev(), gid, 3, nil)
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: p}, nil)
		},
		"8 path の鍵が今と同じ": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: pathLeaf(t, a.dev(), gid, 0, func(l *leafNode) { l.encryptionKey = aPath.leaf.encryptionKey })}, nil)
		},
		"9 Welcome が無い": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{e.kp(c.dev(), kpOpts{})}}, nil)
		},
		"9 Welcome のスイート": func() (CommitResult, error) {
			k := e.kp(c.dev(), kpOpts{})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(1, refOf(t, k)))
		},
		"9 Welcome の宛先": func() (CommitResult, error) {
			k := e.kp(c.dev(), kpOpts{})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(mlsSuite, refOf(t, e.kp(c.dev(), kpOpts{}))))
		},
		"9 Add の無い Welcome": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: pathLeaf(t, a.dev(), gid, 0, nil)}, buildWelcome(mlsSuite, []byte{1}))
		},
		"別のトークの識別子": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{groupId: groupIdOf(other, 0), sender: 0, path: pathLeaf(t, a.dev(), gid, 0, nil)}, nil)
		},
		"Commit の epoch の食い違い": func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{epoch: 9, sender: 0, path: pathLeaf(t, a.dev(), gid, 0, nil)}, nil)
		},
	}
	for what, b := range badHpkeKeys() {
		cases["4 Add の init_key が"+what] = func() (CommitResult, error) {
			k := e.kp(c.dev(), kpOpts{before: func(k *keyPackage) { k.initKey = b }})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(mlsSuite, refOf(t, k)))
		}
		cases["4 Add の encryption_key が"+what] = func() (CommitResult, error) {
			k := e.kp(c.dev(), kpOpts{before: func(k *keyPackage) { k.leaf.encryptionKey = b }})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcome(mlsSuite, refOf(t, k)))
		}
		cases["8 path の encryption_key が"+what] = func() (CommitResult, error) {
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, path: pathLeaf(t, a.dev(), gid, 0, func(l *leafNode) { l.encryptionKey = b })}, nil)
		}
		cases["9 Welcome の kem_output が"+what] = func() (CommitResult, error) {
			k := e.kp(c.dev(), kpOpts{})
			return e.commitAs(room, a.dev(), commitSpec{sender: 0, adds: [][]byte{k}}, buildWelcomeKems(mlsSuite, [][]byte{b}, refOf(t, k)))
		}
	}
	_, before := e.group(room)
	for what, f := range cases {
		_, err := f()
		if err == nil {
			t.Fatalf("%s が通る", what)
		}
		e.wantCode(err, cgen.KEY009)
		if _, ep := e.group(room); ep != before {
			t.Fatalf("%s で epoch が進んだ", what)
		}
	}
	// 外れる端末の Add は同じ Commit なら許す（RFC 9420 12.2）。
	k := e.kp(b.dev(), kpOpts{})
	res, err := e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{1}, adds: [][]byte{k}, path: pathLeaf(t, a.dev(), gid, 0, nil)},
		buildWelcome(mlsSuite, refOf(t, k)))
	e.must(err)
	if !slices.Equal(res.Added, []uuid.UUID{b.dev().deviceId}) || !slices.Equal(res.Removed, []uuid.UUID{b.dev().deviceId}) {
		t.Fatalf("Remove と Add の同じ端末: %+v", res)
	}
}

// R: R-12-3-2
// 拒みの報告と再参加（第12章 5.2 の復旧の 1、第2章 5.5）：世代の食い違いは KEY-007、epoch の範囲と commit_ref の食い違いと leaf の無い端末は
// KEY-009。報告で再参加待ち（pending_readd）。許す時刻は、他の全 leaf が拒んだか 2 回目なら今、他は 10 分後。Remove と Add を 1 つの Commit
// で行うと待ちが解ける。
func TestRejectAndReadd(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b, c)
	res, err := e.addAs(room, a.dev(), 0, b.dev(), c.dev())
	e.must(err)
	reject := func(dk *devKeys, gen int, epoch *int64, ref *string) (RejectResult, error) {
		var r RejectResult
		err := e.tx(func(tx *store.Tx) error {
			var err error
			r, err = e.k.Groups.Reject(e.ctx, tx, room, dk.deviceId, gen, epoch, ref, "confirmation_tag")
			return err
		})
		return r, err
	}
	zero, two := int64(0), int64(2)
	badRef := "x"
	_, err = reject(b.dev(), 1, nil, nil)
	e.wantCode(err, cgen.KEY007)
	_, err = reject(b.dev(), 0, &zero, nil)
	e.wantCode(err, cgen.KEY009)
	_, err = reject(b.dev(), 0, &two, nil)
	e.wantCode(err, cgen.KEY009)
	_, err = reject(b.dev(), 0, nil, &badRef)
	e.wantCode(err, cgen.KEY009)
	nd := e.addDevice(c, "次郎のタブレット", true)
	_, err = reject(nd, 0, nil, nil)
	e.wantCode(err, cgen.KEY009)
	r, err := reject(b.dev(), 0, nil, &res.CommitRef)
	e.must(err)
	if want := e.clk.Now().Add(reinitWait); !r.ReinitAllowedAt.Equal(want.UTC()) {
		t.Fatalf("許す時刻: %v %v", r.ReinitAllowedAt, want)
	}
	info, err := e.k.Groups.Epoch(e.ctx, room)
	e.must(err)
	if !slices.Equal(info.PendingReadd, []uuid.UUID{b.dev().deviceId}) {
		t.Fatalf("再参加待ち: %+v", info)
	}
	readd := 0
	for _, p := range payloads[roomMember](t, e.feedOf(a.id, "room.member")) {
		if p.Change == memberPendingReadd {
			readd++
		}
	}
	if readd != 1 {
		t.Fatalf("pending_readd の room.member: %d", readd)
	}
	// 他の全 leaf（Commit を作った a を除く b と c）が拒めば今。
	r, err = reject(c.dev(), 0, nil, nil)
	e.must(err)
	if !r.ReinitAllowedAt.Equal(e.clk.Now().UTC()) {
		t.Fatalf("全員が拒んだ時の時刻: %v", r.ReinitAllowedAt)
	}
	// 再参加：a が b の Remove と新しい KeyPackage の Add を 1 つの Commit で。
	k := e.kp(b.dev(), kpOpts{})
	_, err = e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{1}, adds: [][]byte{k}, path: pathLeaf(t, a.dev(), groupIdOf(room, 0), 0, nil)},
		buildWelcome(mlsSuite, refOf(t, k)))
	e.must(err)
	info, err = e.k.Groups.Epoch(e.ctx, room)
	e.must(err)
	if slices.Contains(info.PendingReadd, b.dev().deviceId) || !slices.Contains(info.PendingReadd, c.dev().deviceId) {
		t.Fatalf("再参加の後の待ち: %+v", info)
	}
}

// R: R-12-3-2
// 再参加待ちの端末を Remove だけする Commit（第12章 5.2 の復旧の 1。引き継ぐ端末がその端末の KeyPackage を取れない時）：行は追加待ち
// （add、since は Commit の時刻）に替わり、room.member は removed の後に pending_add。その後の普通の Add で追加待ちが解ける。
func TestReaddRemovedOnly(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b, c)
	_, err := e.addAs(room, a.dev(), 0, b.dev(), c.dev())
	e.must(err)
	cd := c.dev().deviceId
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Reject(e.ctx, tx, room, cd, 0, nil, nil, "confirmation_tag")
		return err
	}))
	e.clk.Advance(time.Minute)
	at := e.clk.Now()
	var leaf uint32
	for i, d := range e.leaves(room) {
		if d == cd {
			leaf = uint32(i)
		}
	}
	gid := groupIdOf(room, 0)
	res, err := e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{leaf}, path: pathLeaf(t, a.dev(), gid, 0, nil)}, nil)
	e.must(err)
	if !slices.Equal(res.Removed, []uuid.UUID{cd}) || len(res.Added) != 0 {
		t.Fatalf("Remove だけの Commit: %+v", res)
	}
	var kind string
	var since time.Time
	e.scalar(&kind, `SELECT kind FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, cd)
	e.scalar(&since, `SELECT since FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, cd)
	if kind != "add" || !since.Equal(at.Truncate(time.Microsecond)) {
		t.Fatalf("追加待ちに替わらない: %s %v（%v）", kind, since, at)
	}
	info, err := e.k.Groups.Epoch(e.ctx, room)
	e.must(err)
	if !slices.Equal(info.PendingAddDevices, []uuid.UUID{cd}) || len(info.PendingReadd) != 0 {
		t.Fatalf("Remove だけの後の待ち: %+v", info)
	}
	for _, m := range []*member{a, b, c} {
		var changes []string
		for _, p := range payloads[roomMember](t, e.feedOf(m.id, "room.member")) {
			if p.DeviceId != nil && *p.DeviceId == cd {
				changes = append(changes, p.Change)
			}
		}
		if want := []string{memberAdded, memberPendingReadd, memberRemoved, memberPendingAdd}; !slices.Equal(changes, want) {
			t.Fatalf("%s の room.member: %v", m.id, changes)
		}
	}
	// 普通の Add で戻る。
	_, err = e.addAs(room, a.dev(), 0, c.dev())
	e.must(err)
	if n := e.count(`SELECT count(*) FROM mls_pending WHERE room_id = $1`, room); n != 0 {
		t.Fatalf("Add の後に待ちが残る: %d", n)
	}
	found := false
	for _, d := range e.leaves(room) {
		found = found || d == cd
	}
	if !found {
		t.Fatal("Add の後に leaf が無い")
	}
}

// R: R-12-3-2
// 再初期化（第12章 5.2 の復旧の 2）：再参加待ちでなければ not_pending、条件の前は too_early（allowed_at）、10 分が過ぎ Commit が無ければ許す。
// 世代が 1 進み epoch 0、呼んだ端末が leaf 0、前の世代の記録が消え、入るべき端末（構成員の承認済みの端末）が追加待ちになり、room.mls と知らせ
// mls_reinit。古い世代の Commit は KEY-007、本文は ROOM-006。24 時間に 3 回目は limit と知らせ mls_reinit_limit（要求が失敗しても残る）。
func TestReinit(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	b2 := e.addDevice(b, "太郎のタブレット", true)
	room := e.room(a.dev(), a, b, c)
	_, err := e.addAs(room, a.dev(), 0, b.dev(), c.dev())
	e.must(err)
	reinit := func(dk *devKeys) (ReinitResult, error) {
		var r ReinitResult
		err := e.tx(func(tx *store.Tx) error {
			var err error
			r, err = e.k.Groups.Reinit(e.ctx, tx, room, dk.deviceId)
			return err
		})
		return r, err
	}
	reject := func(dk *devKeys) {
		e.must(e.tx(func(tx *store.Tx) error {
			_, err := e.k.Groups.Reject(e.ctx, tx, room, dk.deviceId, e.mustGen(room), nil, nil, "other")
			return err
		}))
	}
	_, err = reinit(b.dev())
	if ev := e.wantCode(err, cgen.KEY013); ev.Params["reason"] != "not_pending" || ev.Retryable {
		t.Fatalf("not_pending: %+v", ev)
	}
	reject(b.dev())
	_, err = reinit(b.dev())
	if ev := e.wantCode(err, cgen.KEY013); ev.Params["reason"] != "too_early" || ev.Params["allowed_at"] == nil || !ev.Retryable {
		t.Fatalf("too_early: %+v", ev)
	}
	e.clk.Advance(reinitWait)
	r, err := reinit(b.dev())
	e.must(err)
	if r.Generation != 1 || r.Epoch != 0 || !bytes.Equal(r.GroupId, groupIdOf(room, 1)) {
		t.Fatalf("ReinitResult: %+v", r)
	}
	if l := e.leaves(room); len(l) != 1 || l[0] != b.dev().deviceId {
		t.Fatalf("新しい世代の leaf: %v", l)
	}
	if n := e.count(`SELECT count(*) FROM mls_commits WHERE room_id = $1`, room); n != 0 {
		t.Fatalf("前の世代の Commit が残る: %d", n)
	}
	info, err := e.k.Groups.Epoch(e.ctx, room)
	e.must(err)
	want := sortedIds([]uuid.UUID{a.dev().deviceId, b2.deviceId, c.dev().deviceId})
	if !slices.Equal(info.PendingAddDevices, want) || len(info.PendingReadd) != 0 || info.Generation != 1 {
		t.Fatalf("新しい世代の待ち: %+v", info)
	}
	rm := payloads[roomMls](t, e.feedOf(c.id, "room.mls"))
	if last := rm[len(rm)-1]; last.Generation != 1 || last.Epoch != 0 || last.CommitRef != "" {
		t.Fatalf("room.mls: %+v", last)
	}
	if n := e.notices.of(noticeMlsReinit); len(n) != 1 || n[0].audience != identity.AudienceAdmins || n[0].params["generation"] != 1 {
		t.Fatalf("mls_reinit: %+v", n)
	}
	// 古い世代の Commit と本文。
	_, err = e.commitAs(room, a.dev(), commitSpec{groupId: groupIdOf(room, 0), sender: 0, path: pathLeaf(t, a.dev(), groupIdOf(room, 0), 0, nil)}, nil)
	e.wantCode(err, cgen.KEY007)
	body := gen.FtEventMls1{GroupId: b64.EncodeToString(groupIdOf(room, 0)), Epoch: 0, Ciphertext: b64.EncodeToString(buildPrivate(groupIdOf(room, 0), 0, contentApplication))}
	e.wantCode(e.k.Groups.CheckSender(e.ctx, e.pool, room, b.dev().deviceId, body), cgen.ROOM006)
	// 2 回目と 3 回目：新しい世代で b が a を Add し、違う 2 つの epoch で拒めば即時（条件 (b)。報告は epoch ごとに 1 つ）。
	twice := func() {
		gen := e.mustGen(room)
		_, err := e.addAs(room, b.dev(), 0, a.dev())
		e.must(err)
		reject(b.dev())
		_, err = e.commitAs(room, b.dev(), commitSpec{sender: 0, path: pathLeaf(t, b.dev(), groupIdOf(room, gen), 0, nil)}, nil)
		e.must(err)
		reject(b.dev())
	}
	twice()
	r, err = reinit(b.dev())
	e.must(err)
	twice()
	_, err = reinit(b.dev())
	if ev := e.wantCode(err, cgen.KEY013); ev.Params["reason"] != "limit" || ev.Retryable {
		t.Fatalf("limit: %+v", ev)
	}
	if n := e.notices.of(noticeMlsReinitLimit); len(n) != 1 || n[0].params["room_id"] != room.String() {
		t.Fatalf("mls_reinit_limit: %+v", n)
	}
	if g, _ := e.group(room); g != 2 {
		t.Fatalf("上限で世代が進んだ: %d", g)
	}
	e.clk.Advance(24 * time.Hour)
	_, err = reinit(b.dev())
	e.must(err)
}

// mustGen は今の世代。
func (e *env) mustGen(room uuid.UUID) int {
	g, _ := e.group(room)
	return g
}

// R: R-12-3-2
// 外形の検査（第12章 5.2）：本文の外側の形、group_id の食い違い、世代と epoch、leaf の無い端末、グループの無いトーク。
func TestCheckSender(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b)
	gid := groupIdOf(room, 0)
	body := func(outer []byte, inner []byte, epoch int, ct uint8, msg []byte) gen.FtEventMls1 {
		if msg == nil {
			msg = buildPrivate(outer, uint64(epoch), ct)
		}
		return gen.FtEventMls1{GroupId: b64.EncodeToString(inner), Epoch: epoch, Ciphertext: b64.EncodeToString(msg)}
	}
	check := func(dk *devKeys, room uuid.UUID, b gen.FtEventMls1) error {
		return e.k.Groups.CheckSender(e.ctx, e.pool, room, dk.deviceId, b)
	}
	e.must(check(a.dev(), room, body(gid, gid, 0, contentApplication, nil)))
	bad := body(gid, gid, 0, contentApplication, nil)
	bad.Ciphertext = "!"
	e.wantCode(check(a.dev(), room, bad), cgen.ROOM005)
	e.wantCode(check(a.dev(), room, body(gid, gid, 0, contentApplication, e.kp(a.dev(), kpOpts{}))), cgen.ROOM005)
	e.wantCode(check(a.dev(), room, body(gid, gid, 0, contentCommit, nil)), cgen.ROOM005)
	e.wantCode(check(a.dev(), room, body(gid, groupIdOf(room, 1), 0, contentApplication, nil)), cgen.ROOM005)
	other := uuid.Must(uuid.NewV7())
	e.wantCode(check(a.dev(), room, body(groupIdOf(other, 0), groupIdOf(other, 0), 0, contentApplication, nil)), cgen.ROOM005)
	e.wantCode(check(a.dev(), room, body(gid[:16], gid[:16], 0, contentApplication, nil)), cgen.ROOM005)
	e.wantCode(check(a.dev(), room, body(groupIdOf(room, 1), groupIdOf(room, 1), 0, contentApplication, nil)), cgen.ROOM006)
	e.wantCode(check(a.dev(), room, body(gid, gid, 1, contentApplication, nil)), cgen.ROOM006)
	e.wantCode(check(b.dev(), room, body(gid, gid, 0, contentApplication, nil)), cgen.ROOM006)
	e.wantCode(check(a.dev(), other, body(groupIdOf(other, 0), groupIdOf(other, 0), 0, contentApplication, nil)), cgen.ROOM001)
}

// R: R-3-4-2
// 端末の取り消しと削除・抹消・停止・休眠の hook（第12章 5.2、9、I-8）：取り消しで鍵の登録、KeyPackage、コピー (a) と片、追加待ちが消え、leaf の
// あるトークに削除待ち（再参加待ちも削除待ちに）。削除で全端末の削除待ちと、その端末へ包んだ他人の片が消え、本人のコピーは残る。抹消で本人の
// コピーと照合の値と鍵のバックアップが消え、署名鍵が失効する。停止で追加待ちが消える。休眠は判定の後に使われた端末を変えず、コピー (a) だけ
// を消して削除待ちを置き、Add で承認済みへ戻る。停止で未承認に戻った端末も休眠の対象で、削除待ちが立ちコピー (a) が消え、状態は未承認の
// まま（第12章 5.2）。
func TestLifecycleHooks(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b, c)
	_, err := e.addAs(room, a.dev(), 0, b.dev())
	e.must(err)
	// 取り消し：b の端末（leaf 1、再参加待ち）。
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Reject(e.ctx, tx, room, b.dev().deviceId, 0, nil, nil, "other")
		return err
	}))
	_, err = e.k.KeyPackages.Count(e.ctx, b.dev().deviceId)
	e.must(err)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.KeyPackages.Push(e.ctx, tx, b.dev().deviceId, [][]byte{e.kp(b.dev(), kpOpts{})}, nil)
		return err
	}))
	bd := b.dev().deviceId.String()
	e.must(e.putCopy(b.id, b.dev(), BundleDevice, bd, copyOf(b.id, BundleDevice, bd, 1, 0)))
	e.must(e.putCopy(a.id, a.dev(), BundleShare, bd, copyOf(a.id, BundleShare, bd, 1, 1)))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.Revoke(e.ctx, tx, b.dev().deviceId, a.id, false) }))
	for what, sql := range map[string]string{
		"鍵の登録":       `SELECT count(*) FROM device_keys WHERE device_id = $1`,
		"KeyPackage": `SELECT count(*) FROM keypackages WHERE device_id = $1`,
		"コピーと片":      `SELECT count(*) FROM keybundles WHERE target_id = $1::text`,
	} {
		if n := e.count(sql, b.dev().deviceId); n != 0 {
			t.Fatalf("取り消しで %s が残る: %d", what, n)
		}
	}
	var kind string
	e.scalar(&kind, `SELECT kind FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, b.dev().deviceId)
	if kind != "remove" {
		t.Fatalf("取り消しの後の待ち: %s", kind)
	}
	// 停止：c の新しい端末の追加待ちが消える（leaf と削除待ちは触らない）。
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.RequestAdd(e.ctx, tx, c.dev().deviceId)
		return err
	}))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, c.id, a.id, "") }))
	if n := e.count(`SELECT count(*) FROM mls_pending WHERE device_id = $1`, c.dev().deviceId); n != 0 {
		t.Fatalf("停止で追加待ちが残る: %d", n)
	}
	// 削除と抹消：d の端末へ包んだ a の片は削除で消え、d のコピーは削除で残り抹消で消える。
	d := e.member("三郎", boundary.RoleMember)
	e.rooms.set(room, d.id, "active")
	_, err = e.addAs(room, a.dev(), 0, d.dev())
	e.must(err)
	dd := d.dev().deviceId.String()
	e.must(e.putCopy(a.id, a.dev(), BundleShare, dd, copyOf(a.id, BundleShare, dd, 1, 2)))
	e.must(e.putCopy(d.id, d.dev(), BundleDevice, dd, copyOf(d.id, BundleDevice, dd, 1, 0)))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, d.id, a.id, false) }))
	if n := e.count(`SELECT count(*) FROM keybundles WHERE user_id = $1 AND kind = 'share' AND target_id = $2`, a.id, dd); n != 0 {
		t.Fatalf("削除された人の端末へ包んだ片が残る: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM mls_pending WHERE device_id = $1 AND kind = 'remove'`, d.dev().deviceId); n != 1 {
		t.Fatalf("削除で削除待ちが立たない: %d", n)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Hooks.OnUserErased(e.ctx, tx, d.id) }))
	if n := e.count(`SELECT count(*) FROM keybundles WHERE user_id = $1`, d.id); n != 0 {
		t.Fatalf("抹消で本人のコピーが残る: %d", n)
	}
	if f, _ := e.k.UserSigningKeys.Get(e.ctx, e.pool, d.id); f != nil {
		t.Fatal("抹消で署名鍵が失効しない")
	}
	// 休眠。
	f := e.member("四郎", boundary.RoleMember)
	e.rooms.set(room, f.id, "active")
	_, err = e.addAs(room, a.dev(), 0, f.dev())
	e.must(err)
	fd := f.dev().deviceId.String()
	e.must(e.putCopy(f.id, f.dev(), BundleDevice, fd, copyOf(f.id, BundleDevice, fd, 1, 0)))
	e.must(e.putCopy(a.id, a.dev(), BundleShare, fd, copyOf(a.id, BundleShare, fd, 1, 3)))
	if _, err := e.pool.Exec(e.ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, f.dev().deviceId, e.clk.Now().Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cutoff := e.clk.Now().Add(-90 * 24 * time.Hour)
	dl, err := e.k.Groups.DormantLeafs(e.ctx, cutoff)
	e.must(err)
	if len(dl) != 1 || dl[0].DeviceId != f.dev().deviceId || dl[0].RoomId != room {
		t.Fatalf("DormantLeafs: %+v", dl)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.RetireDormant(e.ctx, tx, a.dev().deviceId, cutoff) }))
	if e.devState(a.dev().deviceId) != identity.DeviceApproved {
		t.Fatal("最近使った端末を休眠にした")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.RetireDormant(e.ctx, tx, f.dev().deviceId, cutoff) }))
	if e.devState(f.dev().deviceId) != identity.DeviceDormant {
		t.Fatal("休眠にならない")
	}
	if e.count(`SELECT count(*) FROM keybundles WHERE target_id = $1 AND kind = 'device'`, fd) != 0 ||
		e.count(`SELECT count(*) FROM keybundles WHERE target_id = $1 AND kind = 'share'`, fd) != 1 {
		t.Fatal("休眠でコピー (a) だけが消えるのでない")
	}
	// 休眠の端末の Remove と Add（rejoin）で承認済みへ戻る。
	l := e.leaves(room)
	var fl uint32
	for i, dv := range l {
		if dv == f.dev().deviceId {
			fl = uint32(i)
		}
	}
	k := e.kp(f.dev(), kpOpts{})
	_, err = e.commitAs(room, a.dev(), commitSpec{sender: 0, removes: []uint32{fl}, adds: [][]byte{k}, path: pathLeaf(t, a.dev(), groupIdOf(room, 0), 0, nil)},
		buildWelcome(mlsSuite, refOf(t, k)))
	e.must(err)
	if e.devState(f.dev().deviceId) != identity.DeviceApproved {
		t.Fatal("休眠の端末の Add で承認済みへ戻らない")
	}
	// 停止で未承認に戻った端末の休眠。
	g := e.member("五郎", boundary.RoleMember)
	e.rooms.set(room, g.id, "active")
	_, err = e.addAs(room, a.dev(), 0, g.dev())
	e.must(err)
	gd := g.dev().deviceId.String()
	e.must(e.putCopy(g.id, g.dev(), BundleDevice, gd, copyOf(g.id, BundleDevice, gd, 1, 0)))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, g.id, a.id, "") }))
	if _, err := e.pool.Exec(e.ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, g.dev().deviceId, e.clk.Now().Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	dl, err = e.k.Groups.DormantLeafs(e.ctx, cutoff)
	e.must(err)
	if !slices.Contains(dl, RoomDevice{RoomId: room, DeviceId: g.dev().deviceId}) {
		t.Fatalf("停止で戻った端末の leaf が DormantLeafs に無い: %+v", dl)
	}
	if e.pendingOf(room, g.dev().deviceId) != "" {
		t.Fatal("停止で leaf に待ちが立った")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.RetireDormant(e.ctx, tx, g.dev().deviceId, cutoff) }))
	if e.devState(g.dev().deviceId) != identity.DeviceUnapproved || e.pendingOf(room, g.dev().deviceId) != "remove" ||
		e.count(`SELECT count(*) FROM keybundles WHERE target_id = $1 AND kind = 'device'`, gd) != 0 {
		t.Fatalf("停止で戻った端末の休眠：状態 %s、待ち %q", e.devState(g.dev().deviceId), e.pendingOf(room, g.dev().deviceId))
	}
}

// R: R-3-4-2
// 取り消しと Commit の同時（I-8）：Commit が先に Add の端末を押さえれば、取り消しは Commit の後に走り、新しい leaf に削除待ちを置く。取り消しが
// 先なら、Commit は取り消しの後に端末を読み KEY-009。
func TestRevokeAndCommitConcurrency(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	room := e.room(a.dev(), a, b, c)
	// 1. Commit が先：Commit のトランザクションを開いたまま取り消しを始めると、取り消しは待つ。
	k := e.kp(b.dev(), kpOpts{})
	raw := buildCommit(t, commitSpec{groupId: groupIdOf(room, 0), epoch: 0, sender: 0, adds: [][]byte{k}})
	ctx := e.ctx
	tx1, err := e.pool.Begin(ctx)
	e.must(err)
	_, err = e.k.Groups.Commit(ctx, tx1, room, a.dev().deviceId, 0, raw, buildWelcome(mlsSuite, refOf(t, k)))
	e.must(err)
	done := make(chan error, 1)
	go func() {
		done <- e.pool.InTx(ctx, func(tx *store.Tx) error { return e.id.Devices.Revoke(ctx, tx, b.dev().deviceId, a.id, false) })
	}()
	select {
	case err := <-done:
		t.Fatalf("取り消しが Commit を待たない: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	e.must(tx1.Commit(ctx))
	e.must(<-done)
	var kind string
	e.scalar(&kind, `SELECT kind FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, b.dev().deviceId)
	if kind != "remove" {
		t.Fatalf("Commit の後の取り消しで削除待ちが立たない: %s", kind)
	}
	// 2. 取り消しが先：取り消しのトランザクションを開いたまま Commit を始めると、Commit は待ち、取り消しの後に KEY-009。
	k = e.kp(c.dev(), kpOpts{})
	_, ep := e.group(room)
	raw = buildCommit(t, commitSpec{groupId: groupIdOf(room, 0), epoch: uint64(ep), sender: 0, adds: [][]byte{k}})
	tx2, err := e.pool.Begin(ctx)
	e.must(err)
	e.must(e.id.Devices.Revoke(ctx, tx2, c.dev().deviceId, a.id, false))
	go func() {
		done <- e.pool.InTx(ctx, func(tx *store.Tx) error {
			_, err := e.k.Groups.Commit(ctx, tx, room, a.dev().deviceId, ep, raw, buildWelcome(mlsSuite, refOf(t, k)))
			return err
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("Commit が取り消しを待たない: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	e.must(tx2.Commit(ctx))
	e.wantCode(<-done, cgen.KEY009)
	if _, ep2 := e.group(room); ep2 != ep {
		t.Fatal("取り消された端末の Add で epoch が進んだ")
	}
}

// R: R-12-3-2
// パスキーの hook（第12章 5.4 (e)、第3章 4.6）：パスキーの削除でその資格情報のコピー (e) が消え（無くても誤りにしない）、再設定のリンクの使用で
// recovery.used（reset）と知らせ recovery_used が本人と管理者へ出る。
func TestPasskeyHooks(t *testing.T) {
	e := newEnv(t)
	admin := e.member("花子", boundary.RoleAdmin)
	a := e.member("太郎", boundary.RoleMember)
	cred := []byte("cred-9")
	target := b64.EncodeToString(cred)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO keybundles (user_id, kind, target_id, key_version, body, created_at) VALUES ($1, 'prf', $2, 1, $3, $4)`,
		a.id, target, copyOf(a.id, BundlePrf, target, 1, 0), e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Hooks.OnPasskeyDeleted(e.ctx, tx, a.id, cred) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Hooks.OnPasskeyDeleted(e.ctx, tx, a.id, cred) }))
	if n := e.count(`SELECT count(*) FROM keybundles WHERE user_id = $1`, a.id); n != 0 {
		t.Fatalf("コピー (e) が残る: %d", n)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Hooks.OnPasskeyReset(e.ctx, tx, a.id, a.dev().deviceId) }))
	for _, m := range []*member{a, admin} {
		used := payloads[recoveryUsed](t, e.feedOf(m.id, "recovery.used"))
		if len(used) != 1 || used[0].Method != MethodReset || used[0].Result != ResultSuccess {
			t.Fatalf("recovery.used: %+v", used)
		}
	}
	if n := e.notices.of(noticeRecoveryUsed); len(n) != 2 || n[0].params["method"] != "reset" {
		t.Fatalf("recovery_used: %+v", n)
	}
}
