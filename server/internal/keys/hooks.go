// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Hooks は srv-identity の hook に登録する本ブロックの処理（B-087。Keys.Register が登録する）。どれも呼び出しと同じトランザクションで、
// 錠の順（ブロックのページ「錠の順」）に従う。鍵や leaf の無い端末・構成員では何もしない。
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
// 断った時に呼ばれる。DeviceKeys.Revoke、KeyPackages.DeleteAll、KeyBundles.DeleteOfDevice、Groups.RemoveLeafsOfDevice、
// Inbox.DeleteAllOfDevice の順。
func (h *Hooks) OnDeviceRevoked(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	panic("未実装")
}

// OnUserSuspended は停止で本人の端末の追加待ちを消す（端末は未承認に戻り Add できないため。再開の後は自己承認が RequestAdd で立て直す）。
func (h *Hooks) OnUserSuspended(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}

// OnUserErased は抹消で KeyBundles.DeleteAllOfUser、回復コードの照合の値、open の回復の求めを expired に、KeyBackup.DeleteAllOfUser、
// 本人の署名鍵の失効。srv-identity は先に本人の全端末を取り消す（OnDeviceRevoked が端末ごとに走る）。
func (h *Hooks) OnUserErased(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}

// OnUserDeleted は削除で本人の全端末について Groups.RemoveLeafsOfDevice と KeyBundles.DeleteOfDevice（本人の端末へ包まれた他人の片）。
// 本人のコピーとバックアップは残す（第3章 8.2）。eraseContent は本ブロックでは使わない。
func (h *Hooks) OnUserDeleted(ctx context.Context, tx *store.Tx, userId uuid.UUID, eraseContent bool) error {
	panic("未実装")
}

// OnPasskeyDeleted はパスキーを消した時にそのコピー (e) を消す（KeyBundles.remove。無くても誤りにしない。第12章 5.4 (e)）。
func (h *Hooks) OnPasskeyDeleted(ctx context.Context, tx *store.Tx, userId uuid.UUID, credId []byte) error {
	panic("未実装")
}

// OnPasskeyReset は管理者の再設定のリンクの使用で Recovery.NotifyUsed(reset、success)（第3章 4.6）。
func (h *Hooks) OnPasskeyReset(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) error {
	panic("未実装")
}
