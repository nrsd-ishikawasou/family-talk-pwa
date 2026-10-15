// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package identity

import (
	"bytes"
	"encoding/json"
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

// deviceChanges は利用者の feed の device.changed のうち、端末 device の change の並び。
func (e *env) deviceChanges(user, device uuid.UUID) []string {
	e.t.Helper()
	var out []string
	for _, r := range e.feedOf(user, uuid.Nil) {
		if r.Ref.Kind != "device.changed" {
			continue
		}
		var p deviceChanged
		e.must(json.Unmarshal(r.Ref.Payload, &p))
		if p.DeviceId == device {
			out = append(out, p.Change)
		}
	}
	return out
}

// guardianCleared は devices.guardian_approved_at の有無。
func (e *env) guardianCleared(device uuid.UUID) bool {
	e.t.Helper()
	ok, err := e.id.Devices.GuardianCleared(e.ctx, e.pool, device)
	e.must(err)
	return ok
}

// R: R-3-4-1
// srv-keys が読む口（srv-identity のページの FamilyIds、AdminIds、OfUser、NameOf、ApprovedSeenBefore、OwnerOf）と、
// 休眠と戻り（MarkDormant、MarkActive。第3章 5.1、第12章 5.2）。FamilyIds は削除・抹消を除き停止を含む。AdminIds は通常の管理者だけ。
// OfUser は全状態を識別子の順。NameOf は本人の DEK で開いた名前。ApprovedSeenBefore は承認済みで最終の利用が古い物だけ（休眠は外れる）。
// MarkActive は休眠だけを承認済みに戻す（未承認と取り消し済みは変えない）。OwnerOf は持ち主と PRF の有無で、知らない資格情報は KEY-009。
func TestReadsForKeysAndDormancy(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	a := e.join(admin, "太郎", nil)
	b := e.join(admin, "次郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.SetRole(e.ctx, tx, a.id, boundary.RoleAdmin, admin.id) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, b.id, admin.id, "") }))

	sorted := func(ids ...uuid.UUID) []uuid.UUID {
		slices.SortFunc(ids, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
		return ids
	}
	fam, err := e.id.Users.FamilyIds(e.ctx, e.pool)
	e.must(err)
	if want := sorted(admin.id, a.id, b.id); !slices.Equal(fam, want) {
		t.Fatalf("FamilyIds = %v、期待 %v（停止を含む）", fam, want)
	}
	admins, err := e.id.Users.AdminIds(e.ctx, e.pool)
	e.must(err)
	if want := sorted(admin.id, a.id); !slices.Equal(admins, want) {
		t.Fatalf("AdminIds = %v、期待 %v", admins, want)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, b.id, admin.id, false) }))
	fam, err = e.id.Users.FamilyIds(e.ctx, e.pool)
	e.must(err)
	if slices.Contains(fam, b.id) {
		t.Fatalf("削除した構成員が FamilyIds に残る: %v", fam)
	}

	list, err := e.id.Devices.OfUser(e.ctx, e.pool, a.id)
	e.must(err)
	if len(list) != 1 || list[0].Id != a.device || list[0].State != DeviceApproved {
		t.Fatalf("OfUser が違う: %+v", list)
	}
	name, err := e.id.Devices.NameOf(e.ctx, a.device)
	e.must(err)
	if name != "太郎の電話" {
		t.Fatalf("NameOf = %q", name)
	}
	e.wantCode(func() error { _, err := e.id.Devices.NameOf(e.ctx, uuid.Must(uuid.NewV7())); return err }(), cgen.KEY001)

	// 休眠の判定：太郎の端末だけを 100 日前に最後に使ったことにする。
	if _, err := e.pool.Exec(e.ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, a.device, e.clk.Now().Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old, err := e.id.Devices.ApprovedSeenBefore(e.ctx, e.clk.Now().Add(-90*24*time.Hour))
	e.must(err)
	if len(old) != 1 || old[0].Id != a.device {
		t.Fatalf("ApprovedSeenBefore が違う: %+v", old)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.MarkDormant(e.ctx, tx, a.device) }))
	old, err = e.id.Devices.ApprovedSeenBefore(e.ctx, e.clk.Now().Add(-90*24*time.Hour))
	e.must(err)
	if len(old) != 0 {
		t.Fatalf("休眠の端末が ApprovedSeenBefore に残る: %+v", old)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.MarkActive(e.ctx, tx, a.device) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.MarkActive(e.ctx, tx, admin.device) }))
	var states []string
	for _, d := range []uuid.UUID{a.device, admin.device} {
		var s string
		e.scalar(&s, `SELECT state FROM devices WHERE id = $1`, d)
		states = append(states, s)
	}
	if !slices.Equal(states, []string{"approved", "unapproved"}) {
		t.Fatalf("MarkActive の後の状態 = %v（休眠は承認済みへ、未承認はそのまま）", states)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.Revoke(e.ctx, tx, admin.device, admin.id, true) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.MarkActive(e.ctx, tx, admin.device) }))
	var s string
	e.scalar(&s, `SELECT state FROM devices WHERE id = $1`, admin.device)
	if s != "revoked" {
		t.Fatalf("取り消し済みの端末が MarkActive で戻った: %s", s)
	}

	o, err := e.id.Passkeys.OwnerOf(e.ctx, a.cred.id)
	e.must(err)
	if o.UserId != a.id || o.Prf {
		t.Fatalf("OwnerOf が違う: %+v", o)
	}
	e.wantCode(func() error { _, err := e.id.Passkeys.OwnerOf(e.ctx, []byte("unknown")); return err }(), cgen.KEY009)
}

// R: R-12-3-2
// 家族回復鍵の公開部（Families.SetRecoveryPub。第12章 5.4 (d)）：置くたびに版が 1 増え、Families.Get が最新の公開部と版を返す。
func TestFamilyRecoveryPubVersion(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin("花子")
	fam, err := e.id.Families.Get(e.ctx)
	e.must(err)
	if fam.RecoveryKeyVersion != 0 || fam.RecoveryPub != nil {
		t.Fatalf("初めの版と公開部: %d %s", fam.RecoveryKeyVersion, fam.RecoveryPub)
	}
	for i, x := range []string{"AQ", "Ag"} {
		pub := json.RawMessage(`{"kty":"EC","crv":"P-256","x":"` + x + `","y":"AA"}`)
		var v int
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			v, err = e.id.Families.SetRecoveryPub(e.ctx, tx, pub)
			return err
		}))
		if v != i+1 {
			t.Fatalf("%d 回目の版 = %d", i+1, v)
		}
		fam, err = e.id.Families.Get(e.ctx)
		e.must(err)
		var got map[string]string
		e.must(json.Unmarshal(fam.RecoveryPub, &got))
		if fam.RecoveryKeyVersion != i+1 || got["x"] != x {
			t.Fatalf("Get の公開部と版: %d %s", fam.RecoveryKeyVersion, fam.RecoveryPub)
		}
	}
}

// R: R-3-6-1
// 子の端末の鍵の承認の保護者の求め（第3章 4.4。Guardians.RequestApproval の対象に端末の識別子）：保護者へ device_approval_requested
// （端末の名前と種類は devices の行を子の DEK で開いた物）。同じ端末の open の求めは 1 つ（知らせも 1 回）。保護者が承認すると
// 「保護者が承認した端末」になり、家族へ device.changed（guardian_approved）。断るとその端末を取り消す（OnDeviceRevoked、
// session.revoked）。抹消の求めの対象は空でなければならず、端末の追加の求めはコードと端末のどちらか 1 つ。
func TestChildDeviceKeyApprovalByGuardian(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	kid := e.childOf(admin, "孫")
	if e.guardianCleared(kid.device) {
		t.Fatal("招待で作った端末が保護者の承認済みになっている")
	}
	req := func(device uuid.UUID) uuid.UUID {
		var id uuid.UUID
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			id, err = e.id.Guardians.RequestApproval(e.ctx, tx, kid.id, ApprovalDevice, ApprovalTarget{DeviceId: &device})
			return err
		}))
		return id
	}
	before := len(e.notices.of(noticeDeviceApprovalRequested))
	first := req(kid.device)
	if again := req(kid.device); again != first {
		t.Fatalf("同じ端末の open の求めが 2 つになった: %s %s", first, again)
	}
	reqs := e.notices.of(noticeDeviceApprovalRequested)
	if len(reqs) != before+1 {
		t.Fatalf("知らせの数 = %d、期待 %d", len(reqs), before+1)
	}
	last := reqs[len(reqs)-1]
	if last.audience != AudienceGuardians || last.params["device_name"] != "孫の電話" || last.params["platform"] != "android" ||
		last.params["approval_id"] != first.String() {
		t.Fatalf("device_approval_requested が違う: %+v", last)
	}
	var target uuid.UUID
	e.scalar(&target, `SELECT target_device_id FROM guardian_approvals WHERE id = $1`, first)
	if target != kid.device {
		t.Fatalf("target_device_id = %s", target)
	}

	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, first) }))
	if !e.guardianCleared(kid.device) {
		t.Fatal("保護者の承認の後も保護者の承認済みでない")
	}
	if got := e.deviceChanges(admin.id, kid.device); !slices.Contains(got, changeGuardianApproved) {
		t.Fatalf("家族へ device.changed(guardian_approved) が出ない: %v", got)
	}
	if got := e.deviceChanges(kid.id, kid.device); !slices.Contains(got, changeGuardianApproved) {
		t.Fatalf("本人へ device.changed(guardian_approved) が出ない: %v", got)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, first) }))
	if n := len(slices.DeleteFunc(e.deviceChanges(admin.id, kid.device), func(c string) bool { return c != changeGuardianApproved })); n != 1 {
		t.Fatalf("2 回目の承認で device.changed が増えた: %d", n)
	}

	// 2 台目（同期したパスキーで入った端末の代わりに、未承認の端末を直に作る）を保護者が断る。
	var second Device
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		second, err = e.id.Devices.Register(e.ctx, tx, kid.id, PlatformPc, "孫のパソコン")
		return err
	}))
	deny := req(second.Id)
	if deny == first {
		t.Fatal("別の端末の求めが前の求めと同じ")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Deny(e.ctx, tx, admin.id, deny) }))
	var st string
	e.scalar(&st, `SELECT state FROM devices WHERE id = $1`, second.Id)
	if st != "revoked" || e.hooks.count("device_revoked:"+second.Id.String()) != 1 || e.guardianCleared(second.Id) {
		t.Fatalf("断った端末が取り消されない: %s %d", st, e.hooks.count("device_revoked:"+second.Id.String()))
	}
	var by uuid.UUID
	e.scalar(&by, `SELECT revoked_by FROM devices WHERE id = $1`, second.Id)
	if by != admin.id {
		t.Fatalf("取り消した人 = %s、期待は保護者", by)
	}

	// 取り消し済みの端末の求めを承認しても、端末は保護者の承認済みにならず知らせも増えない。
	third := req(second.Id)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, third) }))
	if e.guardianCleared(second.Id) || slices.Contains(e.deviceChanges(admin.id, second.Id), changeGuardianApproved) {
		t.Fatal("取り消し済みの端末が保護者の承認済みになった")
	}

	// 対象の形の誤りはプログラムの誤り（Fault）。
	code := bytes.Repeat([]byte{1}, 32)
	for _, bad := range []ApprovalTarget{{}, {CodeSha256: code, DeviceId: &kid.device}} {
		err := e.tx(func(tx *store.Tx) error {
			_, err := e.id.Guardians.RequestApproval(e.ctx, tx, kid.id, ApprovalDevice, bad)
			return err
		})
		if err == nil || strings.Contains(err.Error(), "KEY-") {
			t.Fatalf("対象の形の誤りが通った: %+v %v", bad, err)
		}
	}
}

// R: R-3-6-1
// QR の承認を保護者が通した時（第3章 4.4）：承認の後の最初の poll が登録する端末は「保護者が承認した端末」。保護者を経ない
// QR の承認（子でない構成員）で登録した端末はそうならない。
func TestQrGuardianApprovalMarksDevice(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	kid := e.childOf(admin, "孫")
	jwk := gen.CommonJwkP2561{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(bytes.Repeat([]byte{1}, 32)), Y: b64.EncodeToString(bytes.Repeat([]byte{2}, 32))}
	start := func(name string) gen.StartQrLoginResponse {
		var qr gen.StartQrLoginResponse
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			qr, err = e.id.Qr.Start(e.ctx, tx, QrStart{Name: name, Platform: PlatformAndroid, SigPub: jwk, KemPub: jwk})
			return err
		}))
		return qr
	}
	poll := func(qr gen.StartQrLoginResponse) uuid.UUID {
		var dev uuid.UUID
		e.must(e.tx(func(tx *store.Tx) error {
			is, err := e.id.Qr.Poll(e.ctx, tx, qr.DeviceCode)
			if err != nil {
				return err
			}
			dev = uuid.FromStringOrNil(is.Result.Session.DeviceId)
			return nil
		}))
		return dev
	}

	qr := start("子のタブレット")
	p := e.principal(kid)
	e.wantCode(within(t, stuck, func() error {
		return e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, p, qr.UserCode) })
	}), cgen.MEM006)
	reqs := e.notices.of(noticeDeviceApprovalRequested)
	approval := uuid.FromStringOrNil(reqs[len(reqs)-1].params["approval_id"].(string))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Guardians.Approve(e.ctx, tx, admin.id, approval) }))
	if dev := poll(qr); dev == uuid.Nil || !e.guardianCleared(dev) {
		t.Fatalf("保護者が承認した QR の端末が保護者の承認済みでない: %s", dev)
	}

	qr = start("花子のタブレット")
	pa := boundary.WithPrincipal(e.ctx, e.principal(admin))
	ap := boundary.PrincipalFrom(pa)
	now := e.clk.Now()
	ap.ReauthAt = &now
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, ap, qr.UserCode) }))
	if dev := poll(qr); dev == uuid.Nil || e.guardianCleared(dev) {
		t.Fatalf("保護者を経ない QR の端末が保護者の承認済みになった: %s", dev)
	}
}
