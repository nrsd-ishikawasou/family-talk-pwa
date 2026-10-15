// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package identity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// R: R-3-1-1
// 設置（S-02a。T-L1-01 の設置の分）：初回の状態、家族と管理者とパスキーと未承認の最初の端末とセッション、家族の色、
// 本人への new_device が 1 回、OnUserCreated に最初の端末、家族の全員への user.changed と device.changed、コミットの後のアイコン。
// 2 回目の消費と、管理者がいる時のリンクの確かめは AUTH-007。管理者がいればリンクを作らない。
func TestSetupCreatesFamilyAdminAndFirstDevice(t *testing.T) {
	e := newEnv(t)
	st, err := e.id.Setup.Status(e.ctx)
	if err != nil || st != gen.FirstRun {
		t.Fatalf("設置の前の状態が first_run でない: %v %v", st, err)
	}
	var token string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		token, err = e.id.Setup.Create(e.ctx, tx)
		return err
	}))
	if _, err := e.id.Setup.Check(e.ctx, token); err != nil {
		t.Fatalf("使えるリンクを拒んだ: %v", err)
	}
	cid, opts := e.regOptions(nil, &RegToken{Kind: ChallengeSetup, Token: token})
	if opts["authenticatorSelection"].(map[string]any)["authenticatorAttachment"] != nil {
		t.Fatalf("登録の options に authenticatorAttachment が残る（hybrid の登録を締め出す）: %v", opts["authenticatorSelection"])
	}
	resp, cred := e.auth.create(t, opts, credOpts{synced: true, ext: map[string]any{"credProps": map[string]any{"rk": true}, "prf": map[string]any{"enabled": true}}})
	var res AcceptResult
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		res, err = e.id.Setup.Consume(e.ctx, tx, token, cid, "山田家", "花子", resp, NewDevice{Name: "花子の電話", Platform: PlatformIphone})
		return err
	}))
	admin := e.adopt("花子", res, cred)
	if res.User.Role != boundary.RoleAdmin || res.Device.State != DeviceUnapproved || !res.Issued.Result.NewDevice || !res.Passkey.Prf {
		t.Fatalf("設置の結果が違う: %+v", res)
	}
	var color string
	e.scalar(&color, `SELECT color FROM families`)
	if !slices.Contains(familyColors[:], color) {
		t.Fatalf("家族の色が表に無い: %s", color)
	}
	if got := e.notices.to(noticeNewDevice); len(got) != 1 || got[0] != "self:"+admin.id.String() {
		t.Fatalf("new_device が本人へ 1 回でない: %v", got)
	}
	if e.hooks.count("created:"+admin.id.String()+":"+admin.device.String()) != 1 {
		t.Fatal("OnUserCreated に最初の端末が渡らない")
	}
	if got := kinds(e.feedOf(admin.id, admin.device)); strings.Join(got, ",") != "user.changed,device.changed" {
		t.Fatalf("設置の feed が違う: %v", got)
	}
	p := e.principal(admin)
	if p.Role != boundary.RoleAdmin || p.DeviceApproved || p.ReauthAt == nil {
		t.Fatalf("設置のセッションの主体が違う: %+v", p)
	}
	for _, f := range []string{"icon-192-any.png", "icon-512-any.png", "icon-512-maskable.png", "icon-512-monochrome.png"} {
		if _, err := os.Stat(filepath.Join(e.dataDir, "www", "family", f)); err != nil {
			t.Fatalf("コミットの後にアイコン %s が無い: %v", f, err)
		}
	}
	if st, _ := e.id.Setup.Status(e.ctx); st != gen.Ready {
		t.Fatalf("設置の後の状態が ready でない: %v", st)
	}
	if _, err := e.id.Setup.Check(e.ctx, token); codeOf(err) != cgen.AUTH007 {
		t.Fatalf("使ったリンクが AUTH-007 でない: %v", err)
	}
	e.wantCode(e.tx(func(tx *store.Tx) error {
		_, err := e.id.Setup.Consume(e.ctx, tx, token, cid, "山田家", "太郎", resp, NewDevice{Name: "x", Platform: PlatformPc})
		return err
	}), cgen.AUTH007)
	if err := e.tx(func(tx *store.Tx) error { _, err := e.id.Setup.Create(e.ctx, tx); return err }); err == nil || codeOf(err) != "" {
		t.Fatalf("管理者がいるのに設置のリンクを作った: %v", err)
	}
	e.id.SetState(fakeState{s: boundary.StateReadOnly})
	if st, _ := e.id.Setup.Status(e.ctx); st != gen.ReadOnly {
		t.Fatalf("読み取り専用で read_only でない: %v", st)
	}
}

// R: R-3-1-2
// 招待（S-03a。T-L1-02）：発行（管理者は監査）、確かめ（家族の名前、招待者、期限。成功も失敗も 250 ミリ秒）、受諾（構成員、未承認の端末、
// 使用済み、OnUserCreated、new_device が本人へ 1 回）。使用済みと取り消し済みは AUTH-007。他人の招待の取り消しは MEM-001、
// 管理者が他人の招待を取り消すと監査。
func TestInviteIssuePeekAcceptAndRevoke(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	hint := "おばあちゃん"
	var inv Invite
	var tok string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		inv, tok, err = e.id.Invites.Issue(e.ctx, tx, admin.id, InviteSpec{Role: boundary.RoleMember, DisplayHint: &hint})
		return err
	}))
	if slices.Index(e.audit.actions(), auditInviteIssued) < 0 {
		t.Fatalf("管理者の招待の発行を監査に残さない: %v", e.audit.actions())
	}
	var raw []byte
	e.scalar(&raw, `SELECT display_hint FROM invites WHERE id = $1`, inv.Id)
	if bytes.Contains(raw, []byte(hint)) {
		t.Fatal("招待の宛名が平文で DB にある")
	}
	start := wall()
	pk, err := e.id.Invites.Peek(e.ctx, tok)
	if err != nil || pk.FamilyName != "山田家" || pk.InviterName != "花子" || pk.Child != nil {
		t.Fatalf("招待の確かめが違う: %+v %v", pk, err)
	}
	if wall().Sub(start) < peekDelay {
		t.Fatal("確かめが固定の遅延を待たない")
	}
	start = wall()
	if _, err := e.id.Invites.Peek(e.ctx, b64.EncodeToString(make([]byte, 32))); codeOf(err) != cgen.AUTH007 || wall().Sub(start) < peekDelay {
		t.Fatalf("無効なトークンが 250 ミリ秒の後の AUTH-007 でない: %v", err)
	}
	m := e.accept(tok, "祖母")
	var used int
	e.scalar(&used, `SELECT uses_left FROM invites WHERE id = $1`, inv.Id)
	if used != 0 {
		t.Fatal("受諾で招待を使用済みにしない")
	}
	if got := e.notices.to(noticeNewDevice); len(got) != 2 || got[1] != "self:"+m.id.String() {
		t.Fatalf("参加の new_device が本人へ 1 回でない: %v", got)
	}
	if e.hooks.count("created:"+m.id.String()+":"+m.device.String()) != 1 {
		t.Fatal("参加で OnUserCreated が呼ばれない")
	}
	if _, err := e.id.Invites.Peek(e.ctx, tok); codeOf(err) != cgen.AUTH007 {
		t.Fatalf("使用済みの招待が AUTH-007 でない: %v", err)
	}
	if _, err := e.tryAccept(tok, "別人"); codeOf(err) != cgen.AUTH007 {
		t.Fatalf("使用済みの招待で 2 人目が参加した: %v", err)
	}
	// 構成員にも招待を許し、構成員の招待を他の構成員が取り消せない・管理者は取り消せる（監査）。
	e.approve(m.device, admin.id)
	e.setConfig(map[string]any{cgen.KeyAuthMembersCanInvite: true})
	other := e.join(admin, "叔父", nil)
	var mine Invite
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		mine, _, err = e.id.Invites.Issue(e.ctx, tx, m.id, InviteSpec{Role: boundary.RoleMember})
		return err
	}))
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Invites.Revoke(e.ctx, tx, mine.Id, other.id) }), cgen.MEM001)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Invites.Revoke(e.ctx, tx, mine.Id, admin.id) }))
	if slices.Index(e.audit.actions(), auditInviteRevoked) < 0 {
		t.Fatal("管理者が他人の招待を取り消したのを監査に残さない")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Invites.Revoke(e.ctx, tx, mine.Id, admin.id) })) // 取り消し済みは何もしない
	e.wantCode(e.tx(func(tx *store.Tx) error {
		_, _, err := e.id.Invites.Issue(e.ctx, tx, m.id, InviteSpec{Role: boundary.RoleAdmin})
		return err
	}), cgen.MEM007)
	list, more, err := e.id.Invites.ListAll(e.ctx, Page{})
	if err != nil || more || len(list) != 3 || list[0].DisplayHint == nil || *list[0].DisplayHint != hint {
		t.Fatalf("招待の一覧が違う（古い順、宛名を開く）: %d %v %v", len(list), more, err)
	}
}

// R: R-3-3-3
// ログイン（S-04a）：端末の証が通れば同じ端末でセッションを作り直す（古い行は expired、session.revoked は書かない、new_device なし）。
// 証が無い・通らなければ新しい未承認の端末（new_device と device.changed）。停止中は MEM-002、知らない資格情報と challenge の再使用と
// 期限切れは AUTH-003。
func TestLoginKnownDeviceRotatesAndUnknownRegisters(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	before := len(e.notices.of(noticeNewDevice))
	oldSession := *e.principal(admin).SessionId
	is, err := e.login(admin, true, nil)
	e.must(err)
	if is.Result.NewDevice || is.Result.Session.DeviceId != admin.device.String() {
		t.Fatalf("証の通る端末を新しい端末にした: %+v", is.Result)
	}
	var reason string
	e.scalar(&reason, `SELECT revoked_reason FROM sessions WHERE id = $1`, oldSession)
	if reason != string(RevokeExpired) {
		t.Fatalf("作り直しの古い行が expired でない: %s", reason)
	}
	for _, r := range e.feedOf(admin.id, uuid.Nil) {
		if r.Ref.Kind == "session.revoked" {
			t.Fatal("作り直しで session.revoked を書いた")
		}
	}
	if len(e.notices.of(noticeNewDevice)) != before {
		t.Fatal("同じ端末のログインで new_device を出した")
	}
	admin.cookie = is.Cookie
	e.principal(admin)
	// 証の署名が他の鍵なら新しい端末。
	stranger := *admin
	stranger.devKey = e.adopt("x", AcceptResult{Device: Device{Id: uuid.Must(uuid.NewV7())}}, nil).devKey
	is2, err := e.login(&stranger, true, nil)
	e.must(err)
	if !is2.Result.NewDevice || is2.Result.Session.DeviceId == admin.device.String() || is2.Result.Session.Approved {
		t.Fatalf("証の通らない端末を同じ端末にした: %+v", is2.Result)
	}
	if got := e.notices.to(noticeNewDevice); len(got) != before+1 || got[len(got)-1] != "self:"+admin.id.String() {
		t.Fatalf("新しい端末の new_device が本人へ 1 回でない: %v", got)
	}
	// 知らない資格情報。
	ghost := &member{cred: &vcred{id: []byte("ghost-credential"), key: admin.cred.key, handle: make([]byte, 32)}}
	_, err = e.login(ghost, false, nil)
	e.wantCode(err, cgen.AUTH003)
	// challenge の再使用と期限切れ。
	ch, err := e.id.Passkeys.LoginOptions(e.ctx)
	e.must(err)
	resp := e.auth.get(t, ch.Options, admin.cred, nil)
	verify := func() error {
		return e.tx(func(tx *store.Tx) error {
			_, err := e.id.Passkeys.LoginVerify(e.ctx, tx, uuid.FromStringOrNil(ch.ChallengeId), resp, nil, NewDevice{Name: "x", Platform: PlatformPc})
			return err
		})
	}
	e.must(verify())
	e.wantCode(verify(), cgen.AUTH003)
	ch, err = e.id.Passkeys.LoginOptions(e.ctx)
	e.must(err)
	resp = e.auth.get(t, ch.Options, admin.cred, nil)
	e.clk.Advance(challengeTtl + time.Second)
	e.wantCode(verify(), cgen.AUTH003)
	// 停止中の構成員は MEM-002（最後の管理者は停止できないので、もう 1 人の管理者を置く）。
	m := e.join(admin, "太郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, m.id, admin.id, "") }))
	_, err = e.login(m, true, nil)
	e.wantCode(err, cgen.MEM002)
}

// R: R-12-3-2
// PRF の出力はサーバーに渡らない（第12章 5.4 (e)、第3章 4.1、4.2）：ログインの応答に prf があれば空でも AUTH-003、
// 登録の応答の prf.results は AUTH-003、登録の prf.enabled は通り passkeys.prf に残る。発見可能でない資格情報は AUTH-003。
func TestPrfOutputsNeverAccepted(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	for _, ext := range []map[string]any{{"prf": map[string]any{}}, {"prf": map[string]any{"results": map[string]any{"first": "AAAA"}}}} {
		_, err := e.login(admin, false, ext)
		e.wantCode(err, cgen.AUTH003)
	}
	_, err := e.login(admin, true, map[string]any{})
	e.must(err)
	// 登録（ログインの後の追加。承認済みの端末と再認証）。
	e.approve(admin.device, admin.id)
	is, err := e.login(admin, true, nil)
	e.must(err)
	admin.cookie = is.Cookie
	p := e.principal(admin)
	reg := func(o credOpts) error {
		cid, opts := e.regOptions(&p, nil)
		resp, _ := e.auth.create(t, opts, o)
		return e.tx(func(tx *store.Tx) error {
			_, err := e.id.Passkeys.RegisterVerify(e.ctx, tx, admin.id, cid, resp, nil)
			return err
		})
	}
	e.wantCode(reg(credOpts{ext: map[string]any{"credProps": map[string]any{"rk": true}, "prf": map[string]any{"enabled": true, "results": map[string]any{"first": "AAAA"}}}}), cgen.AUTH003)
	e.wantCode(reg(credOpts{notDiscoverable: true}), cgen.AUTH003)
	e.must(reg(credOpts{ext: map[string]any{"credProps": map[string]any{"rk": true}, "prf": map[string]any{"enabled": true}}}))
	if n := e.count(`SELECT count(*) FROM passkeys WHERE user_id = $1 AND prf`, admin.id); n != 1 {
		t.Fatalf("prf.enabled のパスキーが prf を持たない: %d", n)
	}
}

// R: R-3-3-1
// パスキーの追加の認可（第2章 5.2 の register の x-authz。srv-boundary はトークンの値のある経路を通すので本ブロックが確かめる）：トークンも主体も無ければ AUTH-001、端末が未承認なら KEY-004
// （options と verify の両方）、2 本目からは再認証が 5 分を過ぎると AUTH-006。verify は主体が無ければ AUTH-003。トークンは 1 つだけ（NET-001）。
func TestRegisterOptionsRequireApprovedDevice(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	auth := e.id.Apis().Auth
	_, err := e.id.Passkeys.RegisterOptions(e.ctx, nil, nil)
	e.wantCode(err, cgen.AUTH001)
	ctx := e.as(admin)
	_, err = auth.CreatePasskeyRegisterOptions(ctx, gen.CreatePasskeyRegisterOptionsRequestObject{Body: &gen.CreatePasskeyRegisterOptionsRequest{}})
	e.wantCode(err, cgen.KEY004)
	_, err = auth.VerifyPasskeyRegister(ctx, gen.VerifyPasskeyRegisterRequestObject{Body: &gen.VerifyPasskeyRegisterRequest{ChallengeId: newKey()}})
	e.wantCode(err, cgen.KEY004)
	_, err = auth.VerifyPasskeyRegister(e.ctx, gen.VerifyPasskeyRegisterRequestObject{Body: &gen.VerifyPasskeyRegisterRequest{ChallengeId: newKey()}})
	e.wantCode(err, cgen.AUTH003)
	two, one := "a", "b"
	_, err = auth.CreatePasskeyRegisterOptions(e.ctx, gen.CreatePasskeyRegisterOptionsRequestObject{Body: &gen.CreatePasskeyRegisterOptionsRequest{InviteToken: &two, SetupToken: &one}})
	e.wantCode(err, cgen.NET001)
	e.approve(admin.device, admin.id)
	e.clk.Advance(reauthWindow + time.Minute)
	ctx = e.as(admin)
	_, err = auth.CreatePasskeyRegisterOptions(ctx, gen.CreatePasskeyRegisterOptionsRequestObject{Body: &gen.CreatePasskeyRegisterOptionsRequest{}})
	e.wantCode(err, cgen.AUTH006)
	is, err := e.login(admin, true, nil)
	e.must(err)
	admin.cookie = is.Cookie
	r, err := auth.CreatePasskeyRegisterOptions(e.as(admin), gen.CreatePasskeyRegisterOptionsRequestObject{Body: &gen.CreatePasskeyRegisterOptionsRequest{}})
	e.must(err)
	opts := r.(gen.CreatePasskeyRegisterOptions200JSONResponse)
	excl, _ := opts.Options["excludeCredentials"].([]any)
	if len(excl) != 1 {
		t.Fatalf("既存のパスキーを excludeCredentials に入れない: %v", opts.Options["excludeCredentials"])
	}
	resp, _ := e.auth.create(t, opts.Options, credOpts{})
	name := "PC"
	out, err := auth.VerifyPasskeyRegister(e.as(admin), gen.VerifyPasskeyRegisterRequestObject{Body: &gen.VerifyPasskeyRegisterRequest{ChallengeId: opts.ChallengeId, Response: resp, Name: &name}})
	e.must(err)
	if pk := out.(gen.VerifyPasskeyRegister200JSONResponse).Passkey; pk.Name == nil || *pk.Name != "PC" {
		t.Fatalf("追加したパスキーの名前が違う: %+v", pk)
	}
}

// R: R-3-3-1
// パスキーの削除：最後の 1 つは AUTH-008、他人や無いパスキーは何もしない（有無を漏らさない）、消すと OnPasskeyDeleted（コピー (e) の削除）。
// 再設定のリンク（S-04c）：状態が通常でない構成員には作れない（MEM-002）、確かめ、作り直し（既存のパスキーは残す）、未承認の新しい端末、
// new_device、OnPasskeyReset。2 回目は AUTH-007。
func TestPasskeyDeleteAndResetLink(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Passkeys.Delete(e.ctx, tx, m.id, m.cred.id) }), cgen.AUTH008)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Passkeys.Delete(e.ctx, tx, m.id, admin.cred.id) }))
	if e.count(`SELECT count(*) FROM passkeys WHERE cred_id = $1`, admin.cred.id) != 1 {
		t.Fatal("他人のパスキーを消した")
	}
	var link string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		link, err = e.id.Reset.Create(e.ctx, tx, admin.id, m.id)
		return err
	}))
	pk, err := e.id.Reset.Peek(e.ctx, link)
	if err != nil || pk.DisplayName != "太郎" {
		t.Fatalf("再設定のリンクの確かめが違う: %+v %v", pk, err)
	}
	cid, opts := e.regOptions(nil, &RegToken{Kind: ChallengeReset, Token: link})
	resp, cred := e.auth.create(t, opts, credOpts{})
	before := len(e.notices.of(noticeNewDevice))
	var is Issued
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		is, err = e.id.Reset.Consume(e.ctx, tx, link, cid, resp, NewDevice{Name: "新しい電話", Platform: PlatformAndroid})
		return err
	}))
	if !is.Result.NewDevice || is.Result.Session.Approved {
		t.Fatalf("再設定の端末が未承認の新しい端末でない: %+v", is.Result)
	}
	if e.count(`SELECT count(*) FROM passkeys WHERE user_id = $1`, m.id) != 2 {
		t.Fatal("再設定で既存のパスキーを消した、または作らない")
	}
	if e.hooks.count("passkey_reset:"+m.id.String()+":"+is.Result.Session.DeviceId) != 1 {
		t.Fatal("再設定で OnPasskeyReset が呼ばれない")
	}
	if got := e.notices.of(noticeNewDevice); len(got) != before+1 || got[before].user != m.id {
		t.Fatalf("再設定の new_device が本人へ 1 回でない: %v", got)
	}
	e.wantCode(e.tx(func(tx *store.Tx) error {
		_, err := e.id.Reset.Consume(e.ctx, tx, link, cid, resp, NewDevice{Name: "x", Platform: PlatformPc})
		return err
	}), cgen.AUTH007)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Passkeys.Delete(e.ctx, tx, m.id, m.cred.id) }))
	if e.hooks.count("passkey_deleted:"+m.id.String()+":"+b64.EncodeToString(m.cred.id)) != 1 {
		t.Fatal("パスキーの削除で OnPasskeyDeleted が呼ばれない")
	}
	_ = cred
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Suspend(e.ctx, tx, m.id, admin.id, "") }))
	e.wantCode(e.tx(func(tx *store.Tx) error { _, err := e.id.Reset.Create(e.ctx, tx, admin.id, m.id); return err }), cgen.MEM002)
}

// R: R-3-3-3
// QR による端末の追加（S-04b）：開始、承認の画面の情報（鍵の公開部 2 つ）、poll の pending と slow_down、再認証の無い承認は AUTH-006、
// 承認の後の最初の poll だけが端末（未承認、new_device）とセッションを作り、2 回目は expired。期限切れは expired、その承認は AUTH-007。
// 本人が断る操作は無い（第3章 4.4）。保護者の不承認（access_denied）は TestChildDeviceNeedsGuardianApproval。
func TestQrFlowApprovePollExpire(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	e.approve(admin.device, admin.id)
	jwk := gen.CommonJwkP2561{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(bytes.Repeat([]byte{1}, 32)), Y: b64.EncodeToString(bytes.Repeat([]byte{2}, 32))}
	start := func() gen.StartQrLoginResponse {
		var r gen.StartQrLoginResponse
		e.must(e.tx(func(tx *store.Tx) error {
			var err error
			r, err = e.id.Qr.Start(e.ctx, tx, QrStart{Name: "居間の PC", Platform: PlatformPc, SigPub: jwk, KemPub: jwk})
			return err
		}))
		return r
	}
	poll := func(code string) (Issued, error) {
		var is Issued
		err := e.tx(func(tx *store.Tx) error {
			var err error
			is, err = e.id.Qr.Poll(e.ctx, tx, code)
			return err
		})
		return is, err
	}
	qr := start()
	if qr.Interval != 5 || qr.ExpiresIn != 600 || !strings.HasSuffix(qr.Url, "/d/"+strings.ReplaceAll(qr.UserCode, "-", "")) {
		t.Fatalf("開始の応答が違う: %+v", qr)
	}
	info, err := e.id.Qr.Info(e.ctx, e.principal(admin), strings.ToLower(qr.UserCode))
	if err != nil || info.DeviceName != "居間の PC" || info.SigPub.X != jwk.X || info.KemPub.Y != jwk.Y {
		t.Fatalf("承認の画面の情報が違う: %+v %v", info, err)
	}
	_, err = poll(qr.DeviceCode)
	if ev := e.wantCode(err, cgen.AUTH005); ev.Params["state"] != qrPending || !ev.Retryable {
		t.Fatalf("承認の前の poll が pending でない: %+v", ev)
	}
	_, err = poll(qr.DeviceCode)
	if ev := e.wantCode(err, cgen.AUTH005); ev.Params["state"] != qrSlowDown {
		t.Fatalf("5 秒の内の poll が slow_down でない: %+v", ev)
	}
	e.clk.Advance(reauthWindow + time.Second)
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, e.principal(admin), qr.UserCode) }), cgen.AUTH006)
	is, err := e.login(admin, true, nil)
	e.must(err)
	admin.cookie = is.Cookie
	before := len(e.notices.of(noticeNewDevice))
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, e.principal(admin), qr.UserCode) }))
	got, err := poll(qr.DeviceCode)
	e.must(err)
	if !got.Result.NewDevice || got.Result.Session.UserId != admin.id.String() || got.Result.Session.Approved {
		t.Fatalf("承認の後の poll の結果が違う: %+v", got.Result)
	}
	if len(e.notices.of(noticeNewDevice)) != before+1 {
		t.Fatal("QR の端末で new_device を出さない")
	}
	_, err = poll(qr.DeviceCode)
	if ev := e.wantCode(err, cgen.AUTH005); ev.Params["state"] != qrExpired || ev.Retryable {
		t.Fatalf("2 回目の poll が expired でない: %+v", ev)
	}
	late := start()
	e.clk.Advance(qrTtl + time.Second)
	_, err = poll(late.DeviceCode)
	if ev := e.wantCode(err, cgen.AUTH005); ev.Params["state"] != qrExpired {
		t.Fatalf("期限切れの poll が expired でない: %+v", ev)
	}
	is, err = e.login(admin, true, nil)
	e.must(err)
	admin.cookie = is.Cookie
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Qr.Approve(e.ctx, tx, e.principal(admin), late.UserCode) }), cgen.AUTH007)
}

// R: R-3-3-2
// セッション（第2章 4.1、第3章 4.3）：ログアウトはその端末にだけ session.revoked（AUTH-002 logout）、無操作の期限を過ぎた要求は
// AUTH-002 expired で session.revoked を書かない、ExpireDue は期限切れを失効させその端末へ session.revoked{expired}。停止中は AUTH-002 suspended。
// 1 分を過ぎた要求は無操作の期限を延ばし、端末の最終の利用を書く。
func TestSessionsLogoutExpiryAndExtension(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	second, err := e.login(m, false, nil)
	e.must(err)
	secondDevice := uuid.FromStringOrNil(second.Result.Session.DeviceId)
	var idle0 time.Time
	e.scalar(&idle0, `SELECT idle_expires_at FROM sessions WHERE id = $1`, *e.principal(m).SessionId)
	e.clk.Advance(2 * time.Minute)
	p := e.principal(m)
	var idle1, seen time.Time
	e.scalar(&idle1, `SELECT idle_expires_at FROM sessions WHERE id = $1`, *p.SessionId)
	e.scalar(&seen, `SELECT last_seen_at FROM devices WHERE id = $1`, m.device)
	if !idle1.After(idle0) || !seen.Equal(e.clk.Now()) {
		t.Fatalf("1 分を過ぎた要求が期限を延ばさない: %v %v %v", idle0, idle1, seen)
	}
	out, err := e.id.Apis().Auth.Logout(e.as(m), gen.LogoutRequestObject{})
	e.must(err)
	if c := out.(gen.Logout200JSONResponse).Headers.SetCookie; c == nil || !strings.Contains(*c, "Max-Age=0") {
		t.Fatal("ログアウトの応答が Cookie を消さない")
	}
	if _, ev := e.id.Sessions.Verify(e.ctx, m.cookie); ev == nil || ev.Code != cgen.AUTH002 || ev.Params["reason"] != "logout" {
		t.Fatalf("ログアウトの後が AUTH-002 logout でない: %v", ev)
	}
	if got := kinds(e.feedOf(m.id, m.device)); slices.Index(got, "session.revoked") < 0 {
		t.Fatalf("ログアウトした端末に session.revoked が無い: %v", got)
	}
	if got := kinds(e.feedOf(m.id, secondDevice)); slices.Index(got, "session.revoked") >= 0 {
		t.Fatalf("他の端末に session.revoked が届く: %v", got)
	}
	// 無操作の期限（30 日）。要求で見つけた期限切れは session.revoked を書かない。
	e.clk.Advance(31 * 24 * time.Hour)
	if _, ev := e.id.Sessions.Verify(e.ctx, second.Cookie); ev == nil || ev.Params["reason"] != "expired" {
		t.Fatalf("無操作の期限を過ぎた要求が AUTH-002 expired でない: %v", ev)
	}
	if got := kinds(e.feedOf(m.id, secondDevice)); slices.Index(got, "session.revoked") >= 0 {
		t.Fatalf("要求で見つけた期限切れで session.revoked を書いた: %v", got)
	}
	n, err := e.id.Sessions.ExpireDue(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("ExpireDue が管理者のセッション 1 つを失効させない: %d %v", n, err)
	}
	rows := e.feedOf(admin.id, admin.device)
	last := rows[len(rows)-1]
	if last.Ref.Kind != "session.revoked" || last.Ref.DeviceId != admin.device || !strings.Contains(string(last.Ref.Payload), `"expired"`) {
		t.Fatalf("ExpireDue がその端末へ session.revoked{expired} を書かない: %+v", last.Ref)
	}
	if n, _ := e.id.Sessions.ExpireDue(e.ctx); n != 0 {
		t.Fatal("ExpireDue の 2 回目が失効させた")
	}
}

// R: R-12-7-1
// 短いトークン（第2章 4.4、第12章 10）：ws は 60 秒の使い切り、shell は使い切りでない。セッションの失効で ws は AUTH-001、shell は AUTH-002。
func TestShortTokensWsOnceAndShellRevocation(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	p := e.principal(admin)
	issue := func(fn func(tx *store.Tx) (gen.TokenGrant, error)) string {
		var g gen.TokenGrant
		e.must(e.tx(func(tx *store.Tx) error { var err error; g, err = fn(tx); return err }))
		return g.Token
	}
	ws := issue(func(tx *store.Tx) (gen.TokenGrant, error) { return e.id.Sessions.IssueWsToken(e.ctx, tx, p) })
	if got, ev := e.id.Sessions.VerifyWsToken(e.ctx, ws); ev != nil || *got.UserId != admin.id {
		t.Fatalf("ws のトークンが通らない: %v", ev)
	}
	if _, ev := e.id.Sessions.VerifyWsToken(e.ctx, ws); ev == nil || ev.Code != cgen.AUTH001 {
		t.Fatalf("使った ws のトークンが AUTH-001 でない: %v", ev)
	}
	late := issue(func(tx *store.Tx) (gen.TokenGrant, error) { return e.id.Sessions.IssueWsToken(e.ctx, tx, p) })
	e.clk.Advance(wsTokenTtl + time.Second)
	if _, ev := e.id.Sessions.VerifyWsToken(e.ctx, late); ev == nil || ev.Code != cgen.AUTH001 {
		t.Fatalf("期限切れの ws のトークンが AUTH-001 でない: %v", ev)
	}
	shell := issue(func(tx *store.Tx) (gen.TokenGrant, error) { return e.id.Sessions.IssueShellToken(e.ctx, tx, p) })
	for i := 0; i < 2; i++ {
		if got, ev := e.id.Sessions.VerifyShellToken(e.ctx, shell); ev != nil || got.Kind != boundary.KindShell {
			t.Fatalf("shell のトークンが繰り返し通らない: %v", ev)
		}
	}
	ws = issue(func(tx *store.Tx) (gen.TokenGrant, error) { return e.id.Sessions.IssueWsToken(e.ctx, tx, p) })
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Sessions.Revoke(e.ctx, tx, *p.SessionId, RevokeLogout) }))
	if _, ev := e.id.Sessions.VerifyWsToken(e.ctx, ws); ev == nil || ev.Code != cgen.AUTH001 {
		t.Fatalf("失効したセッションの ws のトークンが AUTH-001 でない: %v", ev)
	}
	if _, ev := e.id.Sessions.VerifyShellToken(e.ctx, shell); ev == nil || ev.Code != cgen.AUTH002 {
		t.Fatalf("失効したセッションの shell のトークンが AUTH-002 でない: %v", ev)
	}
}

// R: R-1-6-2
// 端末（第3章 5）：承認（2 回目は KEY-002）、名前の変更（他人の端末は KEY-001）、他の端末の取り消しは再認証（AUTH-006）、取り消しで
// その端末のセッションが失効し session.revoked はその端末へ、OnDeviceRevoked、device.changed は家族の全員へ。取り消し済みの承認は KEY-001。
// 一覧は取り消し済みを除き、名前を開く。自分の端末の取り消しは Cookie を消す。最後の IP は封じて置く（平文を DB に置かない）。
func TestDevicesApproveRenameRevokeAndList(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	m := e.join(admin, "太郎", nil)
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Devices.Approve(e.ctx, tx, m.device, admin.id) }), cgen.KEY002)
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Devices.Rename(e.ctx, tx, admin.id, m.device, "盗った") }), cgen.KEY001)
	second, err := e.login(m, false, nil)
	e.must(err)
	secondDevice := uuid.FromStringOrNil(second.Result.Session.DeviceId)
	dapi := e.id.Apis().Devices
	e.clk.Advance(reauthWindow + time.Second)
	_, err = dapi.DeleteMyDevice(e.as(m), gen.DeleteMyDeviceRequestObject{Id: secondDevice.String()})
	e.wantCode(err, cgen.AUTH006)
	is, err := e.login(m, true, nil)
	e.must(err)
	m.cookie = is.Cookie
	_, err = dapi.UpdateMyDevice(e.as(m), gen.UpdateMyDeviceRequestObject{Id: m.device.String(), Body: &gen.UpdateMyDeviceRequest{Name: "台所のタブレット"}})
	e.must(err)
	var sealedName []byte
	e.scalar(&sealedName, `SELECT name FROM devices WHERE id = $1`, m.device)
	if bytes.Contains(sealedName, []byte("台所のタブレット")) {
		t.Fatal("端末の名前が平文で DB にある")
	}
	_, err = dapi.DeleteMyDevice(e.as(m), gen.DeleteMyDeviceRequestObject{Id: secondDevice.String()})
	e.must(err)
	if e.hooks.count("device_revoked:"+secondDevice.String()) != 1 {
		t.Fatal("取り消しで OnDeviceRevoked が呼ばれない")
	}
	rows := e.feedOf(m.id, secondDevice)
	if got := kinds(rows); slices.Index(got, "session.revoked") < 0 {
		t.Fatalf("取り消した端末に session.revoked が無い: %v", got)
	}
	if got := kinds(e.feedOf(m.id, m.device)); slices.Index(got, "session.revoked") >= 0 {
		t.Fatalf("取り消していない端末に session.revoked が届く: %v", got)
	}
	var changed int
	for _, r := range e.feedOf(admin.id, admin.device) {
		if r.Ref.Kind == "device.changed" && strings.Contains(string(r.Ref.Payload), `"revoked"`) {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("家族の他の構成員に device.changed（revoked）が 1 回でない: %d", changed)
	}
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Devices.Approve(e.ctx, tx, secondDevice, admin.id) }), cgen.KEY001)
	list, _, err := e.id.Devices.List(e.ctx, m.id, m.device, Page{})
	if err != nil || len(list) != 1 || list[0].Name != "台所のタブレット" || !list[0].ThisDevice {
		t.Fatalf("端末の一覧が違う: %+v %v", list, err)
	}
	e.must(e.tx(func(tx *store.Tx) error {
		return e.id.Devices.Touch(e.ctx, tx, m.device, mustAddr("203.0.113.7"), e.clk.Now().Add(2*time.Minute))
	}))
	var lastIp, logged []byte
	e.scalar(&lastIp, `SELECT last_ip FROM devices WHERE id = $1`, m.device)
	e.scalar(&logged, `SELECT ip FROM ip_log WHERE device_id = $1 ORDER BY at DESC LIMIT 1`, m.device)
	if bytes.Contains(lastIp, []byte("203.0.113.7")) || bytes.Contains(logged, []byte("203.0.113.7")) {
		t.Fatal("IP が平文で DB にある")
	}
	list, _, _ = e.id.Devices.List(e.ctx, m.id, m.device, Page{})
	if list[0].LastIp == nil || *list[0].LastIp != "203.0.113.7" {
		t.Fatalf("一覧が最後の IP を開かない: %+v", list[0])
	}
	out, err := dapi.DeleteMyDevice(e.as(m), gen.DeleteMyDeviceRequestObject{Id: m.device.String()})
	e.must(err)
	if c := out.(gen.DeleteMyDevice200JSONResponse).Headers.SetCookie; c == nil || !strings.Contains(*c, "Max-Age=0") {
		t.Fatal("自分の端末の取り消しが Cookie を消さない")
	}
	// 未承認のまま 7 日を過ぎた端末は期限切れになり、取り消しと同じ後始末（OnDeviceRevoked）。
	stale, err := e.login(admin, false, nil)
	e.must(err)
	staleDevice := uuid.FromStringOrNil(stale.Result.Session.DeviceId)
	e.clk.Advance(8 * 24 * time.Hour)
	n, err := e.id.Devices.ExpireUnapproved(e.ctx, e.clk.Now().Add(-7*24*time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("未承認の古い端末を期限切れにしない: %d %v", n, err)
	}
	if e.hooks.count("device_revoked:"+staleDevice.String()) != 1 {
		t.Fatal("期限切れで OnDeviceRevoked が呼ばれない")
	}
	if _, ev := e.id.Sessions.Verify(e.ctx, stale.Cookie); ev == nil || ev.Code != cgen.AUTH002 {
		t.Fatalf("期限切れの端末のセッションが通る: %v", ev)
	}
}

// R: R-12-4-1
// 明示の確認（G-09）：今の版でなければ MEM-008（params.version に今の版）、今の版は記録され GET /me/session に出る。
func TestDisclosureAck(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin("花子")
	doc, err := e.id.Families.Disclosure(e.ctx)
	e.must(err)
	if !strings.Contains(doc.Notice.Lead+strings.Join(doc.Notice.Items, ""), "花子") && !strings.Contains(mustJSON(t, doc), "花子") {
		t.Fatalf("明示の文に管理者の名前を埋めない: %s", mustJSON(t, doc))
	}
	ev := e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Users.AckDisclosure(e.ctx, tx, admin.id, "0000000000000000") }), cgen.MEM008)
	if ev.Params["version"] != doc.Version {
		t.Fatalf("MEM-008 の params.version が今の版でない: %v", ev.Params)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.AckDisclosure(e.ctx, tx, admin.id, doc.Version) }))
	s, err := e.id.Sessions.Describe(e.ctx, e.principal(admin))
	if err != nil || s.DisclosureAckVersion == nil || *s.DisclosureAckVersion != doc.Version {
		t.Fatalf("確認した版が /me/session に出ない: %+v %v", s, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
