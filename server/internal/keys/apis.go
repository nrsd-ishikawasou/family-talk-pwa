// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"
	"strconv"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Apis は生成のタグ keys の 23 操作の実装（gen.Implementations の Keys の欄。ブロックのページの受ける節）。
// 各操作は主体を boundary.PrincipalFrom(ctx) で取り、書き込みは store.TxFrom(ctx) のトランザクション（無ければ開く）で行う。
// *fault.Event はそのまま error で返す（srv-boundary の strict の口が problem にする）。x-reauth の 5 分と x-authz-rule は本ブロックが判定する。
// /rooms/{id}/mls/* の 5 操作はタグ rooms で srv-rooms が実装し、Groups を呼ぶ。
type Apis struct {
	k *Keys
}

var _ gen.KeysApi = (*Apis)(nil)

// pageDefault は一覧の件数の既定（第2章 3）。範囲は境界が確かめる。
const pageDefault = 50

// self は本人の経路の主体の構成員と端末（シェルの主体もセッションの構成員と端末を持つ）。
func self(ctx context.Context) (boundary.Principal, uuid.UUID, uuid.UUID, error) {
	p := boundary.PrincipalFrom(ctx)
	if (p.Kind != boundary.KindUser && p.Kind != boundary.KindShell) || p.UserId == nil || p.DeviceId == nil {
		return p, uuid.Nil, uuid.Nil, ev(cgen.AUTH001)
	}
	return p, *p.UserId, *p.DeviceId, nil
}

// parseId は経路の識別子を読む（形は境界の検証が済ませている。読めなければ NET-001）。
func parseId(s, path string) (uuid.UUID, error) {
	u, err := uuid.FromString(s)
	if err != nil {
		return uuid.Nil, errNet001(path, "UUIDv7 でない")
	}
	return u, nil
}

// requireReauth は再認証が 5 分以内か。足りなければ AUTH-006。
func (a *Apis) requireReauth(p boundary.Principal) error {
	if !a.k.d.reauthed(p) {
		return ev(cgen.AUTH006)
	}
	return nil
}

// ownDevice は経路の端末の識別子が主体の端末か（違えば KEY-001。device_self は境界が確かめるが、shell は経路を見ない）。
func ownDevice(s string, device uuid.UUID) error {
	id, err := parseId(s, "path.id")
	if err != nil {
		return err
	}
	if id != device {
		return ev(cgen.KEY001)
	}
	return nil
}

// orPage は since と limit の既定。
func orPage(since, limit *int) (int64, int) {
	s, l := int64(0), pageDefault
	if since != nil {
		s = int64(*since)
	}
	if limit != nil {
		l = *limit
	}
	return s, l
}

// GetRecoveryShare は GET /api/v1/recovery/{request_id}/share。Shares.ShareFor（承認する端末が自分宛ての片のコピーを取る）。
func (a *Apis) GetRecoveryShare(ctx context.Context, request gen.GetRecoveryShareRequestObject) (gen.GetRecoveryShareResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	rid, err := parseId(request.RequestId, "path.request_id")
	if err != nil {
		return nil, err
	}
	f, err := a.k.Shares.ShareFor(ctx, a.k.d.Pool, rid, device)
	if err != nil {
		return nil, err
	}
	return gen.GetRecoveryShare200JSONResponse(f), nil
}

// PutDeviceInbox は POST /api/v1/devices/{did}/inbox。Inbox.Put（送り手は主体の端末）。
func (a *Apis) PutDeviceInbox(ctx context.Context, request gen.PutDeviceInboxRequestObject) (gen.PutDeviceInboxResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	to, err := parseId(request.Did, "path.did")
	if err != nil {
		return nil, err
	}
	ct, ok1 := decodeB64(request.Body.Ciphertext)
	sig, ok2 := decodeB64(request.Body.Sig)
	if !ok1 || !ok2 {
		return nil, ev(cgen.KEY009)
	}
	var id uuid.UUID
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = a.k.Inbox.Put(ctx, tx, device, to, InboxKind(request.Body.Kind), ct, sig)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.PutDeviceInbox200JSONResponse{Id: id.String()}, nil
}

// GetFamilyRecoveryKeyPublic は GET /api/v1/family/recovery-key/public。FamilyRecoveryKey.Public。
func (a *Apis) GetFamilyRecoveryKeyPublic(ctx context.Context, request gen.GetFamilyRecoveryKeyPublicRequestObject) (gen.GetFamilyRecoveryKeyPublicResponseObject, error) {
	pk, err := a.k.FamilyRecoveryKey.Public(ctx)
	if err != nil {
		return nil, err
	}
	return gen.GetFamilyRecoveryKeyPublic200JSONResponse(pk), nil
}

// UseFamilyRecoveryKey は POST /api/v1/family/recovery-key/use。FamilyRecoveryKey.Use（本文なし。コピー (d) を返す）。
func (a *Apis) UseFamilyRecoveryKey(ctx context.Context, request gen.UseFamilyRecoveryKeyRequestObject) (gen.UseFamilyRecoveryKeyResponseObject, error) {
	_, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	var f gen.Fkb1
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		f, err = a.k.FamilyRecoveryKey.Use(ctx, tx, user, device)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.UseFamilyRecoveryKey200JSONResponse(f), nil
}

// ApproveMyDevice は POST /api/v1/me/devices/{id}/approve。DeviceKeys.Approve（他の端末の承認は再認証。x-reauth conditional）。
// user_sig が解けなければ KEY-003（通らない署名と同じ）。
func (a *Apis) ApproveMyDevice(ctx context.Context, request gen.ApproveMyDeviceRequestObject) (gen.ApproveMyDeviceResponseObject, error) {
	p, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	target, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if target != device {
		if err := a.requireReauth(p); err != nil {
			return nil, err
		}
	}
	sig, ok := decodeB64(request.Body.UserSig)
	if !ok {
		return nil, ev(cgen.KEY003)
	}
	var f gen.Fdev1
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		f, err = a.k.DeviceKeys.Approve(ctx, tx, user, device, target, sig)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.ApproveMyDevice200JSONResponse(f), nil
}

// GetMyKeyPackages は GET /api/v1/me/devices/{id}/keypackages。KeyPackages.Count。
func (a *Apis) GetMyKeyPackages(ctx context.Context, request gen.GetMyKeyPackagesRequestObject) (gen.GetMyKeyPackagesResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	if err := ownDevice(request.Id, device); err != nil {
		return nil, err
	}
	c, err := a.k.KeyPackages.Count(ctx, device)
	if err != nil {
		return nil, err
	}
	return gen.GetMyKeyPackages200JSONResponse{Count: c.Count, LastResort: c.LastResort}, nil
}

// CreateMyKeyPackages は POST /api/v1/me/devices/{id}/keypackages。KeyPackages.Push（要素が解けなければ KEY-009）。
func (a *Apis) CreateMyKeyPackages(ctx context.Context, request gen.CreateMyKeyPackagesRequestObject) (gen.CreateMyKeyPackagesResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	if err := ownDevice(request.Id, device); err != nil {
		return nil, err
	}
	pkgs := make([][]byte, 0, len(request.Body.KeyPackages))
	for _, s := range request.Body.KeyPackages {
		b, ok := decodeB64(s)
		if !ok {
			return nil, ev(cgen.KEY009)
		}
		pkgs = append(pkgs, b)
	}
	var last []byte
	if request.Body.LastResort != nil {
		b, ok := decodeB64(*request.Body.LastResort)
		if !ok {
			return nil, ev(cgen.KEY009)
		}
		last = b
	}
	var n int
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		n, err = a.k.KeyPackages.Push(ctx, tx, device, pkgs, last)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateMyKeyPackages200JSONResponse{Count: n}, nil
}

// PutMyDeviceKeys は PUT /api/v1/me/devices/{id}/keys。DeviceKeys.Put（mls_credential が解けなければ KEY-009）。
func (a *Apis) PutMyDeviceKeys(ctx context.Context, request gen.PutMyDeviceKeysRequestObject) (gen.PutMyDeviceKeysResponseObject, error) {
	_, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	if err := ownDevice(request.Id, device); err != nil {
		return nil, err
	}
	b := request.Body
	cred, ok := decodeB64(b.MlsCredential)
	if !ok {
		return nil, ev(cgen.KEY009)
	}
	var f gen.Fdev1
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		f, err = a.k.DeviceKeys.Put(ctx, tx, user, device, b.SigPub, b.KemPub, cred)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.PutMyDeviceKeys200JSONResponse(f), nil
}

// ListMyInbox は GET /api/v1/me/inbox。Inbox.ListSince（主体の端末宛て）。
func (a *Apis) ListMyInbox(ctx context.Context, request gen.ListMyInboxRequestObject) (gen.ListMyInboxResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	since, limit := orPage(request.Params.Since, request.Params.Limit)
	items, more, err := a.k.Inbox.ListSince(ctx, device, since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]gen.InboxItem, len(items))
	for i, it := range items {
		v := gen.InboxItem{Id: it.Id.String(), Kind: gen.InboxItemKind(it.Kind), FromDevice: it.FromDevice.String(),
			Ciphertext: b64.EncodeToString(it.Ciphertext), Pos: gen.CommonPos1(it.Pos), CreatedAt: it.CreatedAt}
		if it.Sig != nil {
			s := b64.EncodeToString(it.Sig)
			v.Sig = &s
		}
		out[i] = v
	}
	return gen.ListMyInbox200JSONResponse{Items: out, HasMore: more}, nil
}

// DeleteMyInboxItem は DELETE /api/v1/me/inbox/{id}。Inbox.Delete（主体の端末宛ての物だけ）。
func (a *Apis) DeleteMyInboxItem(ctx context.Context, request gen.DeleteMyInboxItemRequestObject) (gen.DeleteMyInboxItemResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if err := a.k.d.inTx(ctx, func(tx *store.Tx) error { return a.k.Inbox.Delete(ctx, tx, device, id) }); err != nil {
		return nil, err
	}
	return gen.DeleteMyInboxItem200JSONResponse{}, nil
}

// ListMyKeyBackup は GET /api/v1/me/keybackup。KeyBackup.ListSince。
func (a *Apis) ListMyKeyBackup(ctx context.Context, request gen.ListMyKeyBackupRequestObject) (gen.ListMyKeyBackupResponseObject, error) {
	_, user, _, err := self(ctx)
	if err != nil {
		return nil, err
	}
	since, limit := orPage(request.Params.Since, request.Params.Limit)
	items, more, err := a.k.KeyBackup.ListSince(ctx, user, since, limit)
	if err != nil {
		return nil, err
	}
	return gen.ListMyKeyBackup200JSONResponse{Items: items, HasMore: more}, nil
}

// PutMyKeyBackup は PUT /api/v1/me/keybackup/{group_id}/{epoch}。KeyBackup.Put（新しく置けば 201、既にあれば 204）、?rewrap=true なら
// 再認証の後に KeyBackup.Replace（201）。group_id が解けなければ KEY-009。
func (a *Apis) PutMyKeyBackup(ctx context.Context, request gen.PutMyKeyBackupRequestObject) (gen.PutMyKeyBackupResponseObject, error) {
	p, user, _, err := self(ctx)
	if err != nil {
		return nil, err
	}
	rewrap := request.Params.Rewrap != nil && *request.Params.Rewrap
	if rewrap {
		if err := a.requireReauth(p); err != nil {
			return nil, err
		}
	}
	gid, ok := decodeB64(request.GroupId)
	if !ok {
		return nil, ev(cgen.KEY009)
	}
	created := true
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		if rewrap {
			return a.k.KeyBackup.Replace(ctx, tx, user, gid, int64(request.Epoch), *request.Body)
		}
		var err error
		created, err = a.k.KeyBackup.Put(ctx, tx, user, gid, int64(request.Epoch), *request.Body)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return gen.PutMyKeyBackup204Response{}, nil
	}
	return gen.PutMyKeyBackup201JSONResponse{}, nil
}

// ListMyKeyBundle は GET /api/v1/me/keybundle。KeyBundles.List。
func (a *Apis) ListMyKeyBundle(ctx context.Context, request gen.ListMyKeyBundleRequestObject) (gen.ListMyKeyBundleResponseObject, error) {
	_, user, _, err := self(ctx)
	if err != nil {
		return nil, err
	}
	items, err := a.k.KeyBundles.List(ctx, user)
	if err != nil {
		return nil, err
	}
	return gen.ListMyKeyBundle200JSONResponse{Items: items}, nil
}

// VerifyRecoveryCode は POST /api/v1/me/keybundle/recovery-code/verify。RecoveryCode.Verify（verifier が解けなければ合わない物として
// 扱う：KEY-012）。
func (a *Apis) VerifyRecoveryCode(ctx context.Context, request gen.VerifyRecoveryCodeRequestObject) (gen.VerifyRecoveryCodeResponseObject, error) {
	_, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	v, _ := decodeB64(request.Body.Verifier)
	var f gen.Fkb1
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		f, err = a.k.RecoveryCode.Verify(ctx, tx, user, device, v)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.VerifyRecoveryCode200JSONResponse(f), nil
}

// DeleteMyKeyBundle は DELETE /api/v1/me/keybundle/{kind}/{target}。再認証（第2章 5.4）。KeyBundles.Delete（byDevice は要求した端末）。
func (a *Apis) DeleteMyKeyBundle(ctx context.Context, request gen.DeleteMyKeyBundleRequestObject) (gen.DeleteMyKeyBundleResponseObject, error) {
	p, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.requireReauth(p); err != nil {
		return nil, err
	}
	if err := a.k.d.inTx(ctx, func(tx *store.Tx) error {
		return a.k.KeyBundles.Delete(ctx, tx, user, device, BundleKind(request.Kind), request.Target)
	}); err != nil {
		return nil, err
	}
	return gen.DeleteMyKeyBundle200JSONResponse{}, nil
}

// GetMyKeyBundle は GET /api/v1/me/keybundle/{kind}/{target}。規則 prf_or_approved（kind prf は未承認の端末にも返し、他は承認済みの
// 端末だけ。違えば KEY-004）。kind prf を未承認の端末へ返す時は回復 (e) の使用で、同じトランザクションで Recovery.NotifyUsed(prf、
// success)（承認済みの端末には書かない。第12章 5.4 (e)）。
func (a *Apis) GetMyKeyBundle(ctx context.Context, request gen.GetMyKeyBundleRequestObject) (gen.GetMyKeyBundleResponseObject, error) {
	p, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	kind := BundleKind(request.Kind)
	if kind != BundlePrf && !p.DeviceApproved {
		return nil, ev(cgen.KEY004)
	}
	var f gen.Fkb1
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		if f, err = a.k.KeyBundles.Get(ctx, tx, user, kind, request.Target); err != nil {
			return err
		}
		if kind == BundlePrf && !p.DeviceApproved {
			return a.k.Recovery.NotifyUsed(ctx, tx, user, device, MethodPrf, ResultSuccess)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return gen.GetMyKeyBundle200JSONResponse(f), nil
}

// PutMyKeyBundle は PUT /api/v1/me/keybundle/{kind}/{target}。KeyBundles.Put（kind recovery-code の置き換えは再認証）。
func (a *Apis) PutMyKeyBundle(ctx context.Context, request gen.PutMyKeyBundleRequestObject) (gen.PutMyKeyBundleResponseObject, error) {
	p, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	kind := BundleKind(request.Kind)
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		if kind == BundleRecoveryCode {
			_, err := a.k.KeyBundles.Get(ctx, tx, user, kind, request.Target)
			if err == nil {
				if err := a.requireReauth(p); err != nil {
					return err
				}
			} else if !isEvent(err, cgen.KEY005) {
				return err
			}
		}
		return a.k.KeyBundles.Put(ctx, tx, user, device, kind, request.Target, *request.Body)
	})
	if err != nil {
		return nil, err
	}
	return gen.PutMyKeyBundle200JSONResponse{}, nil
}

// RejoinMyMls は POST /api/v1/me/mls/rejoin。Groups.RequestAdd（入口 rejoin。本人の承認済みか休眠の端末）。
func (a *Apis) RejoinMyMls(ctx context.Context, request gen.RejoinMyMlsRequestObject) (gen.RejoinMyMlsResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	var n int
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		n, err = a.k.Groups.RequestAdd(ctx, tx, device)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.RejoinMyMls200JSONResponse{Rooms: n}, nil
}

// PutMyRecoveryCode は PUT /api/v1/me/recovery-code。RecoveryCode.PutVerifier（規則 first_or_approved は PutVerifier が判定、置き換えは
// 再認証）。verifier と salt が解けなければ NET-001。
func (a *Apis) PutMyRecoveryCode(ctx context.Context, request gen.PutMyRecoveryCodeRequestObject) (gen.PutMyRecoveryCodeResponseObject, error) {
	p, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	b := request.Body
	v, ok := decodeB64(b.Verifier)
	if !ok {
		return nil, errNet001("body/verifier", "base64url でない")
	}
	salt, ok := decodeB64(b.Params.Salt)
	if !ok {
		return nil, errNet001("body/params/salt", "base64url でない")
	}
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		_, exists, err := readRecoveryParams(ctx, tx, user)
		if err != nil {
			return err
		}
		if exists {
			if err := a.requireReauth(p); err != nil {
				return err
			}
		}
		return a.k.RecoveryCode.PutVerifier(ctx, tx, user, device, v, RecoveryParams{M: b.Params.M, T: b.Params.T, P: b.Params.P, Salt: salt})
	})
	if err != nil {
		return nil, err
	}
	return gen.PutMyRecoveryCode200JSONResponse{}, nil
}

// RequestRecovery は POST /api/v1/me/recovery/request。Shares.Request。
func (a *Apis) RequestRecovery(ctx context.Context, request gen.RequestRecoveryRequestObject) (gen.RequestRecoveryResponseObject, error) {
	_, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	var r RecoveryRequest
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		r, err = a.k.Shares.Request(ctx, tx, device)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.RequestRecovery200JSONResponse(r), nil
}

// PutMySigningKey は PUT /api/v1/me/signing-key。UserSigningKeys.Put（規則 first_or_approved は Put が判定、置き換えは再認証）。
// user_sig が解けなければ KEY-003（通らない署名と同じ）。
func (a *Apis) PutMySigningKey(ctx context.Context, request gen.PutMySigningKeyRequestObject) (gen.PutMySigningKeyResponseObject, error) {
	p, user, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	b := request.Body
	sigs := make([]DeviceSig, 0, len(b.DeviceSigs))
	for i, ds := range b.DeviceSigs {
		id, err := parseId(ds.DeviceId, "body/device_sigs/"+strconv.Itoa(i)+"/device_id")
		if err != nil {
			return nil, err
		}
		s, ok := decodeB64(ds.UserSig)
		if !ok {
			return nil, ev(cgen.KEY003)
		}
		sigs = append(sigs, DeviceSig{DeviceId: id, UserSig: s})
	}
	var f gen.Fusr1
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		v, err := a.k.UserSigningKeys.Version(ctx, tx, user)
		if err != nil {
			return err
		}
		if v > 0 {
			if err := a.requireReauth(p); err != nil {
				return err
			}
		}
		f, err = a.k.UserSigningKeys.Put(ctx, tx, user, device, b.UserSigPub, sigs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.PutMySigningKey200JSONResponse(f), nil
}

// ApproveRecovery は POST /api/v1/recovery/{request_id}/approve。再認証（x-reauth true）。Shares.Approve（規則 share_holder は Approve が
// 判定）。ciphertext と sig が解けなければ KEY-009。
func (a *Apis) ApproveRecovery(ctx context.Context, request gen.ApproveRecoveryRequestObject) (gen.ApproveRecoveryResponseObject, error) {
	p, _, device, err := self(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.requireReauth(p); err != nil {
		return nil, err
	}
	rid, err := parseId(request.RequestId, "path.request_id")
	if err != nil {
		return nil, err
	}
	ct, ok1 := decodeB64(request.Body.Ciphertext)
	sig, ok2 := decodeB64(request.Body.Sig)
	if !ok1 || !ok2 {
		return nil, ev(cgen.KEY009)
	}
	if err := a.k.d.inTx(ctx, func(tx *store.Tx) error { return a.k.Shares.Approve(ctx, tx, device, rid, ct, sig) }); err != nil {
		return nil, err
	}
	return gen.ApproveRecovery200JSONResponse{}, nil
}

// ClaimKeyPackage は POST /api/v1/users/{uid}/devices/{did}/keypackage。KeyPackages.Take(uid, did)（最後の手段なら KEY-006 の警告を添える）。
func (a *Apis) ClaimKeyPackage(ctx context.Context, request gen.ClaimKeyPackageRequestObject) (gen.ClaimKeyPackageResponseObject, error) {
	if _, _, _, err := self(ctx); err != nil {
		return nil, err
	}
	uid, err := parseId(request.Uid, "path.uid")
	if err != nil {
		return nil, err
	}
	did, err := parseId(request.Did, "path.did")
	if err != nil {
		return nil, err
	}
	var t Taken
	err = a.k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		t, err = a.k.KeyPackages.Take(ctx, tx, uid, did)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := gen.ClaimKeyPackage200JSONResponse{KeyPackage: b64.EncodeToString(t.Package)}
	if t.LastResort {
		out.Warnings = &[]struct {
			Code interface{} `json:"code"`
		}{{Code: cgen.KEY006}}
	}
	return out, nil
}
