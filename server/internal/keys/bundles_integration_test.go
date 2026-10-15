// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package keys

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// copyOf は kind のコピー（fkb/1）。share は番号 index（0 なら share の欄なし）。
func copyOf(user uuid.UUID, kind BundleKind, target string, version, index int) gen.Fkb1 {
	f := gen.Fkb1{V: 1, Kind: gen.Fkb1Kind(kind), UserId: user.String(), TargetId: target, KeyVersion: version, Alg: algAes,
		Nonce: b64.EncodeToString(make([]byte, 12)), Ciphertext: b64.EncodeToString([]byte{1, 2, 3})}
	if kind == BundleShare || kind == BundleFamily {
		f.Alg = algHpke
	}
	if index > 0 {
		f.Share = &struct {
			Index     int         `json:"index"`
			N         int         `json:"n"`
			Threshold interface{} `json:"threshold"`
		}{Index: index, N: 2, Threshold: 2}
	}
	return f
}

func (e *env) putCopy(user uuid.UUID, by *devKeys, kind BundleKind, target string, f gen.Fkb1) error {
	return e.tx(func(tx *store.Tx) error { return e.k.KeyBundles.Put(e.ctx, tx, user, by.deviceId, kind, target, f) })
}

// R: R-12-3-2
// 鍵バンドルのコピー（第12章 5.4、12）：系統ごとの対象（device は要求した端末、share は本人以外の承認済みの端末、recovery-code と family は
// self）、形（alg、kdf は回復コードの params と同じ、share の欄）、経路と本文の食い違い、片の番号（同じ構成員の端末は同じ、別の構成員は違う）、
// 版（KEY-011）。一覧、取得（無ければ KEY-005）、削除（device は自分の分だけ）、keybundle.changed。
func TestKeyBundles(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	b2 := e.addDevice(b, "太郎のタブレット", true)
	ad := a.dev()
	self := ad.deviceId.String()
	e.must(e.putCopy(a.id, ad, BundleDevice, self, copyOf(a.id, BundleDevice, self, 1, 0)))
	e.wantCode(e.putCopy(a.id, ad, BundleDevice, b.dev().deviceId.String(), copyOf(a.id, BundleDevice, b.dev().deviceId.String(), 1, 0)), cgen.KEY001)
	e.wantCode(e.putCopy(a.id, ad, BundleDevice, self, copyOf(b.id, BundleDevice, self, 1, 0)), cgen.KEY009)
	bad := copyOf(a.id, BundleDevice, self, 1, 0)
	bad.Alg = algHpke
	e.wantCode(e.putCopy(a.id, ad, BundleDevice, self, bad), cgen.KEY009)
	bad = copyOf(a.id, BundleDevice, self, 1, 0)
	bad.Ciphertext = ""
	e.wantCode(e.putCopy(a.id, ad, BundleDevice, self, bad), cgen.KEY009)
	// 片：本人以外の承認済みの端末へ。同じ構成員の 2 台は同じ番号、別の構成員は違う番号。
	for _, s := range []struct {
		dk    *devKeys
		index int
		code  string
	}{{b.dev(), 1, ""}, {b2, 1, ""}, {c.dev(), 1, cgen.KEY009}, {c.dev(), 2, ""}, {b2, 3, cgen.KEY009}, {ad, 4, cgen.KEY001}} {
		target := s.dk.deviceId.String()
		err := e.putCopy(a.id, ad, BundleShare, target, copyOf(a.id, BundleShare, target, 1, s.index))
		if s.code == "" {
			e.must(err)
		} else {
			e.wantCode(err, s.code)
		}
	}
	noShare := copyOf(a.id, BundleShare, c.dev().deviceId.String(), 1, 0)
	e.wantCode(e.putCopy(a.id, ad, BundleShare, c.dev().deviceId.String(), noShare), cgen.KEY009)
	// 回復コード：照合の値の行が無ければ kdf を照らせない（KEY-009）。行があり params が同じなら置ける。
	rc := copyOf(a.id, BundleRecoveryCode, targetSelf, 1, 0)
	rc.Kdf = &struct {
		Alg interface{} `json:"alg"`
		M   int         `json:"m"`
		P   int         `json:"p"`
		// Salt RFC 4648 の base64url（詰め物なし。第12章 12）
		Salt gen.CommonBase64url1 `json:"salt"`
		T    int                  `json:"t"`
	}{Alg: "argon2id", M: 65536, P: 4, T: 3, Salt: b64.EncodeToString(a.id.Bytes())}
	e.wantCode(e.putCopy(a.id, ad, BundleRecoveryCode, targetSelf, rc), cgen.KEY009)
	e.must(e.tx(func(tx *store.Tx) error {
		return e.k.RecoveryCode.PutVerifier(e.ctx, tx, a.id, ad.deviceId, make([]byte, 32), RecoveryParams{M: 65536, T: 3, P: 4, Salt: a.id.Bytes()})
	}))
	e.must(e.putCopy(a.id, ad, BundleRecoveryCode, targetSelf, rc))
	rc.Kdf.T = 4
	e.wantCode(e.putCopy(a.id, ad, BundleRecoveryCode, targetSelf, rc), cgen.KEY009)
	e.wantCode(e.putCopy(a.id, ad, BundleFamily, "x", copyOf(a.id, BundleFamily, "x", 1, 0)), cgen.KEY001)
	e.must(e.putCopy(a.id, ad, BundleFamily, targetSelf, copyOf(a.id, BundleFamily, targetSelf, 1, 0)))
	// prf：知らない資格情報は KEY-009。
	cred := b64.EncodeToString([]byte("unknown"))
	e.wantCode(e.putCopy(a.id, ad, BundlePrf, cred, copyOf(a.id, BundlePrf, cred, 1, 0)), cgen.KEY009)
	e.wantCode(e.putCopy(a.id, ad, BundleDevice, self, copyOf(a.id, BundleDevice, self, 2, 0)), cgen.KEY011)
	list, err := e.k.KeyBundles.List(e.ctx, a.id)
	e.must(err)
	if len(list) != 6 {
		t.Fatalf("一覧の数: %d", len(list))
	}
	got, err := e.k.KeyBundles.Get(e.ctx, e.pool, a.id, BundleDevice, self)
	e.must(err)
	if got.UserId != a.id.String() || got.CreatedAt.IsZero() {
		t.Fatalf("Get: %+v", got)
	}
	_, err = e.k.KeyBundles.Get(e.ctx, e.pool, b.id, BundleDevice, self)
	if ev := e.wantCode(err, cgen.KEY005); ev.Params["kind"] != "device" {
		t.Fatalf("KEY-005 の params: %+v", ev.Params)
	}
	err = e.tx(func(tx *store.Tx) error {
		return e.k.KeyBundles.Delete(e.ctx, tx, a.id, ad.deviceId, BundleDevice, b.dev().deviceId.String())
	})
	e.wantCode(err, cgen.KEY005)
	e.must(e.tx(func(tx *store.Tx) error {
		return e.k.KeyBundles.Delete(e.ctx, tx, a.id, ad.deviceId, BundleFamily, targetSelf)
	}))
	err = e.tx(func(tx *store.Tx) error {
		return e.k.KeyBundles.Delete(e.ctx, tx, a.id, ad.deviceId, BundleFamily, targetSelf)
	})
	e.wantCode(err, cgen.KEY005)
	holders, err := e.k.Shares.Holders(e.ctx, e.pool, a.id)
	e.must(err)
	if len(holders) != 2 || !containsId(holders, b.id) || !containsId(holders, c.id) {
		t.Fatalf("Holders: %v", holders)
	}
	if n := len(e.feedOf(a.id, "keybundle.changed")); n != 7 {
		t.Fatalf("keybundle.changed の数: %d", n)
	}
	// 端末の取り消しでその端末へ包んだ片とコピー (a) が消える。
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.Revoke(e.ctx, tx, b2.deviceId, b.id, false) }))
	if n := e.count(`SELECT count(*) FROM keybundles WHERE target_id = $1`, b2.deviceId.String()); n != 0 {
		t.Fatalf("取り消した端末の片が残る: %d", n)
	}
}

// R: R-3-3-5
// 回復コード（第12章 5.4 (b)）：照合の値の形（32 バイト、salt は本人の識別子。違えば NET-001）、置き換えは承認済みの端末だけ（KEY-004）。
// 照らし：合えばコピー (b) と recovery.used（success）と知らせ。合わなければ KEY-012 で、recovery.used（failure）は要求が失敗しても残る。
// 照合の値が無ければ KEY-005。
func TestRecoveryCode(t *testing.T) {
	e := newEnv(t)
	admin := e.member("花子", boundary.RoleAdmin)
	a := e.member("太郎", boundary.RoleMember)
	nd := e.addDevice(a, "太郎の新しい電話", false)
	verify := func(v []byte) (gen.Fkb1, error) {
		var f gen.Fkb1
		err := e.tx(func(tx *store.Tx) error {
			var err error
			f, err = e.k.RecoveryCode.Verify(e.ctx, tx, a.id, nd.deviceId, v)
			return err
		})
		return f, err
	}
	_, err := verify(make([]byte, 32))
	e.wantCode(err, cgen.KEY005)
	put := func(by *devKeys, v []byte, salt []byte) error {
		return e.tx(func(tx *store.Tx) error {
			return e.k.RecoveryCode.PutVerifier(e.ctx, tx, a.id, by.deviceId, v, RecoveryParams{M: 65536, T: 3, P: 4, Salt: salt})
		})
	}
	e.wantCode(put(nd, make([]byte, 31), a.id.Bytes()), cgen.NET001)
	e.wantCode(put(nd, make([]byte, 32), admin.id.Bytes()), cgen.NET001)
	v := make([]byte, 32)
	_, _ = rand.Read(v)
	e.must(put(nd, v, a.id.Bytes()))
	e.wantCode(put(nd, v, a.id.Bytes()), cgen.KEY004)
	e.must(put(a.dev(), v, a.id.Bytes()))
	rc := copyOf(a.id, BundleRecoveryCode, targetSelf, 1, 0)
	rc.Kdf = &struct {
		Alg interface{} `json:"alg"`
		M   int         `json:"m"`
		P   int         `json:"p"`
		// Salt RFC 4648 の base64url（詰め物なし。第12章 12）
		Salt gen.CommonBase64url1 `json:"salt"`
		T    int                  `json:"t"`
	}{Alg: "argon2id", M: 65536, P: 4, T: 3, Salt: b64.EncodeToString(a.id.Bytes())}
	// コピー (b) が無い時は照合が合っても KEY-005（使用の記録は書かない）。
	_, err = verify(v)
	e.wantCode(err, cgen.KEY005)
	e.must(e.putCopy(a.id, a.dev(), BundleRecoveryCode, targetSelf, rc))
	wrong := bytes.Clone(v)
	wrong[0] ^= 1
	_, err = verify(wrong)
	e.wantCode(err, cgen.KEY012)
	_, err = verify(v[:16])
	e.wantCode(err, cgen.KEY012)
	f, err := verify(v)
	e.must(err)
	if f.Kind != gen.Fkb1KindRecoveryCode {
		t.Fatalf("コピー: %+v", f)
	}
	used := payloads[recoveryUsed](t, e.feedOf(a.id, "recovery.used"))
	results := []RecoveryResult{}
	for _, u := range used {
		results = append(results, u.Result)
	}
	// 照合の値が無い時の失敗、合わない 2 回、成功（コピーが無い時の照合は数えない）。
	if len(used) != 4 || results[0] != ResultFailure || results[3] != ResultSuccess || *used[3].DeviceId != nd.deviceId {
		t.Fatalf("recovery.used: %+v", used)
	}
	if n := len(e.feedOf(admin.id, "recovery.used")); n != 4 {
		t.Fatalf("管理者への recovery.used: %d", n)
	}
	if n := len(e.notices.of(noticeRecoveryUsed)); n != 8 {
		t.Fatalf("知らせ recovery_used（本人と管理者の 2 つずつ）: %d", n)
	}
}

// R: R-12-3-2
// 家族に頼む（第12章 5.4 (c)）：片を持つ構成員が 2 人に満たなければ KEY-005、承認済みの端末は KEY-002。求めは recovery.request と知らせ
// recovery_requested を片を持つ人へ。片のコピーの取得（ShareFor）は片を持つ人の端末だけ（AUTH-007）。承認はメールボックスの share を置き、
// 同じ構成員の 2 台目は数えず、別の構成員の 2 人で approved と recovery.used（share）。open でない求めの承認は AUTH-007。
func TestSharesRecovery(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	d := e.member("三郎", boundary.RoleMember)
	b2 := e.addDevice(b, "太郎のタブレット", true)
	nd := e.addDevice(a, "花子の新しい電話", false)
	request := func(dev *devKeys) (RecoveryRequest, error) {
		var r RecoveryRequest
		err := e.tx(func(tx *store.Tx) error {
			var err error
			r, err = e.k.Shares.Request(e.ctx, tx, dev.deviceId)
			return err
		})
		return r, err
	}
	_, err := request(nd)
	e.wantCode(err, cgen.KEY005)
	for i, dk := range []*devKeys{b.dev(), b2, c.dev()} {
		idx := 1
		if i == 2 {
			idx = 2
		}
		target := dk.deviceId.String()
		e.must(e.putCopy(a.id, a.dev(), BundleShare, target, copyOf(a.id, BundleShare, target, 1, idx)))
	}
	_, err = request(a.dev())
	e.wantCode(err, cgen.KEY002)
	req, err := request(nd)
	e.must(err)
	again, err := request(nd)
	e.must(err)
	if again.RequestId != req.RequestId {
		t.Fatal("open の求めが 2 つできた")
	}
	rid := uuid.FromStringOrNil(req.RequestId)
	if n := len(e.feedOf(b.id, "recovery.request")); n != 1 {
		t.Fatalf("太郎への recovery.request: %d", n)
	}
	if n := len(e.feedOf(d.id, "recovery.request")); n != 0 {
		t.Fatalf("片の無い人への recovery.request: %d", n)
	}
	if n := len(e.notices.of(noticeRecoveryRequested)); n != 2 {
		t.Fatalf("知らせ recovery_requested: %d", n)
	}
	share, err := e.k.Shares.ShareFor(e.ctx, e.pool, rid, b2.deviceId)
	e.must(err)
	if share.TargetId != b2.deviceId.String() || share.Share.Index != 1 {
		t.Fatalf("片のコピー: %+v", share)
	}
	_, err = e.k.Shares.ShareFor(e.ctx, e.pool, rid, d.dev().deviceId)
	e.wantCode(err, cgen.AUTH007)
	approve := func(by *member, dk *devKeys) error {
		ct := []byte("share-ciphertext")
		sig := signRS(t, dk.sig, inboxMessage(InboxShare, dk.deviceId, nd.deviceId, ct))
		return e.tx(func(tx *store.Tx) error { return e.k.Shares.Approve(e.ctx, tx, dk.deviceId, rid, ct, sig) })
	}
	e.wantCode(approve(d, d.dev()), cgen.AUTH007)
	e.must(approve(b, b.dev()))
	e.must(approve(b, b2))
	var state string
	e.scalar(&state, `SELECT state FROM recovery_requests WHERE id = $1`, rid)
	if state != "open" || e.count(`SELECT count(*) FROM recovery_approvals WHERE request_id = $1`, rid) != 1 {
		t.Fatalf("同じ構成員の 2 台目を数えた: %s", state)
	}
	e.must(approve(c, c.dev()))
	e.scalar(&state, `SELECT state FROM recovery_requests WHERE id = $1`, rid)
	if state != "approved" {
		t.Fatalf("2 人で approved にならない: %s", state)
	}
	if n := e.count(`SELECT count(*) FROM device_inbox WHERE to_device = $1 AND kind = 'share'`, nd.deviceId); n != 2 {
		t.Fatalf("片のメールボックス: %d", n)
	}
	used := payloads[recoveryUsed](t, e.feedOf(a.id, "recovery.used"))
	if len(used) != 1 || used[0].Method != MethodShare {
		t.Fatalf("recovery.used: %+v", used)
	}
	e.wantCode(approve(c, c.dev()), cgen.AUTH007)
	_, err = e.k.Shares.ShareFor(e.ctx, e.pool, rid, b.dev().deviceId)
	e.wantCode(err, cgen.AUTH007)
	// 期限切れ。
	nd2 := e.addDevice(c, "次郎の新しい電話", false)
	for _, dk := range []*devKeys{a.dev(), b.dev()} {
		target := dk.deviceId.String()
		idx := 1
		if dk == b.dev() {
			idx = 2
		}
		e.must(e.putCopy(c.id, c.dev(), BundleShare, target, copyOf(c.id, BundleShare, target, 1, idx)))
	}
	r2, err := request(nd2)
	e.must(err)
	e.clk.Advance(25 * time.Hour)
	n, err := e.k.Shares.Expire(e.ctx)
	e.must(err)
	if n != 1 {
		t.Fatalf("期限切れの数: %d", n)
	}
	_, err = e.k.Shares.ShareFor(e.ctx, e.pool, uuid.FromStringOrNil(r2.RequestId), a.dev().deviceId)
	e.wantCode(err, cgen.AUTH007)
}

// R: R-12-3-2
// 家族回復鍵（第12章 5.4 (d)）：公開部が無ければ KEY-005。置くと版が 1 増え、古いコピー (d) が消え、全員に keybundle.changed。使用はコピーを
// 返し、全員へ recovery.used（family）と知らせ family_key_used。コピーが無ければ KEY-005（記録しない）。
func TestFamilyRecoveryKey(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	_, err := e.k.FamilyRecoveryKey.Public(e.ctx)
	e.wantCode(err, cgen.KEY005)
	e.must(e.putCopy(b.id, b.dev(), BundleFamily, targetSelf, copyOf(b.id, BundleFamily, targetSelf, 1, 0)))
	set := func(pub gen.CommonJwkP2561) (int, error) {
		var v int
		err := e.tx(func(tx *store.Tx) error {
			var err error
			v, err = e.k.FamilyRecoveryKey.SetPublic(e.ctx, tx, pub, a.id)
			return err
		})
		return v, err
	}
	_, err = set(gen.CommonJwkP2561{Crv: "P-256", Kty: "EC", X: vecG.X, Y: vec2G.Y})
	e.wantCode(err, cgen.KEY009)
	v, err := set(vecG)
	e.must(err)
	if v != 1 || e.count(`SELECT count(*) FROM keybundles WHERE kind = 'family'`) != 0 {
		t.Fatal("版か古いコピーの削除が違う")
	}
	pk, err := e.k.FamilyRecoveryKey.Public(e.ctx)
	e.must(err)
	if pk.Version != 1 || pk.PublicKey != vecG {
		t.Fatalf("Public: %+v", pk)
	}
	use := func(m *member) (gen.Fkb1, error) {
		var f gen.Fkb1
		err := e.tx(func(tx *store.Tx) error {
			var err error
			f, err = e.k.FamilyRecoveryKey.Use(e.ctx, tx, m.id, m.dev().deviceId)
			return err
		})
		return f, err
	}
	_, err = use(b)
	e.wantCode(err, cgen.KEY005)
	e.must(e.putCopy(b.id, b.dev(), BundleFamily, targetSelf, copyOf(b.id, BundleFamily, targetSelf, 1, 0)))
	f, err := use(b)
	e.must(err)
	if f.Kind != gen.Fkb1KindFamily {
		t.Fatalf("コピー: %+v", f)
	}
	for _, m := range []*member{a, b} {
		used := payloads[recoveryUsed](t, e.feedOf(m.id, "recovery.used"))
		if len(used) != 1 || used[0].Method != MethodFamily || used[0].UserId != b.id {
			t.Fatalf("recovery.used: %+v", used)
		}
	}
	if fk := e.notices.of(noticeFamilyKeyUsed); len(fk) != 1 || fk[0].audience != identity.AudienceAll {
		t.Fatalf("family_key_used: %+v", fk)
	}
}

// R: R-12-3-2
// 端末宛メールボックス（第12章 5.2、第2章 5.4）：送り手の署名、64 KiB、宛先の鍵の登録と状態、keybundle は本人の端末だけ、welcome と share は
// 端末から置けない（プログラムの誤り）、1 端末 1,000 件で KEY-010（片と Welcome は数えない）、device.inbox は宛先の端末だけ。一覧と削除と
// 期限、取り消しで宛てた物と送った物が消える。
func TestInbox(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	a2 := e.addDevice(a, "花子のタブレット", false)
	put := func(from, to *devKeys, kind InboxKind, ct []byte, sig []byte) (uuid.UUID, error) {
		if sig == nil {
			sig = signRS(t, from.sig, inboxMessage(kind, from.deviceId, to.deviceId, ct))
		}
		var id uuid.UUID
		err := e.tx(func(tx *store.Tx) error {
			var err error
			id, err = e.k.Inbox.Put(e.ctx, tx, from.deviceId, to.deviceId, kind, ct, sig)
			return err
		})
		return id, err
	}
	id, err := put(a.dev(), b.dev(), InboxKeyRequest, []byte("ct"), nil)
	e.must(err)
	_, err = put(a.dev(), b.dev(), InboxKeyRequest, []byte("ct"), signRS(t, b.dev().sig, []byte("x")))
	e.wantCode(err, cgen.KEY009)
	_, err = put(a.dev(), b.dev(), InboxKeyRequest, make([]byte, 64<<10+1), nil)
	e.wantCode(err, cgen.KEY009)
	_, err = put(a.dev(), b.dev(), InboxKeybundle, []byte("s"), nil)
	e.wantCode(err, cgen.KEY009)
	_, err = put(a.dev(), a2, InboxKeybundle, []byte("s"), nil)
	e.must(err)
	for _, k := range []InboxKind{InboxWelcome, InboxShare} {
		if _, err := put(a.dev(), b.dev(), k, []byte("x"), nil); err == nil || isEvent(err, cgen.KEY009) {
			t.Fatalf("%s を端末から置けた: %v", k, err)
		}
	}
	di := e.feedOf(b.id, "device.inbox")
	if len(di) != 1 || di[0].Ref.DeviceId != b.dev().deviceId || payloads[deviceInbox](t, di)[0].Count != 1 {
		t.Fatalf("device.inbox: %+v", di)
	}
	items, more, err := e.k.Inbox.ListSince(e.ctx, b.dev().deviceId, 0, 50)
	e.must(err)
	if len(items) != 1 || more || items[0].Id != id || items[0].FromDevice != a.dev().deviceId || len(items[0].Sig) != 64 {
		t.Fatalf("一覧: %+v", items)
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Inbox.Delete(e.ctx, tx, a.dev().deviceId, id) }))
	if e.count(`SELECT count(*) FROM device_inbox WHERE id = $1`, id) != 1 {
		t.Fatal("他の端末宛ての物を消せた")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Inbox.Delete(e.ctx, tx, b.dev().deviceId, id) }))
	// 1 端末 1,000 件。
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO device_inbox (id, to_device, from_device, kind, ciphertext, sig, created_at, expires_at)
SELECT gen_random_uuid(), $1, $2, 'key_request', '\x00', decode(repeat('00', 64), 'hex'), $3, $4 FROM generate_series(1, 1000)`,
		b.dev().deviceId, a.dev().deviceId, e.clk.Now(), e.clk.Now().Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = put(a.dev(), b.dev(), InboxKeyRequest, []byte("ct"), nil)
	e.wantCode(err, cgen.KEY010)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.Inbox.PutWelcome(e.ctx, tx, a.dev().deviceId, b.dev().deviceId, []byte("welcome"))
		return err
	}))
	items, more, err = e.k.Inbox.ListSince(e.ctx, b.dev().deviceId, 0, 10)
	e.must(err)
	if len(items) != 10 || !more {
		t.Fatalf("頁: %d %v", len(items), more)
	}
	e.clk.Advance(31 * 24 * time.Hour)
	n, err := e.k.Inbox.Prune(e.ctx)
	e.must(err)
	if n != 1002 {
		t.Fatalf("期限の掃除: %d", n)
	}
	_, err = put(b.dev(), a.dev(), InboxKeyRequest, []byte("ct"), nil)
	e.must(err)
	_, err = put(a.dev(), b.dev(), InboxKeyRequest, []byte("ct"), nil)
	e.must(err)
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Devices.Revoke(e.ctx, tx, b.dev().deviceId, b.id, true) }))
	if n := e.count(`SELECT count(*) FROM device_inbox WHERE to_device = $1 OR from_device = $1`, b.dev().deviceId); n != 0 {
		t.Fatalf("取り消した端末の項目が残る: %d", n)
	}
	_, err = put(a.dev(), b.dev(), InboxKeyRequest, []byte("ct"), nil)
	e.wantCode(err, cgen.KEY001)
}

// R: R-12-3-2
// 鍵のバックアップ（第12章 5.6）：グループの識別子は 20 バイト、経路と本文の食い違いは KEY-009、版は KEY-011、既にあれば上書きしない、
// 置き換え（rewrap）は pos を新しく振る。一覧は pos の順と has_more。
func TestKeyBackup(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	gid := groupIdOf(uuid.Must(uuid.NewV7()), 0)
	h := func(g []byte, epoch, version int) gen.FkbHistory1 {
		return gen.FkbHistory1{V: 1, Alg: "A256GCM", UserId: a.id.String(), GroupId: b64.EncodeToString(g), Epoch: epoch, KeyVersion: version,
			Nonce: b64.EncodeToString(make([]byte, 12)), Ciphertext: b64.EncodeToString([]byte{1})}
	}
	put := func(g []byte, epoch int, body gen.FkbHistory1) (bool, error) {
		var ok bool
		err := e.tx(func(tx *store.Tx) error {
			var err error
			ok, err = e.k.KeyBackup.Put(e.ctx, tx, a.id, g, int64(epoch), body)
			return err
		})
		return ok, err
	}
	_, err := put(gid[:16], 1, h(gid[:16], 1, 1))
	e.wantCode(err, cgen.KEY009)
	_, err = put(gid, 2, h(gid, 1, 1))
	e.wantCode(err, cgen.KEY009)
	_, err = put(gid, 1, h(gid, 1, 2))
	e.wantCode(err, cgen.KEY011)
	ok, err := put(gid, 1, h(gid, 1, 1))
	e.must(err)
	again, err := put(gid, 1, h(gid, 1, 1))
	e.must(err)
	ok2, err := put(gid, 2, h(gid, 2, 1))
	e.must(err)
	if !ok || again || !ok2 {
		t.Fatalf("201 と 204: %v %v %v", ok, again, ok2)
	}
	list, more, err := e.k.KeyBackup.ListSince(e.ctx, a.id, 0, 1)
	e.must(err)
	if len(list) != 1 || !more || list[0].Backup.Epoch != 1 {
		t.Fatalf("一覧: %+v %v", list, more)
	}
	first := list[0].Pos
	e.must(e.tx(func(tx *store.Tx) error { return e.k.KeyBackup.Replace(e.ctx, tx, a.id, gid, 1, h(gid, 1, 1)) }))
	list, _, err = e.k.KeyBackup.ListSince(e.ctx, a.id, int64(first), 10)
	e.must(err)
	if len(list) != 2 || list[1].Backup.Epoch != 1 {
		t.Fatalf("置き換えの後の一覧（pos が新しい）: %+v", list)
	}
}
