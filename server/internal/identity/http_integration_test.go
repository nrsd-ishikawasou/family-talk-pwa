// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package identity

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 本ブロックの操作を本物の srv-boundary に通す試験。応答は api の spec で検証される（ValidateResponses）ので、形の食い違いは
// SRV-003 で落ちる。エラーコード表の番号ごとの試験（TestErr_*。tools/errors_check が L1 に求める名）もここに置く。

// jsonOf は応答の本文を読む。
func jsonOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("本文が JSON でない: %s", b)
	}
	return m
}

// ok200 は 200 を確かめて本文を返す。
func (w *web) ok200(method, path, cookie string, body any, hdr map[string]string) map[string]any {
	w.e.t.Helper()
	resp, b := w.do(method, path, cookie, body, hdr)
	if resp.StatusCode != 200 {
		w.e.t.Fatalf("%s %s が 200 でない: %d %s", method, path, resp.StatusCode, b)
	}
	if len(b) == 0 || b[0] != '{' {
		return nil
	}
	return jsonOf(w.e.t, b)
}

// R: R-2-2-2
// 設置、招待、ログイン、ログアウトを HTTP で通す（S-02a、S-03a、S-04a。応答は spec で検証）。未承認の端末は session の経路だけを通り、
// 他は KEY-004（第2章 10）。同期されたパスキーで入った端末も未承認（T-S-07）、取り消した端末は AUTH-002。Set-Cookie は第2章 4.1 の属性。
func TestHttpSetupInviteLoginLogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.serve()
	if st := w.ok200("GET", "/api/v1/setup/status", "", nil, nil); st["state"] != "first_run" {
		t.Fatalf("設置の前の状態が違う: %v", st)
	}
	var token string
	e.must(e.tx(func(tx *store.Tx) error { var err error; token, err = e.id.Setup.Create(e.ctx, tx); return err }))
	w.ok200("GET", "/api/v1/setup/admin/"+token, "", nil, nil)
	ch := w.ok200("POST", "/api/v1/auth/passkey/register/options", "", map[string]any{"setup_token": token}, nil)
	resp, cred := e.auth.create(t, ch["options"].(map[string]any), credOpts{synced: true})
	r, b := w.do("POST", "/api/v1/setup/admin/"+token, "", map[string]any{"challenge_id": ch["challenge_id"], "family_name": "山田家",
		"display_name": "花子", "device_name": "花子の電話", "platform": "iphone", "passkey": resp}, nil)
	if r.StatusCode != 200 {
		t.Fatalf("設置が 200 でない: %d %s", r.StatusCode, b)
	}
	setCookie := strings.Join(r.Header.Values("Set-Cookie"), "\n")
	for _, attr := range []string{"__Host-session=", "Path=/", "Secure", "HttpOnly", "SameSite=Lax", "Max-Age="} {
		if !strings.Contains(setCookie, attr) {
			t.Fatalf("Set-Cookie に %s が無い: %s", attr, setCookie)
		}
	}
	cookie := sessionCookie(t, r)
	res := jsonOf(t, b)
	session := res["session"].(map[string]any)
	if res["new_device"] != true || session["approved"] != false || session["role"] != "admin" {
		t.Fatalf("設置の応答が違う: %s", b)
	}
	adminId := uuid.FromStringOrNil(session["user_id"].(string))
	device := uuid.FromStringOrNil(session["device_id"].(string))
	w.ok200("GET", "/api/v1/me/session", cookie, nil, nil)
	w.ok200("GET", "/api/v1/me/devices", cookie, nil, nil)
	r, b = w.do("GET", "/api/v1/me", cookie, nil, nil)
	w.problem(r, b, 403, cgen.KEY004)
	e.approve(device, adminId)
	if me := w.ok200("GET", "/api/v1/me", cookie, nil, nil); me["user"].(map[string]any)["display_name"] != "花子" {
		t.Fatalf("/me が違う: %v", me)
	}
	w.ok200("PATCH", "/api/v1/me", cookie, map[string]any{"display_name": "花ちゃん"}, nil)
	w.ok200("PUT", "/api/v1/me/settings", cookie, map[string]any{}, nil)
	w.ok200("GET", "/api/v1/me/settings", cookie, nil, nil)
	w.ok200("GET", "/api/v1/me/passkeys", cookie, nil, nil)
	w.ok200("GET", "/api/v1/users", cookie, nil, nil)
	w.ok200("GET", "/api/v1/users/"+adminId.String(), cookie, nil, nil)
	if f := w.ok200("GET", "/api/v1/family", cookie, nil, nil); f["member_count"] != float64(1) {
		t.Fatalf("本人の /family が全部の欄を出さない: %v", f)
	}
	if f := w.ok200("GET", "/api/v1/family", "", nil, nil); f["admin_names"] != nil || f["name"] != "山田家" {
		t.Fatalf("未認証の /family が名前とアイコンだけでない: %v", f)
	}
	w.ok200("GET", "/api/v1/family/disclosure", cookie, nil, nil)
	w.ok200("GET", "/api/v1/family/recovery-cards", cookie, nil, nil)
	w.ok200("POST", "/api/v1/auth/ws-token", cookie, map[string]any{}, nil)
	if m := w.ok200("GET", "/manifest.webmanifest", "", nil, nil); m["name"] != "山田家" || m["display"] != "standalone" {
		t.Fatalf("manifest が違う: %v", m)
	}
	if r, b := w.do("GET", "/api/v1/family/icon", "", nil, nil); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" || len(b) == 0 {
		t.Fatalf("家族のアイコンが PNG でない: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	// 招待と参加。
	inv := w.ok200("POST", "/api/v1/invites", cookie, map[string]any{"role": "member"}, map[string]string{"Idempotency-Key": newKey()})
	url := inv["url"].(string)
	tok := url[strings.LastIndex(url, "/")+1:]
	w.ok200("GET", "/api/v1/invite-tokens/"+tok, "", nil, nil)
	ch = w.ok200("POST", "/api/v1/auth/passkey/register/options", "", map[string]any{"invite_token": tok}, nil)
	resp, _ = e.auth.create(t, ch["options"].(map[string]any), credOpts{})
	r, b = w.do("POST", "/api/v1/invite-tokens/"+tok+"/accept", "", map[string]any{"challenge_id": ch["challenge_id"], "display_name": "太郎",
		"device_name": "太郎の電話", "platform": "android", "passkey": resp}, nil)
	if r.StatusCode != 200 {
		t.Fatalf("参加が 200 でない: %d %s", r.StatusCode, b)
	}
	sessionCookie(t, r)
	// ログインとログアウト。
	ch = w.ok200("POST", "/api/v1/auth/passkey/login/options", "", map[string]any{}, nil)
	assertion := e.auth.get(t, ch["options"].(map[string]any), cred, nil)
	r, b = w.do("POST", "/api/v1/auth/passkey/login/verify", "", map[string]any{"challenge_id": ch["challenge_id"], "response": assertion,
		"device_name": "居間の PC", "platform": "pc"}, nil)
	if r.StatusCode != 200 || jsonOf(t, b)["new_device"] != true {
		t.Fatalf("ログインが新しい端末の 200 でない: %d %s", r.StatusCode, b)
	}
	// 同期されたパスキーで入った別の端末は未承認で、session の経路だけを通る（T-S-07）。取り消すと AUTH-002 device_revoked。
	pcCookie := sessionCookie(t, r)
	pc := jsonOf(t, b)["session"].(map[string]any)["device_id"].(string)
	r, b = w.do("GET", "/api/v1/users", pcCookie, nil, nil)
	w.problem(r, b, 403, cgen.KEY004)
	w.ok200("GET", "/api/v1/me/session", pcCookie, nil, nil)
	w.ok200("DELETE", "/api/v1/me/devices/"+pc, cookie, nil, nil)
	r, b = w.do("GET", "/api/v1/me/session", pcCookie, nil, nil)
	if p := w.problem(r, b, 401, cgen.AUTH002); p["params"].(map[string]any)["reason"] != "device_revoked" {
		t.Fatalf("取り消した端末の理由が device_revoked でない: %s", b)
	}
	ch = w.ok200("POST", "/api/v1/auth/passkey/login/options", "", map[string]any{}, nil)
	assertion = e.auth.get(t, ch["options"].(map[string]any), cred, nil)
	r, b = w.do("POST", "/api/v1/auth/passkey/login/verify", "", map[string]any{"challenge_id": ch["challenge_id"], "response": assertion,
		"device_name": "書斎の PC", "platform": "pc"}, nil)
	if r.StatusCode != 200 {
		t.Fatalf("2 回目のログインが 200 でない: %d %s", r.StatusCode, b)
	}
	pcCookie = sessionCookie(t, r)
	r, _ = w.do("POST", "/api/v1/auth/logout", pcCookie, map[string]any{}, nil)
	if r.StatusCode != 200 || !strings.Contains(strings.Join(r.Header.Values("Set-Cookie"), ""), "Max-Age=0") {
		t.Fatalf("ログアウトが Cookie を消さない: %d", r.StatusCode)
	}
	r, b = w.do("GET", "/api/v1/me/session", pcCookie, nil, nil)
	if p := w.problem(r, b, 401, cgen.AUTH002); p["params"].(map[string]any)["reason"] != "logout" {
		t.Fatalf("ログアウトの後の理由が logout でない: %s", b)
	}
	w.ok200("GET", "/api/v1/me/session", cookie, nil, nil)
}

// httpFamily は HTTP の家族のサーバーと、端末を承認した管理者を作る。試験ごとに DB と時計が別なので並行に走らせる
// （srv-boundary の組み立てが 1 回に数秒かかる。時間に依る同時の試験は並行にしない）。
func httpFamily(t *testing.T) (*env, *web, *member) {
	t.Helper()
	t.Parallel()
	e := newEnv(t)
	w := e.serve()
	admin := e.setupAdmin("花子")
	e.approve(admin.device, admin.id)
	return e, w, admin
}

// TestErr_AUTH_001 は Cookie が無い・形が違う要求（401）。
func TestErr_AUTH_001(t *testing.T) {
	_, w, _ := httpFamily(t)
	for _, c := range []string{"", "not-a-token"} {
		r, b := w.do("GET", "/api/v1/me", c, nil, nil)
		w.problem(r, b, 401, cgen.AUTH001)
	}
}

// TestErr_AUTH_002 は失効したセッション（401。params.reason）。
func TestErr_AUTH_002(t *testing.T) {
	_, w, admin := httpFamily(t)
	w.ok200("POST", "/api/v1/auth/logout", admin.cookie, map[string]any{}, nil)
	r, b := w.do("GET", "/api/v1/me", admin.cookie, nil, nil)
	if p := w.problem(r, b, 401, cgen.AUTH002); p["params"].(map[string]any)["reason"] != "logout" {
		t.Fatalf("params.reason が logout でない: %s", b)
	}
}

// TestErr_AUTH_003 は WebAuthn の検証の失敗（400。無い challenge）。
func TestErr_AUTH_003(t *testing.T) {
	_, w, _ := httpFamily(t)
	r, b := w.do("POST", "/api/v1/auth/passkey/login/verify", "", map[string]any{"challenge_id": newKey(), "response": map[string]any{},
		"device_name": "PC", "platform": "pc"}, nil)
	w.problem(r, b, 400, cgen.AUTH003)
}

// TestErr_AUTH_004 はログインの検証の回数の超過（429。ratelimits.yaml の login：IP あたり 1 分 10 回）。
func TestErr_AUTH_004(t *testing.T) {
	_, w, _ := httpFamily(t)
	body := map[string]any{"challenge_id": newKey(), "response": map[string]any{}, "device_name": "PC", "platform": "pc"}
	for i := 0; i < 10; i++ {
		r, b := w.do("POST", "/api/v1/auth/passkey/login/verify", "", body, nil)
		w.problem(r, b, 400, cgen.AUTH003)
	}
	r, b := w.do("POST", "/api/v1/auth/passkey/login/verify", "", body, nil)
	w.problem(r, b, 429, cgen.AUTH004)
}

// TestErr_AUTH_005 は QR の承認待ち（400。params.state）。
func TestErr_AUTH_005(t *testing.T) {
	_, w, _ := httpFamily(t)
	jwk := map[string]any{"kty": "EC", "crv": "P-256", "x": b64.EncodeToString(make([]byte, 32)), "y": b64.EncodeToString(make([]byte, 32))}
	qr := w.ok200("POST", "/api/v1/auth/qr/start", "", map[string]any{"device_name": "PC", "platform": "pc", "sig_pub": jwk, "kem_pub": jwk}, nil)
	r, b := w.do("POST", "/api/v1/auth/qr/poll", "", map[string]any{"device_code": qr["device_code"]}, nil)
	if p := w.problem(r, b, 400, cgen.AUTH005); p["params"].(map[string]any)["state"] != qrPending || p["retryable"] != true {
		t.Fatalf("承認待ちの params か retryable が違う: %s", b)
	}
}

// TestErr_AUTH_006 は再認証が 5 分を過ぎた（403。パスキーの削除）。
func TestErr_AUTH_006(t *testing.T) {
	e, w, admin := httpFamily(t)
	e.clk.Advance(reauthWindow + time.Minute)
	r, b := w.do("DELETE", "/api/v1/me/passkeys/"+b64.EncodeToString(admin.cred.id), admin.cookie, nil, nil)
	w.problem(r, b, 403, cgen.AUTH006)
}

// TestErr_AUTH_007 は無効・使用済みの招待のトークン（410）。
func TestErr_AUTH_007(t *testing.T) {
	_, w, _ := httpFamily(t)
	r, b := w.do("GET", "/api/v1/invite-tokens/"+b64.EncodeToString(make([]byte, 32)), "", nil, nil)
	w.problem(r, b, 410, cgen.AUTH007)
}

// TestErr_AUTH_008 は最後のパスキーの削除（409）。
func TestErr_AUTH_008(t *testing.T) {
	_, w, admin := httpFamily(t)
	r, b := w.do("DELETE", "/api/v1/me/passkeys/"+b64.EncodeToString(admin.cred.id), admin.cookie, nil, nil)
	w.problem(r, b, 409, cgen.AUTH008)
}

// TestErr_MEM_001 は家族にいない構成員（404）。
func TestErr_MEM_001(t *testing.T) {
	_, w, admin := httpFamily(t)
	r, b := w.do("GET", "/api/v1/users/"+newKey(), admin.cookie, nil, nil)
	w.problem(r, b, 404, cgen.MEM001)
}

// TestErr_MEM_002 は状態が通常でない構成員の操作（409。抹消の猶予の間のカードの受け取り）。
func TestErr_MEM_002(t *testing.T) {
	e, w, admin := httpFamily(t)
	m := e.join(admin, "太郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Users.RequestErase(e.ctx, tx, m.id); return err }))
	r, b := w.do("POST", "/api/v1/me/recovery-cards/2", m.cookie, map[string]any{}, nil)
	w.problem(r, b, 409, cgen.MEM002)
}

// TestErr_MEM_003 は最後の管理者の抹消の求め（403）。
func TestErr_MEM_003(t *testing.T) {
	_, w, admin := httpFamily(t)
	r, b := w.do("POST", "/api/v1/me/erase", admin.cookie, map[string]any{}, map[string]string{"Idempotency-Key": newKey()})
	w.problem(r, b, 403, cgen.MEM003)
}

// TestErr_MEM_004 は未使用の招待の上限（409。設定 auth.unused_invites_max）。
func TestErr_MEM_004(t *testing.T) {
	e, w, admin := httpFamily(t)
	for i := int64(0); i < e.cfg.Get(cgen.KeyAuthUnusedInvitesMax).Int(); i++ {
		e.issueInvite(admin, InviteSpec{Role: boundary.RoleMember})
	}
	r, b := w.do("POST", "/api/v1/invites", admin.cookie, map[string]any{"role": "member"}, map[string]string{"Idempotency-Key": newKey()})
	w.problem(r, b, 409, cgen.MEM004)
}

// TestErr_MEM_005 はプロフィールの形（422。未来の誕生日）。
func TestErr_MEM_005(t *testing.T) {
	_, w, admin := httpFamily(t)
	r, b := w.do("PATCH", "/api/v1/me", admin.cookie, map[string]any{"birthday": "2999-01-01"}, nil)
	w.problem(r, b, 422, cgen.MEM005)
}

// TestErr_MEM_006 は保護者の承認が要る（403。子の抹消の求め）。
func TestErr_MEM_006(t *testing.T) {
	e, w, admin := httpFamily(t)
	kid := e.childOf(admin, "孫")
	r, b := w.do("POST", "/api/v1/me/erase", kid.cookie, map[string]any{}, map[string]string{"Idempotency-Key": newKey()})
	w.problem(r, b, 403, cgen.MEM006)
}

// TestErr_MEM_007 は子ども区分ではできない操作（403。自分の誕生日の変更）。
func TestErr_MEM_007(t *testing.T) {
	e, w, admin := httpFamily(t)
	kid := e.childOf(admin, "孫")
	r, b := w.do("PATCH", "/api/v1/me", kid.cookie, map[string]any{"birthday": "2015-01-01"}, nil)
	w.problem(r, b, 403, cgen.MEM007)
}

// TestErr_MEM_008 は明示の版が古い（409。params.version に今の版）。
func TestErr_MEM_008(t *testing.T) {
	e, w, admin := httpFamily(t)
	r, b := w.do("POST", "/api/v1/me/disclosure-ack", admin.cookie, map[string]any{"version": "0000000000000000"}, nil)
	doc, err := e.id.Families.Disclosure(e.ctx)
	e.must(err)
	if p := w.problem(r, b, 409, cgen.MEM008); p["params"].(map[string]any)["version"] != doc.Version {
		t.Fatalf("params.version が今の版でない: %s", b)
	}
}

// TestErr_MEM_009 は別の人が持つ札（409。params.holder）。
func TestErr_MEM_009(t *testing.T) {
	e, w, admin := httpFamily(t)
	m := e.join(admin, "太郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Families.MarkCardHeld(e.ctx, tx, admin.id, 2) }))
	r, b := w.do("POST", "/api/v1/me/recovery-cards/2", m.cookie, map[string]any{}, nil)
	if p := w.problem(r, b, 409, cgen.MEM009); p["params"].(map[string]any)["holder"] != "花子" {
		t.Fatalf("params.holder が持ち主の名前でない: %s", b)
	}
}

// TestErr_MEM_010 は子ども区分のカードの受け取り（403）。
func TestErr_MEM_010(t *testing.T) {
	e, w, admin := httpFamily(t)
	kid := e.childOf(admin, "孫")
	r, b := w.do("POST", "/api/v1/me/recovery-cards/3", kid.cookie, map[string]any{}, nil)
	w.problem(r, b, 403, cgen.MEM010)
}

// TestErr_MEM_011 は持てる構成員が 3 人以上の家族で別の札を持つ人の受け取り（409。params.held）。
func TestErr_MEM_011(t *testing.T) {
	e, w, admin := httpFamily(t)
	e.join(admin, "太郎", nil)
	e.join(admin, "次郎", nil)
	e.must(e.tx(func(tx *store.Tx) error { _, err := e.id.Families.MarkCardPrinted(e.ctx, tx, admin.id); return err }))
	r, b := w.do("POST", "/api/v1/me/recovery-cards/2", admin.cookie, map[string]any{}, nil)
	if p := w.problem(r, b, 409, cgen.MEM011); p["params"].(map[string]any)["held"] != float64(1) {
		t.Fatalf("params.held が持つ札の番号でない: %s", b)
	}
}

// TestErr_MEM_012 は 18 歳以上の誕生日の人への子ども区分（409）。経路 PUT /admin/users/{id}/child は srv-admin が持つので、
// 本ブロックの公開操作 Users.SetChild で確かめる（HTTP の分は srv-admin の試験）。
func TestErr_MEM_012(t *testing.T) {
	e, _, admin := httpFamily(t)
	m := e.join(admin, "太郎", nil)
	b := e.birthdayFor(30, 0)
	e.must(e.tx(func(tx *store.Tx) error {
		return e.id.Users.UpdateProfile(e.ctx, tx, m.id, gen.UpdateMeRequest{Birthday: &b})
	}))
	e.wantCode(e.tx(func(tx *store.Tx) error { return e.id.Users.SetChild(e.ctx, tx, m.id, true, admin.id) }), cgen.MEM012)
}

// R: R-3-1-2
// 招待のトークンの推測（T-S-01）：無効・期限切れ・使用済みは全部 AUTH-007、確かめの応答は成功も失敗も 250 ミリ秒の後、
// IP あたり 1 時間 20 回で NET-004（ratelimits.yaml の invite_token）。256 ビットのトークンを 10 万回試すには、この制限で 5,000 時間かかる。
func TestInviteTokenGuessingIsLimited(t *testing.T) {
	e, w, admin := httpFamily(t)
	valid := e.issueInvite(admin, InviteSpec{Role: boundary.RoleMember})
	used := e.issueInvite(admin, InviteSpec{Role: boundary.RoleMember})
	e.accept(used, "太郎")
	expired := e.issueInvite(admin, InviteSpec{Role: boundary.RoleMember})
	timed := func(tok string) (time.Duration, int, []byte) {
		start := wall()
		r, b := w.do("GET", "/api/v1/invite-tokens/"+tok, "", nil, nil)
		return wall().Sub(start), r.StatusCode, b
	}
	d, status, _ := timed(valid)
	if status != 200 || d < peekDelay {
		t.Fatalf("有効なトークンの確かめが 250 ミリ秒を待たない: %d %v", status, d)
	}
	// 期限切れは DB の期限を過去にして作る（時計を進めるとレート制限の窓も切り替わる）。
	_, err := e.pool.Exec(e.ctx, `UPDATE invites SET expires_at = $1 WHERE token_sha256 = $2`, e.clk.Now().Add(-time.Minute), mustHash(t, expired))
	e.must(err)
	for _, tok := range []string{used, expired, b64.EncodeToString(make([]byte, 32))} {
		d, status, b := timed(tok)
		if status != 410 || !strings.Contains(string(b), cgen.AUTH007) || d < peekDelay {
			t.Fatalf("無効なトークンが 250 ミリ秒の後の AUTH-007 でない: %d %v %s", status, d, b)
		}
	}
	for i := 4; i < 20; i++ {
		if _, status, _ := timed(b64.EncodeToString([]byte(strings.Repeat(string(rune('a'+i)), 32)))); status != 410 {
			t.Fatalf("%d 回目が 410 でない: %d", i+1, status)
		}
	}
	r, b := w.do("GET", "/api/v1/invite-tokens/"+b64.EncodeToString(make([]byte, 32)), "", nil, nil)
	w.problem(r, b, 429, cgen.NET004)
}

// mustHash は URL のトークンの SHA-256（DB の token_sha256）。
func mustHash(t *testing.T, tok string) []byte {
	t.Helper()
	h, ok := tokenHash(tok)
	if !ok {
		t.Fatalf("トークンの形が違う: %s", tok)
	}
	return h
}
