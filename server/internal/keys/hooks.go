// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Hooks は srv-identity の hook に登録する本ブロックの処理（B-087。Keys.Register が登録する）。どれも呼び出しと同じトランザクションで、
// 錠の順（ブロックのページ「錠の順」）に従う。鍵や leaf の無い端末・構成員では何もしない。本ブロックの hook が取る RoomLock
// （RemoveLeafsOfDevice、removeLeafsOfDevices、dropPendingAdds）は、srv-identity が先に呼ぶ hook OnBeforeUserChange・
// OnBeforeDeviceRevoke で srv-rooms が和として識別子の順に取り済みで、待たない（全体実装設計書 2.4、I-18）。hook の中の順は、単独で
// 呼ばれる経路（休眠）のために残す。
type Hooks struct {
	d        *Deps
	devices  *DeviceKeys
	packages *KeyPackages
	bundles  *KeyBundles
	groups   *Groups
	inbox    *Inbox
	codes    *RecoveryCode
	shares   *Shares
	backup   *KeyBackup
	signing  *UserSigningKeys
	recovery *Recovery
}

// OnDeviceRevoked は端末の取り消し、未承認の 7 日の期限切れ、抹消（端末を 1 台ずつ取り消す）、保護者が子の端末の鍵の承認を
// 断った時に呼ばれる。DeviceKeys.Revoke、KeyPackages.DeleteAll、KeyBundles.DeleteOfDevice、その端末の open の回復の求めを expired に、
// Groups.RemoveLeafsOfDevice、Inbox.DeleteAllOfDevice の順（錠の順）。
func (h *Hooks) OnDeviceRevoked(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	if err := h.devices.Revoke(ctx, tx, deviceId); err != nil {
		return err
	}
	if err := h.packages.DeleteAll(ctx, tx, deviceId); err != nil {
		return err
	}
	if err := h.bundles.DeleteOfDevice(ctx, tx, deviceId); err != nil {
		return err
	}
	if err := h.shares.expireOfDevice(ctx, tx, deviceId); err != nil {
		return err
	}
	if err := h.groups.RemoveLeafsOfDevice(ctx, tx, deviceId); err != nil {
		return err
	}
	return h.inbox.DeleteAllOfDevice(ctx, tx, deviceId)
}

// OnUserSuspended は停止で本人の端末の追加待ちを消す（端末は未承認に戻り Add できないため。再開の後は自己承認が RequestAdd で立て直す）。
// reason（停止か削除）は見ない。
func (h *Hooks) OnUserSuspended(ctx context.Context, tx *store.Tx, userId uuid.UUID, _ identity.RevokeReason) error {
	return h.groups.dropPendingAdds(ctx, tx, userId)
}

// OnUserErased は抹消で KeyBundles.DeleteAllOfUser、回復コードの照合の値、open の回復の求めを expired に、KeyBackup.DeleteAllOfUser、
// 本人の署名鍵の失効。srv-identity は先に本人の全端末を取り消す（OnDeviceRevoked が端末ごとに走る）。
func (h *Hooks) OnUserErased(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	if err := h.bundles.DeleteAllOfUser(ctx, tx, userId); err != nil {
		return err
	}
	if err := h.codes.deleteOfUser(ctx, tx, userId); err != nil {
		return err
	}
	if err := h.shares.expireOfUser(ctx, tx, userId); err != nil {
		return err
	}
	if err := h.backup.DeleteAllOfUser(ctx, tx, userId); err != nil {
		return err
	}
	return h.signing.revoke(ctx, tx, userId)
}

// OnUserDeleted は削除で本人の全端末について KeyBundles.DeleteOfDevice（本人の端末へ包まれた他人の片。識別子の順）と、全端末をまとめて
// Groups の削除待ち（トークの錠をトークの識別子の順に取る）。本人のコピーとバックアップは残す（第3章 8.2）。eraseContent は本ブロックでは
// 使わない。
func (h *Hooks) OnUserDeleted(ctx context.Context, tx *store.Tx, userId uuid.UUID, eraseContent bool) error {
	devs, err := h.d.Identity.Devices.OfUser(ctx, tx, userId)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(devs))
	for i, dv := range devs {
		ids[i] = dv.Id
	}
	ids = sortedIds(ids)
	for _, id := range ids {
		if err := h.bundles.DeleteOfDevice(ctx, tx, id); err != nil {
			return err
		}
	}
	return h.groups.removeLeafsOfDevices(ctx, tx, ids)
}

// OnPasskeyDeleted はパスキーを消した時にそのコピー (e) を消す（KeyBundles.remove。無くても誤りにしない。第12章 5.4 (e)）。
func (h *Hooks) OnPasskeyDeleted(ctx context.Context, tx *store.Tx, userId uuid.UUID, credId []byte) error {
	_, err := h.bundles.remove(ctx, tx, userId, BundlePrf, b64.EncodeToString(credId))
	return err
}

// OnPasskeyReset は管理者の再設定のリンクの使用で Recovery.NotifyUsed(reset、success)（第3章 4.6）。
func (h *Hooks) OnPasskeyReset(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) error {
	return h.recovery.NotifyUsed(ctx, tx, userId, deviceId, MethodReset, ResultSuccess)
}
