// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package keys

import (
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// R: R-12-3-2
// タグ keys の 23 操作（第2章 5.4）：経路の端末が主体の端末か（KEY-001）、再認証の要る時（AUTH-006）、base64url の解けない値の番号、応答の形、
// 規則 prf_or_approved と使用の記録、最後の手段の警告 KEY-006。
func TestApis(t *testing.T) {
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	b := e.member("太郎", boundary.RoleMember)
	c := e.member("次郎", boundary.RoleMember)
	api := e.k.Apis()
	now := e.clk.Now()
	fresh := now
	ad := a.dev()
	pa := e.principal(a.id, ad, nil)
	paR := e.principal(a.id, ad, &fresh)

	// 端末の鍵の登録と承認。
	nd := e.addDevice(a, "花子の新しい電話", false)
	pn := e.principal(a.id, nd, nil)
	_, err := api.PutMyDeviceKeys(pn, gen.PutMyDeviceKeysRequestObject{Id: ad.deviceId.String(), Body: &gen.PutMyDeviceKeysRequest{}})
	e.wantCode(err, cgen.KEY001)
	_, err = api.PutMyDeviceKeys(pn, gen.PutMyDeviceKeysRequestObject{Id: nd.deviceId.String(), Body: &gen.PutMyDeviceKeysRequest{
		SigPub: jwkOf(t, nd.sig), KemPub: jwkOf(t, nd.kem), MlsCredential: "!"}})
	e.wantCode(err, cgen.KEY009)
	pk, err := api.PutMyDeviceKeys(pn, gen.PutMyDeviceKeysRequestObject{Id: nd.deviceId.String(), Body: &gen.PutMyDeviceKeysRequest{
		SigPub: jwkOf(t, nd.sig), KemPub: jwkOf(t, nd.kem), MlsCredential: b64.EncodeToString(credentialFor(a.id, nd.deviceId))}})
	e.must(err)
	if f := pk.(gen.PutMyDeviceKeys200JSONResponse); f.DeviceId != nd.deviceId.String() {
		t.Fatalf("PutMyDeviceKeys: %+v", f)
	}
	approveReq := gen.ApproveMyDeviceRequestObject{Id: nd.deviceId.String(), Body: &gen.ApproveMyDeviceRequest{UserSig: b64.EncodeToString(userSigFor(t, a.usk, nd))}}
	_, err = api.ApproveMyDevice(pa, approveReq)
	e.wantCode(err, cgen.AUTH006)
	_, err = api.ApproveMyDevice(paR, gen.ApproveMyDeviceRequestObject{Id: nd.deviceId.String(), Body: &gen.ApproveMyDeviceRequest{UserSig: "!"}})
	e.wantCode(err, cgen.KEY003)
	_, err = api.ApproveMyDevice(paR, approveReq)
	e.must(err)
	rj, err := api.RejoinMyMls(pa, gen.RejoinMyMlsRequestObject{})
	e.must(err)
	if rj.(gen.RejoinMyMls200JSONResponse).Rooms != 0 {
		t.Fatal("RejoinMyMls の数")
	}

	// KeyPackage。
	_, err = api.CreateMyKeyPackages(pa, gen.CreateMyKeyPackagesRequestObject{Id: ad.deviceId.String(), Body: &gen.CreateMyKeyPackagesRequest{KeyPackages: []string{"!"}}})
	e.wantCode(err, cgen.KEY009)
	last := b64.EncodeToString(buildKeyPackage(t, ad, now, kpOpts{}))
	cr, err := api.CreateMyKeyPackages(pa, gen.CreateMyKeyPackagesRequestObject{Id: ad.deviceId.String(), Body: &gen.CreateMyKeyPackagesRequest{
		KeyPackages: []string{b64.EncodeToString(buildKeyPackage(t, ad, now, kpOpts{}))}, LastResort: &last}})
	e.must(err)
	if cr.(gen.CreateMyKeyPackages200JSONResponse).Count != 1 {
		t.Fatal("CreateMyKeyPackages の数")
	}
	_, err = api.GetMyKeyPackages(pa, gen.GetMyKeyPackagesRequestObject{Id: nd.deviceId.String()})
	e.wantCode(err, cgen.KEY001)
	gk, err := api.GetMyKeyPackages(pa, gen.GetMyKeyPackagesRequestObject{Id: ad.deviceId.String()})
	e.must(err)
	if g := gk.(gen.GetMyKeyPackages200JSONResponse); g.Count != 1 || !g.LastResort {
		t.Fatalf("GetMyKeyPackages: %+v", g)
	}
	pb := e.principal(b.id, b.dev(), nil)
	claim := gen.ClaimKeyPackageRequestObject{Uid: a.id.String(), Did: ad.deviceId.String()}
	c1, err := api.ClaimKeyPackage(pb, claim)
	e.must(err)
	c2, err := api.ClaimKeyPackage(pb, claim)
	e.must(err)
	if w := c1.(gen.ClaimKeyPackage200JSONResponse).Warnings; w != nil {
		t.Fatal("使い切りに警告が付いた")
	}
	if w := c2.(gen.ClaimKeyPackage200JSONResponse).Warnings; w == nil || (*w)[0].Code != cgen.KEY006 {
		t.Fatal("最後の手段に KEY-006 の警告が付かない")
	}

	// 鍵バンドルのコピー。
	self := ad.deviceId.String()
	_, err = api.PutMyKeyBundle(pa, gen.PutMyKeyBundleRequestObject{Kind: "device", Target: self, Body: ptr(copyOf(a.id, BundleDevice, self, 1, 0))})
	e.must(err)
	lb, err := api.ListMyKeyBundle(pa, gen.ListMyKeyBundleRequestObject{})
	e.must(err)
	if len(lb.(gen.ListMyKeyBundle200JSONResponse).Items) != 1 {
		t.Fatal("ListMyKeyBundle の数")
	}
	_, err = api.DeleteMyKeyBundle(pa, gen.DeleteMyKeyBundleRequestObject{Kind: "device", Target: self})
	e.wantCode(err, cgen.AUTH006)
	_, err = api.DeleteMyKeyBundle(paR, gen.DeleteMyKeyBundleRequestObject{Kind: "device", Target: self})
	e.must(err)
	// 規則 prf_or_approved：未承認の端末は prf だけ取れ、取ると使用の記録（承認済みの端末には書かない）。
	un := e.addDevice(a, "花子の 3 台目", false)
	pu := e.principal(a.id, un, nil)
	cred := b64.EncodeToString([]byte("cred-1"))
	prf := copyOf(a.id, BundlePrf, cred, 1, 0)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO keybundles (user_id, kind, target_id, key_version, body, created_at) VALUES ($1, 'prf', $2, 1, $3, $4)`,
		a.id, cred, prf, now); err != nil {
		t.Fatal(err)
	}
	_, err = api.GetMyKeyBundle(pu, gen.GetMyKeyBundleRequestObject{Kind: "device", Target: un.deviceId.String()})
	e.wantCode(err, cgen.KEY004)
	_, err = api.GetMyKeyBundle(pu, gen.GetMyKeyBundleRequestObject{Kind: "prf", Target: cred})
	e.must(err)
	_, err = api.GetMyKeyBundle(pa, gen.GetMyKeyBundleRequestObject{Kind: "prf", Target: cred})
	e.must(err)
	used := payloads[recoveryUsed](t, e.feedOf(a.id, "recovery.used"))
	if len(used) != 1 || used[0].Method != MethodPrf || *used[0].DeviceId != un.deviceId {
		t.Fatalf("prf の使用の記録: %+v", used)
	}

	// 回復コード。
	v := make([]byte, 32)
	rcReq := func(verifier, salt string) gen.PutMyRecoveryCodeRequestObject {
		body := &gen.PutMyRecoveryCodeRequest{Verifier: verifier}
		body.Params.Alg, body.Params.M, body.Params.T, body.Params.P, body.Params.Salt = "argon2id", 65536, 3, 4, salt
		return gen.PutMyRecoveryCodeRequestObject{Body: body}
	}
	_, err = api.PutMyRecoveryCode(pa, rcReq("!", b64.EncodeToString(a.id.Bytes())))
	e.wantCode(err, cgen.NET001)
	_, err = api.PutMyRecoveryCode(pa, rcReq(b64.EncodeToString(v), b64.EncodeToString(a.id.Bytes())))
	e.must(err)
	_, err = api.PutMyRecoveryCode(pa, rcReq(b64.EncodeToString(v), b64.EncodeToString(a.id.Bytes())))
	e.wantCode(err, cgen.AUTH006)
	_, err = api.PutMyRecoveryCode(paR, rcReq(b64.EncodeToString(v), b64.EncodeToString(a.id.Bytes())))
	e.must(err)
	rc := copyOf(a.id, BundleRecoveryCode, targetSelf, 1, 0)
	rc.Kdf = &struct {
		Alg interface{} `json:"alg"`
		M   int         `json:"m"`
		P   int         `json:"p"`
		// Salt RFC 4648 の base64url（詰め物なし。第12章 12）
		Salt gen.CommonBase64url1 `json:"salt"`
		T    int                  `json:"t"`
	}{Alg: "argon2id", M: 65536, P: 4, T: 3, Salt: b64.EncodeToString(a.id.Bytes())}
	_, err = api.PutMyKeyBundle(pa, gen.PutMyKeyBundleRequestObject{Kind: "recovery-code", Target: targetSelf, Body: &rc})
	e.must(err)
	_, err = api.PutMyKeyBundle(pa, gen.PutMyKeyBundleRequestObject{Kind: "recovery-code", Target: targetSelf, Body: &rc})
	e.wantCode(err, cgen.AUTH006)
	_, err = api.VerifyRecoveryCode(pu, gen.VerifyRecoveryCodeRequestObject{Body: &gen.VerifyRecoveryCodeRequest{Verifier: "!"}})
	e.wantCode(err, cgen.KEY012)
	vr, err := api.VerifyRecoveryCode(pu, gen.VerifyRecoveryCodeRequestObject{Body: &gen.VerifyRecoveryCodeRequest{Verifier: b64.EncodeToString(v)}})
	e.must(err)
	if vr.(gen.VerifyRecoveryCode200JSONResponse).Kind != gen.Fkb1KindRecoveryCode {
		t.Fatal("VerifyRecoveryCode のコピー")
	}

	// 本人の署名鍵の置き換え（再認証）。
	next := newKey(t)
	sigs := []struct {
		DeviceId gen.CommonUuidv71    `json:"device_id"`
		UserSig  gen.CommonBase64url1 `json:"user_sig"`
	}{}
	for _, dk := range []*devKeys{ad, nd} {
		sigs = append(sigs, struct {
			DeviceId gen.CommonUuidv71    `json:"device_id"`
			UserSig  gen.CommonBase64url1 `json:"user_sig"`
		}{DeviceId: dk.deviceId.String(), UserSig: b64.EncodeToString(userSigFor(t, next, dk))})
	}
	sk := gen.PutMySigningKeyRequestObject{Body: &gen.PutMySigningKeyRequest{UserSigPub: jwkOf(t, next), DeviceSigs: sigs}}
	_, err = api.PutMySigningKey(pa, sk)
	e.wantCode(err, cgen.AUTH006)
	fr, err := api.PutMySigningKey(paR, sk)
	e.must(err)
	if fr.(gen.PutMySigningKey200JSONResponse).Version != 2 {
		t.Fatal("PutMySigningKey の版")
	}

	// 家族に頼む（c の片を a と b が持つ）。
	for i, dk := range []*devKeys{ad, b.dev()} {
		target := dk.deviceId.String()
		e.must(e.putCopy(c.id, c.dev(), BundleShare, target, copyOf(c.id, BundleShare, target, 1, i+1)))
	}
	cn := e.addDevice(c, "次郎の新しい電話", false)
	rr, err := api.RequestRecovery(e.principal(c.id, cn, nil), gen.RequestRecoveryRequestObject{})
	e.must(err)
	rid := rr.(gen.RequestRecovery200JSONResponse).RequestId
	sh, err := api.GetRecoveryShare(pa, gen.GetRecoveryShareRequestObject{RequestId: rid})
	e.must(err)
	if sh.(gen.GetRecoveryShare200JSONResponse).TargetId != self {
		t.Fatal("GetRecoveryShare の対象")
	}
	ct := []byte("share")
	ar := gen.ApproveRecoveryRequestObject{RequestId: rid, Body: &gen.ApproveRecoveryRequest{Ciphertext: b64.EncodeToString(ct),
		Sig: b64.EncodeToString(signRS(t, ad.sig, inboxMessage(InboxShare, ad.deviceId, cn.deviceId, ct)))}}
	_, err = api.ApproveRecovery(pa, ar)
	e.wantCode(err, cgen.AUTH006)
	_, err = api.ApproveRecovery(paR, gen.ApproveRecoveryRequestObject{RequestId: rid, Body: &gen.ApproveRecoveryRequest{Ciphertext: "!", Sig: "!"}})
	e.wantCode(err, cgen.KEY009)
	_, err = api.ApproveRecovery(paR, ar)
	e.must(err)

	// 家族回復鍵。
	_, err = api.GetFamilyRecoveryKeyPublic(pa, gen.GetFamilyRecoveryKeyPublicRequestObject{})
	e.wantCode(err, cgen.KEY005)
	_, err = api.UseFamilyRecoveryKey(pa, gen.UseFamilyRecoveryKeyRequestObject{})
	e.wantCode(err, cgen.KEY005)

	// 鍵のバックアップ。
	gid := groupIdOf(uuid.Must(uuid.NewV7()), 0)
	hist := gen.FkbHistory1{V: 1, Alg: "A256GCM", UserId: a.id.String(), GroupId: b64.EncodeToString(gid), Epoch: 1, KeyVersion: 2,
		Nonce: b64.EncodeToString(make([]byte, 12)), Ciphertext: b64.EncodeToString([]byte{1})}
	kb, err := api.PutMyKeyBackup(pa, gen.PutMyKeyBackupRequestObject{GroupId: b64.EncodeToString(gid), Epoch: 1, Body: &hist})
	e.must(err)
	if _, ok := kb.(gen.PutMyKeyBackup201JSONResponse); !ok {
		t.Fatalf("新しく置いた時が 201 でない: %T", kb)
	}
	kb, err = api.PutMyKeyBackup(pa, gen.PutMyKeyBackupRequestObject{GroupId: b64.EncodeToString(gid), Epoch: 1, Body: &hist})
	e.must(err)
	if _, ok := kb.(gen.PutMyKeyBackup204Response); !ok {
		t.Fatalf("既にある時が 204 でない: %T", kb)
	}
	yes := true
	_, err = api.PutMyKeyBackup(pa, gen.PutMyKeyBackupRequestObject{GroupId: b64.EncodeToString(gid), Epoch: 1, Params: gen.PutMyKeyBackupParams{Rewrap: &yes}, Body: &hist})
	e.wantCode(err, cgen.AUTH006)
	_, err = api.PutMyKeyBackup(paR, gen.PutMyKeyBackupRequestObject{GroupId: b64.EncodeToString(gid), Epoch: 1, Params: gen.PutMyKeyBackupParams{Rewrap: &yes}, Body: &hist})
	e.must(err)
	_, err = api.PutMyKeyBackup(pa, gen.PutMyKeyBackupRequestObject{GroupId: "!", Epoch: 1, Body: &hist})
	e.wantCode(err, cgen.KEY009)
	lk, err := api.ListMyKeyBackup(pa, gen.ListMyKeyBackupRequestObject{})
	e.must(err)
	if len(lk.(gen.ListMyKeyBackup200JSONResponse).Items) != 1 {
		t.Fatal("ListMyKeyBackup の数")
	}

	// メールボックス。
	msg := []byte("key-request")
	in := gen.PutDeviceInboxRequestObject{Did: b.dev().deviceId.String(), Body: &gen.PutDeviceInboxRequest{Kind: "key_request",
		Ciphertext: b64.EncodeToString(msg), Sig: b64.EncodeToString(signRS(t, ad.sig, inboxMessage(InboxKeyRequest, ad.deviceId, b.dev().deviceId, msg)))}}
	_, err = api.PutDeviceInbox(pa, gen.PutDeviceInboxRequestObject{Did: b.dev().deviceId.String(), Body: &gen.PutDeviceInboxRequest{Kind: "key_request", Ciphertext: "!", Sig: "!"}})
	e.wantCode(err, cgen.KEY009)
	pi, err := api.PutDeviceInbox(pa, in)
	e.must(err)
	li, err := api.ListMyInbox(pb, gen.ListMyInboxRequestObject{})
	e.must(err)
	items := li.(gen.ListMyInbox200JSONResponse).Items
	if len(items) != 1 || items[0].Id != pi.(gen.PutDeviceInbox200JSONResponse).Id || items[0].Sig == nil {
		t.Fatalf("ListMyInbox: %+v", items)
	}
	_, err = api.DeleteMyInboxItem(pb, gen.DeleteMyInboxItemRequestObject{Id: items[0].Id})
	e.must(err)
	if e.count(`SELECT count(*) FROM device_inbox WHERE to_device = $1`, b.dev().deviceId) != 0 {
		t.Fatal("DeleteMyInboxItem で消えない")
	}
	// 再認証の 5 分。
	old := now.Add(-6 * time.Minute)
	_, err = api.DeleteMyKeyBundle(e.principal(a.id, ad, &old), gen.DeleteMyKeyBundleRequestObject{Kind: "family", Target: targetSelf})
	e.wantCode(err, cgen.AUTH006)
	// 主体の無い要求。
	_, err = api.ListMyInbox(boundary.WithPrincipal(e.ctx, boundary.Principal{Kind: boundary.KindNone}), gen.ListMyInboxRequestObject{})
	e.wantCode(err, cgen.AUTH001)
}

func ptr[T any](v T) *T { return &v }
