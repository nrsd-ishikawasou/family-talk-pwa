// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package identity

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 同時の要求の試験。止まらないことは 10 秒で終わることで確かめる（PostgreSQL の deadlock の検出は 1 秒で、検出されれば
// 片方が SRV-003 になる。アプリの中の待ち合いは検出されず時間切れまで止まる）。

const stuck = 10 * time.Second

// rendezvous は n 個の待ち合わせ。arm の後に hook を通ったトランザクションが n 個そろうまで待たせる。
type rendezvous struct {
	armed   atomic.Bool
	arrived sync.WaitGroup
	release chan struct{}
}

func newRendezvous(n int) *rendezvous {
	r := &rendezvous{release: make(chan struct{})}
	r.arrived.Add(n)
	return r
}

// wait は hook の中から呼ぶ。そろうまで（最大 10 秒）待つ。
func (r *rendezvous) wait() {
	if !r.armed.Load() {
		return
	}
	r.arrived.Done()
	select {
	case <-r.release:
	case <-time.After(stuck):
	}
}

// R: R-2-2-3
// 同時の 2 つの端末の取り消し（T-L1-03。全体実装設計書 2.4 の採番の錠の順）：構成員 A（識別子が小）と C（大）が同時に自分の他の端末を
// 取り消す。両方のトランザクションが本人のその端末への session.revoked を溜めた所で待ち合わせ、続けて家族の全員へ device.changed を溜めて
// コミットする（宛先を逆の順に持つ 2 つの書き込み）。両方が成功し（40P01 なし）、A と C の feed に自分のトランザクションの
// session.revoked と device.changed が積んだ順の連続の pos で入り、相手の device.changed も 1 行ずつ入る。
func TestConcurrentDeviceRevokesAcrossMembers(t *testing.T) {
	rv := newRendezvous(2)
	e := newEnv(t, func(h *Hooks) {
		h.RegisterOnDeviceRevoked(func(context.Context, *store.Tx, uuid.UUID) error { rv.wait(); return nil })
	})
	a := e.setupAdmin("花子")
	b := e.join(a, "太郎", nil)
	c := e.join(a, "次郎", nil)
	if bytes.Compare(a.id[:], c.id[:]) > 0 {
		t.Fatal("作った順の識別子でない（試験の前提）")
	}
	second := func(m *member) uuid.UUID {
		is, err := e.login(m, false, nil)
		e.must(err)
		return uuid.FromStringOrNil(is.Result.Session.DeviceId)
	}
	a2, c2 := second(a), second(c)
	ctxA, ctxC := e.as(a), e.as(c)
	dapi := e.id.Apis().Devices
	rv.armed.Store(true)
	errs := make(chan error, 2)
	go func() {
		_, err := dapi.DeleteMyDevice(ctxA, gen.DeleteMyDeviceRequestObject{Id: a2.String()})
		errs <- err
	}()
	go func() {
		_, err := dapi.DeleteMyDevice(ctxC, gen.DeleteMyDeviceRequestObject{Id: c2.String()})
		errs <- err
	}()
	within(t, stuck, func() bool { rv.arrived.Wait(); return true })
	close(rv.release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("同時の取り消しのどちらかが失敗した（デッドロックなら SRV-003）: %v", err)
			}
		case <-time.After(stuck):
			t.Fatal("同時の取り消しが終わらない")
		}
	}
	check := func(m *member, own, other uuid.UUID) {
		t.Helper()
		// 取り消した端末から見える行（宛先の無い行とその端末宛ての行）。
		rows := e.feedOf(m.id, own)
		var revokedAt, ownChanged, otherChanged = -1, 0, 0
		for i, r := range rows {
			switch {
			case r.Ref.Kind == "session.revoked" && r.Ref.DeviceId == own:
				revokedAt = i
			case r.Ref.Kind == "device.changed" && strings.Contains(string(r.Ref.Payload), own.String()) && strings.Contains(string(r.Ref.Payload), `"revoked"`):
				if revokedAt < 0 || i != revokedAt+1 || r.Pos != rows[revokedAt].Pos+1 {
					t.Fatalf("%s の session.revoked の直後の pos に自分の device.changed が無い: %v", m.name, kinds(rows))
				}
				ownChanged++
			case r.Ref.Kind == "device.changed" && strings.Contains(string(r.Ref.Payload), other.String()) && strings.Contains(string(r.Ref.Payload), `"revoked"`):
				otherChanged++
			}
		}
		if revokedAt < 0 || ownChanged != 1 || otherChanged != 1 {
			t.Fatalf("%s の feed が違う: session.revoked=%d 自分=%d 相手=%d %v", m.name, revokedAt, ownChanged, otherChanged, kinds(rows))
		}
	}
	check(a, a2, c2)
	check(c, c2, a2)
	var bothToB int
	for _, r := range e.feedOf(b.id, uuid.Nil) {
		if r.Ref.Kind == "device.changed" && strings.Contains(string(r.Ref.Payload), `"revoked"`) {
			bothToB++
		}
	}
	if bothToB != 2 {
		t.Fatalf("第三者に 2 つの device.changed が届かない: %d", bothToB)
	}
}

// R: R-3-1-2
// 招待の発行の上限の同時（第3章 3.2。設定 auth.unused_invites_max）：上限の 1 つ手前で同じ発行者の 2 つの発行が重なると、1 つ目のトランザクションが発行者の行の錠を
// 持つ間 2 つ目は待ち、1 つ目のコミットの後に数え直して MEM-004 になる（上限を超えない）。
func TestInviteIssueLimitSerializedPerIssuer(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	max := int(e.cfg.Get(cgen.KeyAuthUnusedInvitesMax).Int())
	for i := 0; i < max-1; i++ {
		e.issueInvite(admin, InviteSpec{Role: boundary.RoleMember})
	}
	tx1, err := e.pool.Begin(e.ctx)
	e.must(err)
	defer tx1.Rollback(e.ctx)
	_, _, err = e.id.Invites.Issue(e.ctx, tx1, admin.id, InviteSpec{Role: boundary.RoleMember})
	e.must(err)
	done := make(chan error, 1)
	go func() {
		done <- e.tx(func(tx *store.Tx) error {
			_, _, err := e.id.Invites.Issue(e.ctx, tx, admin.id, InviteSpec{Role: boundary.RoleMember})
			return err
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("1 つ目のトランザクションの間に 2 つ目の発行が終わった（直列でない）: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	e.must(tx1.Commit(e.ctx))
	select {
	case err := <-done:
		e.wantCode(err, cgen.MEM004)
	case <-time.After(stuck):
		t.Fatal("2 つ目の発行が終わらない")
	}
	if n := e.count(`SELECT count(*) FROM invites WHERE issuer = $1 AND revoked_at IS NULL`, admin.id); n != max {
		t.Fatalf("未使用の招待が上限 %d でない: %d", max, n)
	}
}

// R: R-3-6-1
// 子の抹消の求め（第3章 8.3）：要求のトランザクションが子の users の行の錠を持ったまま、別のトランザクションで
// guardian_approvals（外部キーで users を指す）を確定しても待ち合わず、MEM-006 を返す。要求のトランザクションが戻っても求めと
// 保護者への知らせは残る。同じ求めの再送は求めを増やさない。
func TestChildEraseRequestDoesNotWaitOnOwnRowLock(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	kid := e.join(admin, "孫", &ChildSpec{IsChild: true})
	for i := 0; i < 2; i++ {
		err := within(t, stuck, func() error {
			return e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, kid.id); return err })
		})
		e.wantCode(err, cgen.MEM006)
	}
	if n := e.count(`SELECT count(*) FROM guardian_approvals WHERE child_id = $1 AND kind = 'erase' AND state = 'open'`, kid.id); n != 1 {
		t.Fatalf("抹消の承認の求めが 1 つでない: %d", n)
	}
	if got := e.notices.to(noticeEraseApprovalRequested); len(got) != 1 || got[0] != "guardians:"+kid.id.String() {
		t.Fatalf("保護者への求めの知らせが 1 回でない: %v", got)
	}
	var st string
	e.scalar(&st, `SELECT state FROM users WHERE id = $1`, kid.id)
	if st != string(UserNormal) {
		t.Fatalf("承認の前に抹消の猶予が始まった: %s", st)
	}
}

// R: R-1-6-3
// 期限切れの引き継ぎの受諾（第3章 8.4）：求めの行の錠を持ったまま止まらず AUTH-007、失効の書き込みは
// ExpireTransfers の 1 か所。期限内の受諾は役割を入れ替え、user.changed を 2 人分と監査。
func TestExpiredTransferAcceptReturnsWithoutWaiting(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	transfer := func() uuid.UUID {
		var id uuid.UUID
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			id, err = e.id.Users.Transfer(e.ctx, tx, admin.id, m.id)
			return err
		}))
		return id
	}
	id := transfer()
	if got := e.notices.of(noticeTransferRequested); len(got) != 1 || got[0].user != m.id || got[0].params["from_name"] != "花子" {
		t.Fatalf("引き継ぎの求めの知らせが違う: %+v", got)
	}
	e.clk.Advance(transferTtl + time.Hour)
	p := e.principal(m)
	err := within(t, stuck, func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Users.AcceptTransfer(e.ctx, tx, p, id) })
	})
	e.wantCode(err, cgen.AUTH007)
	var st string
	e.scalar(&st, `SELECT state FROM admin_transfers WHERE id = $1`, id)
	if st != "open" {
		t.Fatalf("要求の経路が失効を書いた: %s", st)
	}
	if n, err := e.id.Users.ExpireTransfers(e.ctx); err != nil || n != 1 {
		t.Fatalf("ExpireTransfers が 1 つを失効させない: %d %v", n, err)
	}
	e.scalar(&st, `SELECT state FROM admin_transfers WHERE id = $1`, id)
	if st != "expired" {
		t.Fatalf("期限切れの求めが expired でない: %s", st)
	}
	id = transfer()
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Users.AcceptTransfer(e.ctx, tx, e.principal(admin), id) }), cgen.AUTH007)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.AcceptTransfer(e.ctx, tx, e.principal(m), id) }))
	var ra, rm string
	e.scalar(&ra, `SELECT role FROM users WHERE id = $1`, admin.id)
	e.scalar(&rm, `SELECT role FROM users WHERE id = $1`, m.id)
	if ra != "member" || rm != "admin" {
		t.Fatalf("引き継ぎで役割が入れ替わらない: %s %s", ra, rm)
	}
	acts := e.audit.actions()
	if acts[len(acts)-1] != auditTransferAccepted {
		t.Fatalf("引き継ぎの受諾を監査に残さない: %v", acts)
	}
}

// R: R-3-6-1
// 期限切れの保護者の承認（第3章 8.3）：承認は止まらず AUTH-007 で失効を書かない。ExpireDue が 1 回だけ失効させ、
// 子と保護者へ erase_approval_expired を 1 回ずつ。2 回目の ExpireDue は何もしない。
func TestExpiredGuardianApprovalReturnsWithoutWaiting(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	kid := e.join(admin, "孫", &ChildSpec{IsChild: true})
	e.wantCode(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, kid.id); return err }), cgen.MEM006)
	var approval uuid.UUID
	e.scalar(&approval, `SELECT id FROM guardian_approvals WHERE child_id = $1`, kid.id)
	e.clk.Advance(approvalEraseTtl + time.Hour)
	err := within(t, stuck, func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, approval) })
	})
	e.wantCode(err, cgen.AUTH007)
	if len(e.notices.of(noticeEraseApprovalExpired)) != 0 {
		t.Fatal("承認の経路が失効の知らせを出した")
	}
	if n, err := e.id.Guardians.ExpireDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("ExpireDue が 1 つを失効させない: %d %v", n, err)
	}
	got := e.notices.to(noticeEraseApprovalExpired)
	if len(got) != 2 || got[0] != "self:"+kid.id.String() || got[1] != "guardians:"+kid.id.String() {
		t.Fatalf("失効の知らせが子と保護者へ 1 回ずつでない: %v", got)
	}
	if calls := e.notices.calls(noticeEraseApprovalExpired); len(calls) != 1 {
		t.Fatalf("失効の知らせが 1 回の発行でない: %v", calls)
	}
	if n, _ := e.id.Guardians.ExpireDue(e.ctx); n != 0 || len(e.notices.of(noticeEraseApprovalExpired)) != 2 {
		t.Fatal("ExpireDue の 2 回目が失効させた、または知らせを出した")
	}
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, approval) }), cgen.AUTH007)
}

// R: R-1-6-2
// 行の錠の順（構成員、端末、セッションの順。全体実装設計書 2.4）：試験のトランザクションが端末の行の錠を持つ間に操作を走らせると、操作は端末の行で
// 待ち、まだセッションの行の錠を取っていない（試験のトランザクションが NOWAIT で取れる）。停止、要求の無操作の期限の延長、同じ端末の
// ログイン、抹消の 4 つ。逆の順の操作と端末の取り消しが重なるとデッドロックになる。
func TestRowLockOrderDeviceBeforeSession(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	ops := []struct {
		name string
		run  func(m *member) error
	}{
		{"停止", func(m *member) error {
			return e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, m.id, admin.id, "") })
		}},
		{"無操作の期限の延長", func(m *member) error {
			if _, ev := e.id.Sessions.Verify(e.ctx, m.cookie); ev != nil {
				return ev
			}
			return nil
		}},
		{"同じ端末のログイン", func(m *member) error { _, err := e.login(m, true, nil); return err }},
		{"抹消", func(m *member) error {
			return e.tx(func(tx *store.Tx) error { return e.id.Users.Erase(e.ctx, tx, m.id) })
		}},
	}
	for _, op := range ops {
		m := e.join(admin, "太郎", nil)
		sid := *e.principal(m).SessionId
		if op.name == "抹消" {
			e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, m.id); return err }))
			e.clk.Advance(eraseGrace + time.Minute)
		}
		e.clk.Advance(2 * time.Minute) // 延長と Touch が書く間隔を越える
		hold, err := e.pool.Begin(e.ctx)
		e.must(err)
		_, err = hold.Exec(e.ctx, `SELECT 1 FROM devices WHERE id = $1 FOR UPDATE`, m.device)
		e.must(err)
		done := make(chan error, 1)
		go func() { done <- op.run(m) }()
		time.Sleep(300 * time.Millisecond)
		select {
		case err := <-done:
			hold.Rollback(e.ctx)
			t.Fatalf("%s が端末の行の錠を待たずに終わった（試験の前提）: %v", op.name, err)
		default:
		}
		if _, err := hold.Exec(e.ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE NOWAIT`, sid); err != nil {
			hold.Rollback(e.ctx)
			t.Fatalf("%s が端末より先にセッションの行の錠を取った: %v", op.name, err)
		}
		hold.Rollback(e.ctx)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s が失敗した: %v", op.name, err)
			}
		case <-time.After(stuck):
			t.Fatalf("%s が終わらない", op.name)
		}
	}
}

// R: R-3-3-1
// 同じ challenge の同時の 2 つの検証（第3章 4.2）：消費の行の錠で直列になり、1 つだけがセッションを出し、もう 1 つは AUTH-003。
func TestLoginChallengeConsumedOnceUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	ch, err := e.id.Passkeys.LoginOptions(e.ctx)
	e.must(err)
	resp := e.auth.get(t, ch.Options, admin.cred, nil)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			results <- e.tx(func(tx *store.Tx) error {
				_, err := e.id.Passkeys.LoginVerify(e.ctx, tx, uuid.FromStringOrNil(ch.ChallengeId), resp, nil, NewDevice{Name: "x", Platform: PlatformPc})
				return err
			})
		}()
	}
	close(start)
	ok, rejected := 0, 0
	for i := 0; i < 2; i++ {
		err := within(t, stuck, func() error { return <-results })
		switch codeOf(err) {
		case "":
			e.must(err)
			ok++
		case cgen.AUTH003:
			rejected++
		default:
			t.Fatalf("予期しない誤り: %v", err)
		}
	}
	if ok != 1 || rejected != 1 {
		t.Fatalf("1 つだけが通る形でない: 成功 %d 拒否 %d", ok, rejected)
	}
}

// R: R-3-3-3
// 承認の後の同時の 2 つの poll（S-04b）：端末のコードの行の錠で 1 つだけが端末とセッションを作り、もう 1 つは expired。
func TestQrPollAfterApprovalIssuesOnce(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	e.approve(admin.device, admin.id)
	jwk := gen.CommonJwkP2561{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(bytes.Repeat([]byte{1}, 32)), Y: b64.EncodeToString(bytes.Repeat([]byte{2}, 32))}
	var qr gen.StartQrLoginResponse
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		qr, err = e.id.Qr.Start(e.ctx, tx, QrStart{Name: "PC", Platform: PlatformPc, SigPub: jwk, KemPub: jwk})
		return err
	}))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, e.principal(admin), qr.UserCode) }))
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			results <- e.tx(func(tx *store.Tx) error { _, err := e.id.Qr.Poll(e.ctx, tx, qr.DeviceCode); return err })
		}()
	}
	close(start)
	ok, expired := 0, 0
	for i := 0; i < 2; i++ {
		err := within(t, stuck, func() error { return <-results })
		if err == nil {
			ok++
			continue
		}
		if ev := e.wantCode(err, cgen.AUTH005); ev.Params["state"] == qrExpired {
			expired++
		}
	}
	if ok != 1 || expired != 1 {
		t.Fatalf("1 つだけがセッションを作る形でない: 成功 %d expired %d", ok, expired)
	}
	if n := e.count(`SELECT count(*) FROM devices WHERE user_id = $1`, admin.id); n != 2 {
		t.Fatalf("端末が 1 台だけ増える形でない: %d", n)
	}
}

// R: R-12-4-1
// カードの受け取りの同時（第12章 5.4 の 1〜4。家族の 1 行の錠）：持てる構成員が 3 人以上の時、同じ人が 2 枚目と 3 枚目を同時に
// 受け取ると、1 つだけが通り、もう 1 つは MEM-011（持つ札の番号）。
func TestRecoveryCardsSerializedPerFamily(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	e.join(admin, "太郎", nil)
	x := e.join(admin, "次郎", nil)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, no := range []int{2, 3} {
		go func() {
			<-start
			results <- e.tx(func(tx *store.Tx) error { return e.id.Families.MarkCardHeld(e.ctx, tx, x.id, no) })
		}()
	}
	close(start)
	ok, refused := 0, 0
	for i := 0; i < 2; i++ {
		err := within(t, stuck, func() error { return <-results })
		if err == nil {
			ok++
			continue
		}
		e.wantCode(err, cgen.MEM011)
		refused++
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("同じ人が 2 枚を持てた、または両方が拒まれた: 成功 %d 拒否 %d", ok, refused)
	}
}

// R: R-1-6-2
// 取り消しとログインの作り直しが重なり取り消した端末にセッションが残っても、Verify が端末の状態を毎回見るので通らない（全体実装設計書 3.1。承認の正は devices.state）。
func TestSessionOnRevokedDeviceDoesNotVerify(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.Revoke(e.ctx, tx, m.device, admin.id, false) }))
	var cookie string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		cookie, _, err = e.id.Sessions.Issue(e.ctx, tx, m.id, m.device, true)
		return err
	}))
	if _, ev := e.id.Sessions.Verify(e.ctx, cookie); ev == nil || ev.Code != cgen.AUTH002 || ev.Params["reason"] != string(RevokeDeviceRevoked) {
		t.Fatalf("取り消した端末のセッションが通る: %v", ev)
	}
}

// R: R-1-6-2
// 行の錠の順（構成員が先。全体実装設計書 2.4）：保護者の承認と不承認は、子の構成員の行の錠を取ってから承認の求めの行の錠を取る。
// 子ども区分を外す操作（Users.SetChild、誕生日の 18 歳の解除）は構成員の行の後に open の求めを expired にするので、逆の順の承認と
// 重なるとデッドロックになる。試験のトランザクションが子の構成員の行の錠を持つ間に承認（抹消、端末の鍵）と不承認を走らせると、
// 操作は構成員の行で待ち、まだ求めの行の錠を取っていない（試験のトランザクションが NOWAIT で取れる）。
func TestRowLockOrderUserBeforeGuardianApproval(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	ops := []struct {
		name string
		kind ApprovalKind
		run  func(approval uuid.UUID) error
	}{
		{"抹消の承認", ApprovalErase, func(a uuid.UUID) error {
			return e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, a) })
		}},
		{"端末の鍵の承認", ApprovalDevice, func(a uuid.UUID) error {
			return e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, a) })
		}},
		{"端末の鍵の不承認", ApprovalDevice, func(a uuid.UUID) error {
			return e.tx(func(tx *store.Tx) error { return e.id.Guardians.Deny(e.ctx, tx, admin.id, a) })
		}},
	}
	for _, op := range ops {
		kid := e.childOf(admin, "孫")
		target := ApprovalTarget{}
		if op.kind == ApprovalDevice {
			target.DeviceId = &kid.device
		}
		var approval uuid.UUID
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			approval, err = e.id.Guardians.RequestApproval(e.ctx, tx, kid.id, op.kind, target)
			return err
		}))
		hold, err := e.pool.Begin(e.ctx)
		e.must(err)
		_, err = hold.Exec(e.ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, kid.id)
		e.must(err)
		done := make(chan error, 1)
		go func() { done <- op.run(approval) }()
		time.Sleep(300 * time.Millisecond)
		select {
		case err := <-done:
			hold.Rollback(e.ctx)
			t.Fatalf("%s が構成員の行の錠を待たずに終わった（試験の前提）: %v", op.name, err)
		default:
		}
		if _, err := hold.Exec(e.ctx, `SELECT 1 FROM guardian_approvals WHERE id = $1 FOR UPDATE NOWAIT`, approval); err != nil {
			hold.Rollback(e.ctx)
			<-done
			t.Fatalf("%s が構成員より先に承認の求めの行の錠を取った: %v", op.name, err)
		}
		hold.Rollback(e.ctx)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s が失敗した: %v", op.name, err)
			}
		case <-time.After(stuck):
			t.Fatalf("%s が終わらない", op.name)
		}
	}
}

// R: R-1-6-2
// 停止は本人の全端末の行を識別子の順に取る（全体実装設計書 2.4、I-18。T-L1-09）。小さい端末の行を後ろへ動かして走査の順を識別子の
// 逆にし（索引の列 last_seen_at の値を変える更新。値の変わらない更新は HOT で索引の項目が元の位置のまま）、試験のトランザクションが小さい端末の行を共有の錠（Groups.Commit の Devices.LockShared と同じ）で持つ間に停止を走らせる。
// 停止は小さい端末で待ち、大きい端末の行の錠をまだ取っていない（試験が NOWAIT で取れる）。
func TestRowLockOrderSuspendDevicesInIdOrder(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	var second Device
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		second, err = e.id.Devices.Register(e.ctx, tx, m.id, PlatformPc, "太郎のパソコン")
		return err
	}))
	e.approve(second.Id, m.id)
	lo, hi := m.device, second.Id
	if bytes.Compare(lo[:], hi[:]) > 0 {
		lo, hi = hi, lo
	}
	_, err := e.pool.Exec(e.ctx, `UPDATE devices SET last_seen_at = last_seen_at + interval '1 microsecond' WHERE id = $1`, lo)
	e.must(err)
	hold, err := e.pool.Begin(e.ctx)
	e.must(err)
	t.Cleanup(func() { hold.Rollback(context.Background()) })
	_, err = hold.Exec(e.ctx, `SELECT 1 FROM devices WHERE id = $1 FOR SHARE`, lo)
	e.must(err)
	done := make(chan error, 1)
	go func() {
		done <- e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, m.id, admin.id, "") })
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("停止が小さい端末の行の錠を待たずに終わった（試験の前提）: %v", err)
	default:
	}
	probe, err := e.pool.Begin(e.ctx)
	e.must(err)
	_, perr := probe.Exec(e.ctx, `SELECT 1 FROM devices WHERE id = $1 FOR UPDATE NOWAIT`, hi)
	probe.Rollback(e.ctx)
	hold.Rollback(e.ctx)
	if perr != nil {
		t.Fatalf("停止が小さい端末で待つ間に大きい端末の行の錠を持っている: %v", perr)
	}
	select {
	case err := <-done:
		e.must(err)
	case <-time.After(stuck):
		t.Fatal("停止が終わらない")
	}
}
