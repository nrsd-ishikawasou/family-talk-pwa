// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package identity

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// lastRevoked は端末宛ての最後の session.revoked の理由（無ければ空）。
func (e *env) lastRevoked(m *member) string {
	e.t.Helper()
	reason := ""
	for _, r := range e.feedOf(m.id, m.device) {
		if r.Ref.Kind == "session.revoked" {
			reason = string(r.Ref.Payload)
		}
	}
	return reason
}

// stateOf は構成員の状態。
func (e *env) stateOf(id uuid.UUID) string {
	e.t.Helper()
	var s string
	e.scalar(&s, `SELECT state FROM users WHERE id = $1`, id)
	return s
}

// R: R-3-5-1
// 停止・再開・削除（第3章 8.1、8.2。T-L1-08）：最後の管理者は MEM-003。停止はセッションを失効（その端末へ session.revoked{suspended}）、
// 端末を未承認に、本人の未使用の招待を取り消し、OnUserSuspended、家族へ user.changed、カードの持ち主なら管理者へ card_holder_changed。
// 本人への知らせは出さない。再開は状態だけを戻す（端末は未承認のまま）。削除は session.revoked{deleted}、OnUserDeleted、2 回目は MEM-002。
// 既に停止中の人の削除はカードの知らせを繰り返さない。役割の解除は最後の管理者なら MEM-003。
func TestSuspendUnsuspendDeleteAndLastAdmin(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, admin.id, admin.id, "") }), cgen.MEM003)
	e.setConfig(map[string]any{cgen.KeyAuthMembersCanInvite: true})
	m := e.join(admin, "太郎", nil)
	e.issueInvite(m, InviteSpec{Role: boundary.RoleMember})
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Families.MarkCardHeld(e.ctx, tx, m.id, 2) }))
	before := len(e.notices.rows)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, m.id, admin.id, "理由") }))
	if e.stateOf(m.id) != "suspended" || !strings.Contains(e.lastRevoked(m), `"suspended"`) {
		t.Fatalf("停止の状態か session.revoked が違う: %s %s", e.stateOf(m.id), e.lastRevoked(m))
	}
	var dev string
	e.scalar(&dev, `SELECT state FROM devices WHERE id = $1`, m.device)
	if dev != "unapproved" || e.count(`SELECT count(*) FROM invites WHERE issuer = $1 AND revoked_at IS NULL`, m.id) != 0 {
		t.Fatalf("停止が端末を未承認にしない、または招待を取り消さない: %s", dev)
	}
	if e.hooks.count("suspended:"+m.id.String()) != 1 {
		t.Fatal("OnUserSuspended が呼ばれない")
	}
	if got := e.notices.of(noticeCardHolderChanged); len(got) != 1 || got[0].audience != AudienceAdmins || !slices.Equal(got[0].params["cards"].([]int), []int{2}) {
		t.Fatalf("カードの持ち主の停止の知らせが違う: %+v", got)
	}
	for _, r := range e.notices.rows[before:] {
		if r.user == m.id {
			t.Fatalf("停止した本人へ知らせを出した: %+v", r)
		}
	}
	if _, ev := e.id.Sessions.Verify(e.ctx, m.cookie); ev == nil || ev.Params["reason"] != "suspended" {
		t.Fatalf("停止の後のセッションが AUTH-002 suspended でない: %v", ev)
	}
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, m.id, admin.id, "") }), cgen.MEM002)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Unsuspend(e.ctx, tx, m.id, admin.id) }))
	e.scalar(&dev, `SELECT state FROM devices WHERE id = $1`, m.device)
	if e.stateOf(m.id) != "normal" || dev != "unapproved" {
		t.Fatalf("再開の後の状態が違う: %s %s", e.stateOf(m.id), dev)
	}
	is, err := e.login(m, true, nil)
	e.must(err)
	m.cookie = is.Cookie
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, m.id, admin.id, true) }))
	if e.stateOf(m.id) != "deleted" || !strings.Contains(e.lastRevoked(m), `"deleted"`) || e.hooks.count("deleted:"+m.id.String()+":true") != 1 {
		t.Fatalf("削除の状態、session.revoked、hook が違う: %s %s", e.stateOf(m.id), e.lastRevoked(m))
	}
	if len(e.notices.of(noticeCardHolderChanged)) != 2 {
		t.Fatal("状態が通常の持ち主の削除でカードの知らせを出さない")
	}
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, m.id, admin.id, false) }), cgen.MEM002)
	_, err = e.id.Users.Detail(e.ctx, e.principal(admin), m.id)
	e.wantCode(err, cgen.MEM001)
	// 既に停止中の持ち主の削除はカードの知らせを繰り返さない。
	x := e.join(admin, "次郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Families.MarkCardHeld(e.ctx, tx, x.id, 3) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, x.id, admin.id, "") }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, x.id, admin.id, false) }))
	if len(e.notices.of(noticeCardHolderChanged)) != 3 {
		t.Fatalf("停止中の持ち主の削除でカードの知らせを繰り返した: %d", len(e.notices.of(noticeCardHolderChanged)))
	}
	// 役割。
	e.wantCode(e.tx(func(tx *store.Tx) error {
		return e.id.Users.SetRole(e.ctx, tx, admin.id, boundary.RoleMember, admin.id)
	}), cgen.MEM003)
	y := e.join(admin, "三郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.SetRole(e.ctx, tx, y.id, boundary.RoleAdmin, admin.id) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.SetRole(e.ctx, tx, admin.id, boundary.RoleMember, y.id) }))
}

// R: R-3-5-1
// 抹消（第3章 8.3、第12章 9。T-L1-09）：求めで 24 時間の猶予（erase_pending、仕事 erasure、本人へ erase_requested）、2 回目は同じ時刻。
// 取り消しで仕事も消え erase_cancelled。期限の前の Erase は何もしない。期限の後はセッションを erased で失効、全端末の取り消し、
// OnUserErased、DEK と user_private の削除、状態 erased。2 回目は何もしない。抹消した人の招待の宛名は開けないが一覧は出る。
func TestEraseRequestCancelAndErase(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	_, err := e.id.Apis().Me.EraseMe(e.as(admin), gen.EraseMeRequestObject{})
	e.wantCode(err, cgen.MEM003)
	m := e.join(admin, "太郎", nil)
	hint := "いとこ"
	e.setConfig(map[string]any{cgen.KeyAuthMembersCanInvite: true})
	e.must(e.tx(func(tx *store.Tx) error {
		_, _, err := e.id.Invites.Issue(e.ctx, tx, m.id, InviteSpec{Role: boundary.RoleMember, DisplayHint: &hint})
		return err
	}))
	request := func() time.Time {
		var at time.Time
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			at, err = e.id.Users.RequestErase(e.ctx, tx, m.id)
			return err
		}))
		return at
	}
	at := request()
	if !at.Equal(e.clk.Now().Add(eraseGrace)) || e.stateOf(m.id) != "erase_pending" {
		t.Fatalf("猶予の終わりか状態が違う: %v %s", at, e.stateOf(m.id))
	}
	if e.count(`SELECT count(*) FROM jobs WHERE kind = 'erasure' AND dedupe_key = $1 AND state = 'queued'`, "erasure:"+m.id.String()) != 1 {
		t.Fatal("抹消の仕事を積まない")
	}
	if got := e.notices.of(noticeEraseRequested); len(got) != 1 || got[0].user != m.id || got[0].params["erase_at"] != at.UTC().Format(time.RFC3339) {
		t.Fatalf("erase_requested が違う: %+v", got)
	}
	e.clk.Advance(time.Hour)
	if again := request(); !again.Equal(at) {
		t.Fatalf("2 回目の求めが猶予を延ばした: %v %v", at, again)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.CancelErase(e.ctx, tx, m.id) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.CancelErase(e.ctx, tx, m.id) }))
	if e.stateOf(m.id) != "normal" || e.count(`SELECT count(*) FROM jobs WHERE kind = 'erasure'`) != 0 || len(e.notices.of(noticeEraseCancelled)) != 1 {
		t.Fatalf("取り消しの後が違う: %s", e.stateOf(m.id))
	}
	request()
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Erase(e.ctx, tx, m.id) }))
	if e.stateOf(m.id) != "erase_pending" {
		t.Fatal("期限の前に抹消した")
	}
	e.clk.Advance(eraseGrace + time.Minute)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Erase(e.ctx, tx, m.id) }))
	if e.stateOf(m.id) != "erased" || !strings.Contains(e.lastRevoked(m), `"erased"`) || e.hooks.count("erased:"+m.id.String()) != 1 {
		t.Fatalf("抹消の状態、session.revoked、hook が違う: %s %s", e.stateOf(m.id), e.lastRevoked(m))
	}
	if e.count(`SELECT count(*) FROM user_dek WHERE user_id = $1`, m.id)+e.count(`SELECT count(*) FROM user_private WHERE user_id = $1`, m.id) != 0 {
		t.Fatal("抹消で DEK か user_private が残る")
	}
	if e.count(`SELECT count(*) FROM devices WHERE user_id = $1 AND state <> 'revoked'`, m.id) != 0 || e.hooks.count("device_revoked:"+m.device.String()) != 1 {
		t.Fatal("抹消で端末を取り消さない")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Erase(e.ctx, tx, m.id) }))
	if e.hooks.count("erased:"+m.id.String()) != 1 {
		t.Fatal("2 回目の Erase が抹消を繰り返した")
	}
	list, _, err := e.id.Invites.ListAll(e.ctx, Page{})
	e.must(err)
	for _, inv := range list {
		if inv.Issuer == m.id && inv.DisplayHint != nil {
			t.Fatal("抹消した人の招待の宛名が開けた")
		}
	}
}

// childOf は保護者 guardians の子ども区分の構成員を作る。
func (e *env) childOf(admin *member, name string, guardians ...uuid.UUID) *member {
	e.t.Helper()
	return e.join(admin, name, &ChildSpec{IsChild: true, Guardians: guardians})
}

// isChild は子ども区分か。
func (e *env) isChild(id uuid.UUID) bool {
	e.t.Helper()
	var c bool
	e.scalar(&c, `SELECT is_child FROM users WHERE id = $1`, id)
	return c
}

// R: R-3-6-1
// 子ども区分の付け外し（第3章 9.1。Users.SetChild）：年のある誕生日で 18 歳以上なら MEM-012。付けると本人と保護者へ child_flag_set。
// 外すと保護者と制限を消し、open の承認の求め（抹消、端末の追加）を知らせなしで expired にし、端末の追加の求めの端末のコードも expired（誰も断っていない。第3章 4.4）。
// 本人と消す前の保護者へ child_flag_cleared。
func TestSetChildAndClearExpiresOpenApprovals(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	adult := e.join(admin, "太郎", nil)
	b20 := e.clk.Now().AddDate(-20, 0, 0).Format(time.DateOnly)
	e.must(e.tx(func(tx *store.Tx) error {
		return e.id.Users.UpdateProfile(e.ctx, tx, adult.id, gen.UpdateMeRequest{Birthday: &b20})
	}))
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Users.SetChild(e.ctx, tx, adult.id, true, admin.id) }), cgen.MEM012)
	x := e.join(admin, "次郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.SetChild(e.ctx, tx, x.id, true, admin.id) }))
	if !e.isChild(x.id) || !slices.Equal(e.notices.to(noticeChildFlagSet), []string{"self:" + x.id.String(), "guardians:" + x.id.String()}) {
		t.Fatalf("区分を付けた知らせが違う: %v", e.notices.to(noticeChildFlagSet))
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Set(e.ctx, tx, x.id, []uuid.UUID{admin.id}, admin.id) }))
	e.wantCode(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, x.id); return err }), cgen.MEM006)
	jwk := gen.CommonJwkP2561{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(bytes.Repeat([]byte{1}, 32)), Y: b64.EncodeToString(bytes.Repeat([]byte{2}, 32))}
	var qr gen.StartQrLoginResponse
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		qr, err = e.id.Qr.Start(e.ctx, tx, QrStart{Name: "子の PC", Platform: PlatformPc, SigPub: jwk, KemPub: jwk})
		return err
	}))
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, e.principal(x), qr.UserCode) }), cgen.MEM006)
	if e.count(`SELECT count(*) FROM guardian_approvals WHERE child_id = $1 AND state = 'open'`, x.id) != 2 {
		t.Fatal("抹消と端末の追加の求めが open で 2 つない")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.SetChild(e.ctx, tx, x.id, false, admin.id) }))
	if e.isChild(x.id) || e.count(`SELECT count(*) FROM guardians WHERE child_id = $1`, x.id) != 0 {
		t.Fatal("区分を外しても保護者が残る")
	}
	if e.count(`SELECT count(*) FROM guardian_approvals WHERE child_id = $1 AND state = 'expired'`, x.id) != 2 || len(e.notices.of(noticeEraseApprovalExpired)) != 0 {
		t.Fatal("区分を外した時の求めが知らせなしの expired でない")
	}
	var st string
	e.scalar(&st, `SELECT state FROM device_codes WHERE user_code = $1`, qr.UserCode)
	if st != "expired" {
		t.Fatalf("端末の追加の求めの端末のコードが expired でない: %s", st)
	}
	if got := e.notices.to(noticeChildFlagCleared); !slices.Equal(got, []string{"self:" + x.id.String(), "self:" + admin.id.String()}) {
		t.Fatalf("区分を外した知らせが本人と消す前の保護者へ出ない: %v", got)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.SetChild(e.ctx, tx, x.id, false, admin.id) })) // 同じ値は何もしない
	if len(e.notices.of(noticeChildFlagCleared)) != 2 {
		t.Fatal("同じ値の SetChild が知らせを出した")
	}
}

// putSettings は保護者 by が子の制限を置く。
func (e *env) putSettings(child, by uuid.UUID, s gen.FtSettingsGuardian1) {
	e.t.Helper()
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.PutSettings(e.ctx, tx, child, by, s) }))
}

// birthdayFor は今日から見て、年齢 age の誕生日を days 日の後に迎える日付（YYYY-MM-DD）。
func (e *env) birthdayFor(age, days int) string {
	return e.clk.Now().AddDate(-age, 0, days).Format(time.DateOnly)
}

// R: R-3-6-1
// 誕生日と 16 歳・18 歳（第3章 9.1）：子は自分の誕生日を書けない（MEM-007）。保護者の書く誕生日は年が要り未来は不可（MEM-005）。
// 16 歳の緩和は、保護者が 16 歳の誕生日より後に変えた設定を保ち、前の設定なら時間帯とリンクの確認を外して本人と保護者へ child_relaxed_16、
// 同じ日の 2 回目は何もしない。年齢の問いの行（updated_by が子）は保護者の変更と読まない。18 歳の当日の ChildTransitions は区分を外し、
// 本人と消す前の保護者へ child_released_18、2 回目は何もしない。区分の無い人の 18 歳未満の誕生日は管理者へ child_flag_suggested。
func TestBirthdaysAndAgeTransitions(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	g := e.join(admin, "母", nil)
	quiet, on := "21:00-07:00", true
	setBirthday := func(child *member, s string) error {
		return e.tx(func(tx *store.Tx) error { return e.id.Guardians.SetBirthday(e.ctx, tx, child.id, s) })
	}
	settings := func(child *member) gen.FtSettingsGuardian1 {
		s, err := e.id.Guardians.Settings(e.ctx, child.id)
		e.must(err)
		return s
	}
	relaxed := func(s gen.FtSettingsGuardian1) bool {
		return s.QuietHours != nil && *s.QuietHours == "" && s.LinkConfirm != nil && !*s.LinkConfirm
	}
	// 子は自分の誕生日を書けない。保護者の書く誕生日の形。
	c1 := e.childOf(admin, "一郎", g.id)
	b := e.birthdayFor(10, 0)
	e.wantCode(e.tx(func(tx *store.Tx) error {
		return e.id.Users.UpdateProfile(e.ctx, tx, c1.id, gen.UpdateMeRequest{Birthday: &b})
	}), cgen.MEM007)
	e.wantCode(setBirthday(c1, e.clk.Now().AddDate(1, 0, 0).Format(time.DateOnly)), cgen.MEM005)
	e.wantCode(setBirthday(c1, "05-01"), cgen.MEM005)
	// 保護者が 16 歳の誕生日より後に変えた設定は保つ。
	e.putSettings(c1.id, g.id, gen.FtSettingsGuardian1{QuietHours: &quiet, LinkConfirm: &on})
	e.must(setBirthday(c1, e.birthdayFor(16, -30)))
	if relaxed(settings(c1)) || len(e.notices.of(noticeChildRelaxed16)) != 0 {
		t.Fatal("16 歳の誕生日の後の保護者の設定を緩めた")
	}
	// 16 歳の誕生日より前の保護者の設定は緩める。同じ日の 2 回目は何もしない。
	c2 := e.childOf(admin, "二郎", g.id)
	e.putSettings(c2.id, g.id, gen.FtSettingsGuardian1{QuietHours: &quiet, LinkConfirm: &on})
	b16 := e.birthdayFor(16, 10) // 16 歳の誕生日は 10 日の後
	e.clk.Advance(20 * 24 * time.Hour)
	e.must(setBirthday(c2, b16))
	if !relaxed(settings(c2)) {
		t.Fatalf("16 歳の誕生日の前の設定を緩めない: %+v", settings(c2))
	}
	if got := e.notices.to(noticeChildRelaxed16); !slices.Equal(got, []string{"self:" + c2.id.String(), "guardians:" + c2.id.String()}) {
		t.Fatalf("child_relaxed_16 が本人と保護者へ出ない: %v", got)
	}
	e.must(setBirthday(c2, b16))
	if len(e.notices.of(noticeChildRelaxed16)) != 2 {
		t.Fatal("同じ日の 2 回目に緩和を繰り返した")
	}
	// 年齢の問いの行は保護者の変更と読まない。
	c3 := e.childOf(admin, "三郎", g.id)
	e.clk.Advance(2*365*24*time.Hour + 24*time.Hour)
	asked, err := e.id.Guardians.AskAgeEveryTwoYears(e.ctx)
	e.must(err)
	if !slices.Contains(asked, c3.id) {
		t.Fatalf("2 年を過ぎた申告だけの子を問わない: %v", asked)
	}
	if again, _ := e.id.Guardians.AskAgeEveryTwoYears(e.ctx); slices.Contains(again, c3.id) {
		t.Fatal("同じ日に 2 回問うた")
	}
	e.must(setBirthday(c3, e.birthdayFor(17, 0)))
	if !relaxed(settings(c3)) {
		t.Fatal("年齢の問いの行で緩和が止まった")
	}
	// 18 歳の当日の解除。
	c4 := e.childOf(admin, "四郎", g.id)
	e.must(setBirthday(c4, e.birthdayFor(18, 1))) // 18 歳の誕生日は明日
	if !e.isChild(c4.id) {
		t.Fatal("17 歳で区分が外れた")
	}
	e.wantCode(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, c4.id); return err }), cgen.MEM006)
	e.clk.Advance(24 * time.Hour)
	tr, err := e.id.Users.ChildTransitions(e.ctx, e.clk.Now())
	e.must(err)
	if !slices.Contains(tr, Transition{UserId: c4.id, Age: childAge}) || e.isChild(c4.id) {
		t.Fatalf("18 歳の当日に区分が外れない: %v", tr)
	}
	if e.count(`SELECT count(*) FROM guardian_approvals WHERE child_id = $1 AND state = 'expired'`, c4.id) != 1 || len(e.notices.of(noticeEraseApprovalExpired)) != 0 {
		t.Fatal("18 歳の解除で open の求めが知らせなしの expired にならない")
	}
	if got := e.notices.to(noticeChildReleased18); !slices.Equal(got, []string{"self:" + c4.id.String(), "self:" + g.id.String()}) {
		t.Fatalf("child_released_18 が本人と消す前の保護者へ出ない: %v", got)
	}
	if tr, _ := e.id.Users.ChildTransitions(e.ctx, e.clk.Now()); len(tr) != 0 {
		t.Fatalf("同じ日の 2 回目に解除を繰り返した: %v", tr)
	}
	// 区分の無い人の誕生日。
	b17 := e.birthdayFor(17, 0)
	e.must(e.tx(func(tx *store.Tx) error {
		return e.id.Users.UpdateProfile(e.ctx, tx, g.id, gen.UpdateMeRequest{Birthday: &b17})
	}))
	var sealedBirthday []byte
	e.scalar(&sealedBirthday, `SELECT birthday FROM user_private WHERE user_id = $1`, g.id)
	if bytes.Contains(sealedBirthday, []byte(b17)) {
		t.Fatal("誕生日が平文で DB にある")
	}
	if got := e.notices.to(noticeChildFlagSuggested); !slices.Equal(got, []string{"admins"}) || e.isChild(g.id) {
		t.Fatalf("18 歳未満の誕生日の勧めが管理者へ出ない、または区分を付けた: %v", got)
	}
	future := e.clk.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	e.wantCode(e.tx(func(tx *store.Tx) error {
		return e.id.Users.UpdateProfile(e.ctx, tx, g.id, gen.UpdateMeRequest{Birthday: &future})
	}), cgen.MEM005)
	me, err := e.id.Users.Me(e.ctx, g.id)
	if err != nil || me.Birthday == nil || *me.Birthday != b17 {
		t.Fatalf("誕生日を開いて返さない: %+v %v", me.Birthday, err)
	}
}

// R: R-3-6-1
// 保護者（第3章 9.2）：子でない人の保護者の組は MEM-001、状態が通常でない保護者と子自身も MEM-001。置き直しは子と新旧の保護者へ
// guardians_changed を 1 回ずつ。設定の変更は子と書いた人以外の保護者へ guardian_settings_changed。保護者でない人の読みと承認は MEM-001。
// 子の招待は保護者の許可（allow_invites）が要る（MEM-007）。
func TestGuardiansSetSettingsAndRules(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	g1 := e.join(admin, "母", nil)
	g2 := e.join(admin, "父", nil)
	stranger := e.join(admin, "叔父", nil)
	kid := e.childOf(admin, "孫")
	set := func(child uuid.UUID, gs ...uuid.UUID) error {
		return e.tx(func(tx *store.Tx) error { return e.id.Guardians.Set(e.ctx, tx, child, gs, admin.id) })
	}
	e.wantCode(set(g1.id, g2.id), cgen.MEM001)
	e.wantCode(set(kid.id, kid.id), cgen.MEM001)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, stranger.id, admin.id, "") }))
	e.wantCode(set(kid.id, stranger.id), cgen.MEM001)
	e.must(set(kid.id, g1.id, g2.id, g1.id))
	got := e.notices.to(noticeGuardiansChanged)
	want := []string{"self:" + kid.id.String(), "self:" + admin.id.String(), "self:" + g1.id.String(), "self:" + g2.id.String()}
	if !slices.Equal(got, want) {
		t.Fatalf("guardians_changed が子と新旧の保護者へ 1 回ずつでない: %v", got)
	}
	if names := e.notices.of(noticeGuardiansChanged)[0].params["guardian_names"].([]string); !slices.Equal(names, []string{"母", "父"}) {
		t.Fatalf("guardian_names が新しい保護者の名前でない: %v", names)
	}
	allow := true
	e.putSettings(kid.id, g1.id, gen.FtSettingsGuardian1{AllowInvites: &allow})
	if got := e.notices.to(noticeGuardianSettings); !slices.Equal(got, []string{"self:" + kid.id.String(), "self:" + g2.id.String()}) {
		t.Fatalf("guardian_settings_changed が子と他の保護者へ出ない: %v", got)
	}
	me := e.id.Apis().Me
	_, err := me.GetChildControls(e.as(admin), gen.GetChildControlsRequestObject{Id: kid.id.String()})
	e.wantCode(err, cgen.MEM001)
	if r, err := me.GetChildControls(e.as(g2), gen.GetChildControlsRequestObject{Id: kid.id.String()}); err != nil ||
		*r.(gen.GetChildControls200JSONResponse).AllowInvites != true {
		t.Fatalf("保護者が子の設定を読めない: %v", err)
	}
	e.setConfig(map[string]any{cgen.KeyAuthMembersCanInvite: true})
	e.must(e.tx(func(tx *store.Tx) error {
		_, _, err := e.id.Invites.Issue(e.ctx, tx, kid.id, InviteSpec{Role: boundary.RoleMember})
		return err
	}))
	no := false
	e.putSettings(kid.id, g2.id, gen.FtSettingsGuardian1{AllowInvites: &no})
	e.wantCode(e.tx(func(tx *store.Tx) error {
		_, _, err := e.id.Invites.Issue(e.ctx, tx, kid.id, InviteSpec{Role: boundary.RoleMember})
		return err
	}), cgen.MEM007)
	e.wantCode(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, kid.id); return err }), cgen.MEM006)
	var approval uuid.UUID
	e.scalar(&approval, `SELECT id FROM guardian_approvals WHERE child_id = $1`, kid.id)
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, approval) }), cgen.MEM001)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, g2.id, approval) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, g1.id, approval) })) // 承認済みは何もしない
	if e.stateOf(kid.id) != "erase_pending" || len(e.notices.of(noticeEraseRequested)) != 1 {
		t.Fatalf("保護者の承認で猶予が始まらない: %s", e.stateOf(kid.id))
	}
}

// R: R-3-6-1
// 子の端末の追加の保護者の承認（第3章 9.2、S-04b）：子の承認は MEM-006 で保護者へ device_approval_requested（端末の名前と種類）、
// 保護者の承認で子の端末として poll が通る。保護者が断れば poll は access_denied（RFC 8628 3.5。第3章 4.4。再試行不可）。
func TestChildDeviceNeedsGuardianApproval(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	kid := e.childOf(admin, "孫")
	jwk := gen.CommonJwkP2561{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(bytes.Repeat([]byte{1}, 32)), Y: b64.EncodeToString(bytes.Repeat([]byte{2}, 32))}
	flow := func(approve bool) error {
		var qr gen.StartQrLoginResponse
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			qr, err = e.id.Qr.Start(e.ctx, tx, QrStart{Name: "子のタブレット", Platform: PlatformAndroid, SigPub: jwk, KemPub: jwk})
			return err
		}))
		p := e.principal(kid)
		e.wantCode(within(t, stuck, func() error {
			return e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, p, qr.UserCode) })
		}), cgen.MEM006)
		reqs := e.notices.of(noticeDeviceApprovalRequested)
		last := reqs[len(reqs)-1]
		if last.audience != AudienceGuardians || last.params["device_name"] != "子のタブレット" || last.params["platform"] != "android" {
			t.Fatalf("device_approval_requested が違う: %+v", last)
		}
		approval := uuid.FromStringOrNil(last.params["approval_id"].(string))
		e.must(e.tx(func(tx *store.Tx) error {
			if approve {
				return e.id.Guardians.Approve(e.ctx, tx, admin.id, approval)
			}
			return e.id.Guardians.Deny(e.ctx, tx, admin.id, approval)
		}))
		return e.tx(func(tx *store.Tx) error {
			is, err := e.id.Qr.Poll(e.ctx, tx, qr.DeviceCode)
			if err == nil && is.Result.Session.UserId != kid.id.String() {
				t.Fatalf("保護者の承認の後の端末が子の物でない: %+v", is.Result)
			}
			return err
		})
	}
	e.must(flow(true))
	if ev := e.wantCode(flow(false), cgen.AUTH005); ev.Params["state"] != qrAccessDenied || ev.Retryable {
		t.Fatalf("保護者が断った後の poll が access_denied（再試行不可）でない: %+v", ev)
	}
	if got := e.notices.of(noticeNewDevice); got[len(got)-1].audience != AudienceGuardians {
		t.Fatalf("子の新しい端末の new_device が保護者へ出ない: %+v", got[len(got)-1])
	}
}

// R: R-3-6-1
// 招待の受諾の時の保護者（第3章 3.3、9.1）：発行の後に停止された保護者は置かない。誰も残らなければ保護者のいない子ども区分。
func TestInviteAcceptKeepsOnlyNormalGuardians(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	g := e.join(admin, "母", nil)
	both := e.issueInvite(admin, InviteSpec{Role: boundary.RoleMember, Child: &ChildSpec{IsChild: true, Guardians: []uuid.UUID{g.id, admin.id}}})
	only := e.issueInvite(admin, InviteSpec{Role: boundary.RoleMember, Child: &ChildSpec{IsChild: true, Guardians: []uuid.UUID{g.id}}})
	pk, err := e.id.Invites.Peek(e.ctx, both)
	if err != nil || pk.Child == nil || !slices.Equal(pk.Child.GuardianNames, []string{"母", "花子"}) {
		t.Fatalf("子の招待の確かめに保護者の名前が無い: %+v %v", pk.Child, err)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, g.id, admin.id, "") }))
	c1 := e.accept(both, "一郎")
	c2 := e.accept(only, "二郎")
	gs, err := e.id.Guardians.Of(e.ctx, c1.id)
	if err != nil || !slices.Equal(gs, []uuid.UUID{admin.id}) {
		t.Fatalf("停止された保護者を置いた: %v %v", gs, err)
	}
	if gs, _ := e.id.Guardians.Of(e.ctx, c2.id); len(gs) != 0 || !e.isChild(c2.id) {
		t.Fatalf("保護者のいない子ども区分にならない: %v", gs)
	}
	e.wantCode(e.tx(func(tx *store.Tx) error {
		_, _, err := e.id.Invites.Issue(e.ctx, tx, admin.id, InviteSpec{Role: boundary.RoleMember, Child: &ChildSpec{IsChild: true, Guardians: []uuid.UUID{g.id}}})
		return err
	}), cgen.NET001)
}

// R: R-12-4-1
// カードの受け取りの記録（第12章 5.4 の 1〜4）：子ども区分は MEM-010、状態が通常でなければ MEM-002、別の人の札は MEM-009（持ち主の名前）、
// 同じ札は冪等、別の札を持つ人は持てる構成員が 3 人以上なら MEM-011（持つ札の番号）、3 人未満なら許す。
func TestRecoveryCardHolderRules(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	mark := func(u uuid.UUID, no int) error {
		return e.tx(func(tx *store.Tx) error { return e.id.Families.MarkCardHeld(e.ctx, tx, u, no) })
	}
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Families.MarkCardPrinted(e.ctx, tx, admin.id); return err }))
	e.must(mark(admin.id, 2)) // 2 人の家族：カード 1 の持ち主も 2 枚目を持てる
	e.must(mark(admin.id, 2))
	ev := e.wantCode(mark(m.id, 2), cgen.MEM009)
	if ev.Params["holder"] != "花子" {
		t.Fatalf("MEM-009 の params.holder が持ち主の名前でない: %v", ev.Params)
	}
	kid := e.childOf(admin, "孫")
	e.wantCode(mark(kid.id, 3), cgen.MEM010)
	x := e.join(admin, "次郎", nil) // 持てる構成員が 3 人に
	ev = e.wantCode(mark(admin.id, 3), cgen.MEM011)
	if ev.Params["held"] != 1 {
		t.Fatalf("MEM-011 の params.held が持つ札の番号でない: %v", ev.Params)
	}
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, x.id); return err }))
	e.wantCode(mark(x.id, 3), cgen.MEM002)
	e.must(mark(m.id, 3))
	cards, err := e.id.Families.Cards(e.ctx)
	if err != nil || *cards[0].HolderId != admin.id || *cards[1].HolderId != admin.id || *cards[2].HolderId != m.id {
		t.Fatalf("カードの持ち主の一覧が違う: %+v %v", cards, err)
	}
	doc, err := e.id.Families.Disclosure(e.ctx)
	e.must(err)
	if s := mustJSON(t, doc); !strings.Contains(s, "1 枚目は花子") || !strings.Contains(s, "3 枚目は太郎") {
		t.Fatalf("明示の文にカードの持ち主を埋めない: %s", s)
	}
}

// R: R-3-2-2
// プロフィールと設定（第3章 6）：別名は 20 書記素で封じて置き（平文を DB に置かない）、相手がいなければ MEM-001、消せる。
// 設定は既定に重ね、与えた項目だけを置き換える（muted_users は別の列）。一覧の子ども区分は本人、保護者、管理者にだけ見せる。
// 端末からの報告は管理者へ update_failed。
func TestProfileAliasesSettingsListAndReports(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	kid := e.childOf(admin, "孫", m.id)
	other := e.join(admin, "次郎", nil)
	alias := func(target uuid.UUID, name *string) error {
		return e.tx(func(tx *store.Tx) error { return e.id.Users.SetAlias(e.ctx, tx, m.id, target, name) })
	}
	nick := "ばあば"
	e.wantCode(alias(uuid.Must(uuid.NewV7()), &nick), cgen.MEM001)
	long := strings.Repeat("あ", nameMax+1)
	e.wantCode(alias(admin.id, &long), cgen.MEM005)
	e.must(alias(admin.id, &nick))
	var raw []byte
	e.scalar(&raw, `SELECT aliases FROM user_private WHERE user_id = $1`, m.id)
	if bytes.Contains(raw, []byte(nick)) {
		t.Fatal("別名が平文で DB にある")
	}
	e.must(alias(admin.id, nil))
	muted := []any{admin.id.String()}
	e.must(e.tx(func(tx *store.Tx) error {
		return e.id.Users.PutSettings(e.ctx, tx, m.id, gen.FtSettingsUser1{MutedUsers: &muted})
	}))
	s, err := e.id.Users.Settings(e.ctx, m.id)
	if err != nil || s.MutedUsers == nil || len(*s.MutedUsers) != 1 || (*s.MutedUsers)[0] != admin.id.String() {
		t.Fatalf("設定の muted_users が戻らない: %+v %v", s.MutedUsers, err)
	}
	listed := func(viewer *member) map[uuid.UUID]*bool {
		items, _, err := e.id.Users.List(e.ctx, e.principal(viewer), Page{})
		e.must(err)
		out := map[uuid.UUID]*bool{}
		for _, u := range items {
			out[uuid.FromStringOrNil(u.Id)] = u.Child
		}
		return out
	}
	if c := listed(admin)[kid.id]; c == nil || !*c {
		t.Fatal("管理者に子ども区分を見せない")
	}
	if c := listed(m)[kid.id]; c == nil || !*c {
		t.Fatal("保護者に子ども区分を見せない")
	}
	if c := listed(other)[kid.id]; c != nil {
		t.Fatal("保護者でも管理者でもない人に子ども区分を見せた")
	}
	code := "E42"
	e.must(e.tx(func(tx *store.Tx) error {
		return e.id.Reports.Create(e.ctx, tx, e.principal(m), gen.CreateReportRequest{Kind: gen.UpdateFailed, Code: &code})
	}))
	if got := e.notices.of(noticeUpdateFailed); len(got) != 1 || got[0].audience != AudienceAdmins || got[0].params["name"] != "太郎" || got[0].params["code"] != "E42" {
		t.Fatalf("update_failed が違う: %+v", got)
	}
}

// R: R-1-3-2
// hook の誤りはトランザクションを戻す（I-8）：OnUserSuspended の登録が誤りを返すと停止は全部戻り、状態とセッションは元のまま。
func TestHookErrorRollsBackOperation(t *testing.T) {
	fail := errors.New("上のブロックの誤り")
	var armed bool
	e := newEnv(t, func(h *Hooks) {
		h.RegisterOnUserSuspended(func(context.Context, *store.Tx, uuid.UUID) error {
			if armed {
				return fail
			}
			return nil
		})
	})
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	armed = true
	if err := e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, m.id, admin.id, "") }); !errors.Is(err, fail) {
		t.Fatalf("hook の誤りが返らない: %v", err)
	}
	if e.stateOf(m.id) != "normal" || e.lastRevoked(m) != "" || e.hooks.count("suspended:") != 0 {
		t.Fatal("hook の誤りで停止が戻らない")
	}
	e.principal(m)
}

// R: R-3-3-4
// 再認証（第3章 4.5。x-reauth の 8 経路は本ブロックが判定する）：最後の UV のあるパスキーの操作から 5 分を過ぎると、抹消の求め、
// 子の設定、子の誕生日、QR の承認、パスキーの削除、他の端末の取り消し、2 本目からのパスキーの追加（options と verify）が AUTH-006。
func TestReauthRequiredRoutes(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	e.approve(admin.device, admin.id)
	kid := e.childOf(admin, "孫")
	e.clk.Advance(reauthWindow + time.Minute)
	ctx := e.as(admin)
	if p := boundaryPrincipal(ctx); e.id.d.reauthed(p) {
		t.Fatal("試験の前提：再認証が 5 分の内")
	}
	me, auth, dev := e.id.Apis().Me, e.id.Apis().Auth, e.id.Apis().Devices
	b := e.birthdayFor(10, 0)
	calls := map[string]func() error{
		"抹消の求め": func() error { _, err := me.EraseMe(ctx, gen.EraseMeRequestObject{}); return err },
		"子の設定": func() error {
			_, err := me.PutChildControls(ctx, gen.PutChildControlsRequestObject{Id: kid.id.String(), Body: &gen.FtSettingsGuardian1{}})
			return err
		},
		"子の誕生日": func() error {
			_, err := me.PutChildBirthday(ctx, gen.PutChildBirthdayRequestObject{Id: kid.id.String(), Body: &gen.PutChildBirthdayRequest{Birthday: b}})
			return err
		},
		"QR の承認": func() error {
			_, err := auth.ApproveQrLogin(ctx, gen.ApproveQrLoginRequestObject{Body: &gen.ApproveQrLoginRequest{UserCode: "ABCD-EFGH"}})
			return err
		},
		"パスキーの削除": func() error {
			_, err := auth.DeleteMyPasskey(ctx, gen.DeleteMyPasskeyRequestObject{Id: b64.EncodeToString(admin.cred.id)})
			return err
		},
		"他の端末の取り消し": func() error {
			_, err := dev.DeleteMyDevice(ctx, gen.DeleteMyDeviceRequestObject{Id: kid.device.String()})
			return err
		},
		"パスキーの追加の options": func() error {
			_, err := auth.CreatePasskeyRegisterOptions(ctx, gen.CreatePasskeyRegisterOptionsRequestObject{Body: &gen.CreatePasskeyRegisterOptionsRequest{}})
			return err
		},
		"パスキーの追加の verify": func() error {
			_, err := auth.VerifyPasskeyRegister(ctx, gen.VerifyPasskeyRegisterRequestObject{Body: &gen.VerifyPasskeyRegisterRequest{ChallengeId: newKey()}})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); codeOf(err) != cgen.AUTH006 {
			t.Fatalf("%s が AUTH-006 でない: %v", name, err)
		}
	}
}
