// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"strings"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 本ブロックが hook OnNotice で出す知らせの種類（第7章 9 の表の行。level と text_key は srv-push が種類から決める）。
const (
	// noticeCardHolderChanged はカードの持ち主の停止・削除・抹消（管理者へ。作り直しの案内。第12章 5.4）。
	noticeCardHolderChanged = "card_holder_changed"
	// noticeUpdateFailed は端末の更新の失敗の報告（管理者へ。POST /me/report。第9章 10）。
	noticeUpdateFailed = "update_failed"
	// noticeChildRelaxed16 と noticeChildReleased18 は 16 歳の緩和と 18 歳の解除（本人と保護者へ。第3章 9.1）。
	noticeChildRelaxed16  = "child_relaxed_16"
	noticeChildReleased18 = "child_released_18"
	// noticeChildFlagSuggested は区分の無い構成員が 18 歳未満になる誕生日を書いた時（管理者へ。第3章 9.1）。
	noticeChildFlagSuggested = "child_flag_suggested"
	// noticeTransferRequested は管理者の引き継ぎの求め（指名された人へ。第3章 8.4）。
	noticeTransferRequested = "transfer_requested"
	// noticeEraseApprovalRequested と noticeDeviceApprovalRequested は保護者の承認の求め（抹消・端末の追加。保護者へ。第3章 8.3、9.2）。
	noticeEraseApprovalRequested  = "erase_approval_requested"
	noticeDeviceApprovalRequested = "device_approval_requested"
	// noticeEraseApprovalExpired は抹消の承認の求めの 7 日の失効（子と保護者へ。第3章 8.3）。
	noticeEraseApprovalExpired = "erase_approval_expired"
	// noticeChildFlagSet と noticeChildFlagCleared は管理者による子ども区分の付け外し（本人と保護者へ。第3章 9.1）。
	noticeChildFlagSet     = "child_flag_set"
	noticeChildFlagCleared = "child_flag_cleared"
	// noticeGuardianSettings は保護者の設定の変更（子と他の保護者へ。第2章 5.3）。
	noticeGuardianSettings = "guardian_settings_changed"
	// noticeGuardiansChanged は保護者の変更（子と新旧の保護者へ。第3章 9.2）。
	noticeGuardiansChanged = "guardians_changed"
	// noticeEraseRequested と noticeEraseCancelled は抹消の猶予の始まりと取り消し（本人の全端末へ。第3章 8.3）。
	noticeEraseRequested = "erase_requested"
	noticeEraseCancelled = "erase_cancelled"
	// noticeNewDevice は新しい端末・ログイン（本人へ。子ども区分なら保護者にも。第3章 5.2、9.2）。
	noticeNewDevice = "new_device"
)

// noticeKey は知らせの事象の鍵（第7章 9 の表の「事象の鍵」の欄の値を `:` で連ねる。srv-push は (kind, key, 日) で重複を抑える）。
func noticeKey(parts ...string) string {
	return strings.Join(parts, ":")
}

// notifySelf は本人への知らせ。key は事象の鍵（noticeKey）。
func (h *Hooks) notifySelf(ctx context.Context, tx *store.Tx, userId uuid.UUID, kind, key string, params map[string]any) error {
	u := userId
	return h.onNotice(ctx, tx, Notice{Kind: kind, Key: key, Params: params, Audience: Audience{Kind: AudienceSelf, UserId: &u}})
}

// notifyAdmins は管理者への知らせ。
func (h *Hooks) notifyAdmins(ctx context.Context, tx *store.Tx, kind, key string, params map[string]any) error {
	return h.onNotice(ctx, tx, Notice{Kind: kind, Key: key, Params: params, Audience: Audience{Kind: AudienceAdmins}})
}

// notifyGuardians は子の保護者への知らせ。
func (h *Hooks) notifyGuardians(ctx context.Context, tx *store.Tx, childId uuid.UUID, kind, key string, params map[string]any) error {
	c := childId
	return h.onNotice(ctx, tx, Notice{Kind: kind, Key: key, Params: params, Audience: Audience{Kind: AudienceGuardians, UserId: &c}})
}

// notifyEach は構成員ごとに本人宛ての知らせを出す（宛先が保護者の一部や外れた保護者を含む時。同じ人には 1 回）。
func (h *Hooks) notifyEach(ctx context.Context, tx *store.Tx, to []uuid.UUID, kind, key string, params map[string]any) error {
	seen := map[uuid.UUID]bool{}
	for _, u := range to {
		if seen[u] {
			continue
		}
		seen[u] = true
		if err := h.notifySelf(ctx, tx, u, kind, key, params); err != nil {
			return err
		}
	}
	return nil
}
