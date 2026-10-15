// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
)

// Apis は生成のタグ keys の 22 操作の実装（gen.Implementations の Keys の欄。ブロックのページの受ける節）。
// 各操作は主体を boundary.PrincipalFrom(ctx) で取り、書き込みは store.TxFrom(ctx) のトランザクション（無ければ開く）で行う。
// *fault.Event はそのまま error で返す（srv-boundary の strict の口が problem にする）。x-reauth の 5 分と x-authz-rule は本ブロックが判定する。
// /rooms/{id}/mls/* の 5 操作はタグ rooms で srv-rooms が実装し、Groups を呼ぶ。
type Apis struct {
	k *Keys
}

var _ gen.KeysApi = (*Apis)(nil)

// GetRecoveryShare は GET /api/v1/recovery/{request_id}/share。Shares.ShareFor（承認する端末が自分宛ての片のコピーを取る）。
func (a *Apis) GetRecoveryShare(ctx context.Context, request gen.GetRecoveryShareRequestObject) (gen.GetRecoveryShareResponseObject, error) {
	panic("未実装")
}

// PutDeviceInbox は POST /api/v1/devices/{did}/inbox。Inbox.Put。
func (a *Apis) PutDeviceInbox(ctx context.Context, request gen.PutDeviceInboxRequestObject) (gen.PutDeviceInboxResponseObject, error) {
	panic("未実装")
}

// GetFamilyRecoveryKeyPublic は GET /api/v1/family/recovery-key/public。FamilyRecoveryKey.Public。
func (a *Apis) GetFamilyRecoveryKeyPublic(ctx context.Context, request gen.GetFamilyRecoveryKeyPublicRequestObject) (gen.GetFamilyRecoveryKeyPublicResponseObject, error) {
	panic("未実装")
}

// UseFamilyRecoveryKey は POST /api/v1/family/recovery-key/use。FamilyRecoveryKey.Use（本文なし。コピー (d) を返す）。
func (a *Apis) UseFamilyRecoveryKey(ctx context.Context, request gen.UseFamilyRecoveryKeyRequestObject) (gen.UseFamilyRecoveryKeyResponseObject, error) {
	panic("未実装")
}

// ApproveMyDevice は POST /api/v1/me/devices/{id}/approve。DeviceKeys.Approve（他の端末の承認は再認証。x-reauth conditional）。
func (a *Apis) ApproveMyDevice(ctx context.Context, request gen.ApproveMyDeviceRequestObject) (gen.ApproveMyDeviceResponseObject, error) {
	panic("未実装")
}

// GetMyKeyPackages は GET /api/v1/me/devices/{id}/keypackages。KeyPackages.Count。
func (a *Apis) GetMyKeyPackages(ctx context.Context, request gen.GetMyKeyPackagesRequestObject) (gen.GetMyKeyPackagesResponseObject, error) {
	panic("未実装")
}

// CreateMyKeyPackages は POST /api/v1/me/devices/{id}/keypackages。KeyPackages.Push。
func (a *Apis) CreateMyKeyPackages(ctx context.Context, request gen.CreateMyKeyPackagesRequestObject) (gen.CreateMyKeyPackagesResponseObject, error) {
	panic("未実装")
}

// PutMyDeviceKeys は PUT /api/v1/me/devices/{id}/keys。DeviceKeys.Put。
func (a *Apis) PutMyDeviceKeys(ctx context.Context, request gen.PutMyDeviceKeysRequestObject) (gen.PutMyDeviceKeysResponseObject, error) {
	panic("未実装")
}

// ListMyInbox は GET /api/v1/me/inbox。Inbox.ListSince。
func (a *Apis) ListMyInbox(ctx context.Context, request gen.ListMyInboxRequestObject) (gen.ListMyInboxResponseObject, error) {
	panic("未実装")
}

// DeleteMyInboxItem は DELETE /api/v1/me/inbox/{id}。Inbox.Delete。
func (a *Apis) DeleteMyInboxItem(ctx context.Context, request gen.DeleteMyInboxItemRequestObject) (gen.DeleteMyInboxItemResponseObject, error) {
	panic("未実装")
}

// ListMyKeyBackup は GET /api/v1/me/keybackup。KeyBackup.ListSince。
func (a *Apis) ListMyKeyBackup(ctx context.Context, request gen.ListMyKeyBackupRequestObject) (gen.ListMyKeyBackupResponseObject, error) {
	panic("未実装")
}

// PutMyKeyBackup は PUT /api/v1/me/keybackup/{group_id}/{epoch}。KeyBackup.Put、?rewrap=true なら KeyBackup.Replace（再認証）。
func (a *Apis) PutMyKeyBackup(ctx context.Context, request gen.PutMyKeyBackupRequestObject) (gen.PutMyKeyBackupResponseObject, error) {
	panic("未実装")
}

// ListMyKeyBundle は GET /api/v1/me/keybundle。KeyBundles.List。
func (a *Apis) ListMyKeyBundle(ctx context.Context, request gen.ListMyKeyBundleRequestObject) (gen.ListMyKeyBundleResponseObject, error) {
	panic("未実装")
}

// VerifyRecoveryCode は POST /api/v1/me/keybundle/recovery-code/verify。RecoveryCode.Verify。
func (a *Apis) VerifyRecoveryCode(ctx context.Context, request gen.VerifyRecoveryCodeRequestObject) (gen.VerifyRecoveryCodeResponseObject, error) {
	panic("未実装")
}

// DeleteMyKeyBundle は DELETE /api/v1/me/keybundle/{kind}/{target}。KeyBundles.Delete（byDevice は要求した端末。再認証）。
func (a *Apis) DeleteMyKeyBundle(ctx context.Context, request gen.DeleteMyKeyBundleRequestObject) (gen.DeleteMyKeyBundleResponseObject, error) {
	panic("未実装")
}

// GetMyKeyBundle は GET /api/v1/me/keybundle/{kind}/{target}。KeyBundles.Get（規則 prf_or_approved は本操作が端末の状態を読んで判定）。
// kind prf を未承認の端末へ返す時は回復 (e) の使用で、同じトランザクションで Recovery.NotifyUsed(prf、success)（承認済みの端末には
// 書かない。第12章 5.4 (e)）。
func (a *Apis) GetMyKeyBundle(ctx context.Context, request gen.GetMyKeyBundleRequestObject) (gen.GetMyKeyBundleResponseObject, error) {
	panic("未実装")
}

// PutMyKeyBundle は PUT /api/v1/me/keybundle/{kind}/{target}。KeyBundles.Put（kind recovery-code の置き換えは再認証）。
func (a *Apis) PutMyKeyBundle(ctx context.Context, request gen.PutMyKeyBundleRequestObject) (gen.PutMyKeyBundleResponseObject, error) {
	panic("未実装")
}

// RejoinMyMls は POST /api/v1/me/mls/rejoin。Groups.RequestAdd（入口 rejoin。本人の承認済みか休眠の端末）。
func (a *Apis) RejoinMyMls(ctx context.Context, request gen.RejoinMyMlsRequestObject) (gen.RejoinMyMlsResponseObject, error) {
	panic("未実装")
}

// PutMyRecoveryCode は PUT /api/v1/me/recovery-code。RecoveryCode.PutVerifier（置き換えは再認証）。
func (a *Apis) PutMyRecoveryCode(ctx context.Context, request gen.PutMyRecoveryCodeRequestObject) (gen.PutMyRecoveryCodeResponseObject, error) {
	panic("未実装")
}

// RequestRecovery は POST /api/v1/me/recovery/request。Shares.Request。
func (a *Apis) RequestRecovery(ctx context.Context, request gen.RequestRecoveryRequestObject) (gen.RequestRecoveryResponseObject, error) {
	panic("未実装")
}

// PutMySigningKey は PUT /api/v1/me/signing-key。UserSigningKeys.Put（置き換えは再認証）。
func (a *Apis) PutMySigningKey(ctx context.Context, request gen.PutMySigningKeyRequestObject) (gen.PutMySigningKeyResponseObject, error) {
	panic("未実装")
}

// ApproveRecovery は POST /api/v1/recovery/{request_id}/approve。Shares.Approve（規則 share_holder は本操作が判定）。
func (a *Apis) ApproveRecovery(ctx context.Context, request gen.ApproveRecoveryRequestObject) (gen.ApproveRecoveryResponseObject, error) {
	panic("未実装")
}

// ClaimKeyPackage は POST /api/v1/users/{uid}/devices/{did}/keypackage。KeyPackages.Take(uid, did)（最後の手段なら KEY-006 の警告を添える）。
func (a *Apis) ClaimKeyPackage(ctx context.Context, request gen.ClaimKeyPackageRequestObject) (gen.ClaimKeyPackageResponseObject, error) {
	panic("未実装")
}
