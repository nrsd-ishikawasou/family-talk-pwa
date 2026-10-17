// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"
	"errors"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// RecoveryMethod は回復に使った手段（recovery.used の method。第2章 6.2）。
type RecoveryMethod string

// 本ブロックが書く手段の 5 つ。
const (
	MethodShare        RecoveryMethod = "share"
	MethodRecoveryCode RecoveryMethod = "recovery-code"
	MethodFamily       RecoveryMethod = "family"
	MethodPrf          RecoveryMethod = "prf"
	MethodReset        RecoveryMethod = "reset"
)

// RecoveryResult は回復の結果（recovery.used の result）。
type RecoveryResult string

// 結果の 2 つ。
const (
	ResultSuccess RecoveryResult = "success"
	ResultFailure RecoveryResult = "failure"
)

// Recovery は回復の順と使用の記録を持つ（B-083。第12章 5.4）。
type Recovery struct {
	d *Deps
}

// Order は画面の回復の順（1 既存の端末、(e) PRF、2 家族、3 回復コード、4 家族回復鍵。第12章 5.4）。
func (r *Recovery) Order() []string {
	return []string{"device", string(MethodPrf), string(MethodShare), string(MethodRecoveryCode), string(MethodFamily)}
}

// NotifyUsed は recovery.used を feed に書く（本ブロックだけが書く）。宛先は本人と管理者（family は家族の全員）。同じトランザクションで
// srv-identity Hooks.Notify に知らせを渡す（family は family_key_used を全員へ、他は recovery_used を本人と管理者へ。第7章 9）。
// 書き手は share が Shares.Approve、recovery-code が RecoveryCode.Verify、family が FamilyRecoveryKey.Use、prf が Apis.GetMyKeyBundle、
// reset が hook OnPasskeyReset。既存の端末による承認は端末の追加であって回復ではなく、書かない（第3章 4.4、第12章 5.4）。
// method と result が列挙の外ならプログラムの誤り（Fault）。
func (r *Recovery) NotifyUsed(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, method RecoveryMethod, result RecoveryResult) error {
	switch method {
	case MethodShare, MethodRecoveryCode, MethodFamily, MethodPrf, MethodReset:
	default:
		return fault.Wrap(errors.New("keys: 回復の手段が列挙の外"))
	}
	if result != ResultSuccess && result != ResultFailure {
		return fault.Wrap(errors.New("keys: 回復の結果が列挙の外"))
	}
	ids := r.d.Identity.Users
	var to []uuid.UUID
	var err error
	if method == MethodFamily {
		to, err = ids.FamilyIds(ctx, tx)
	} else {
		to, err = ids.AdminIds(ctx, tx)
		to = append(to, userId)
	}
	if err != nil {
		return err
	}
	var dev *uuid.UUID
	if deviceId != uuid.Nil {
		d := deviceId
		dev = &d
	}
	if err := r.d.emit(tx, to, "recovery.used", uuid.Nil, uuid.Nil, recoveryUsed{UserId: userId, Method: method, DeviceId: dev, Result: result}); err != nil {
		return err
	}
	usr, err := ids.Get(ctx, tx, userId)
	if err != nil {
		return err
	}
	// 事象の鍵（第7章 9 の表の欄）。端末の無い回復（deviceId が Nil）は端末の欄を空にし、欄の数は変えない。
	devKey := ""
	if dev != nil {
		devKey = dev.String()
	}
	if method == MethodFamily {
		return r.d.Identity.Hooks.Notify(ctx, tx, identity.Notice{Kind: noticeFamilyKeyUsed, Key: userId.String() + ":" + devKey,
			Audience: identity.Audience{Kind: identity.AudienceAll},
			Params:   map[string]any{"user_id": userId.String(), "name": usr.DisplayName, "result": string(result)}})
	}
	params := map[string]any{"user_id": userId.String(), "name": usr.DisplayName, "method": string(method), "result": string(result)}
	key := userId.String() + ":" + devKey + ":" + string(method) + ":" + string(result)
	self := userId
	for _, a := range []identity.Audience{{Kind: identity.AudienceSelf, UserId: &self}, {Kind: identity.AudienceAdmins}} {
		if err := r.d.Identity.Hooks.Notify(ctx, tx, identity.Notice{Kind: noticeRecoveryUsed, Key: key, Audience: a, Params: params}); err != nil {
			return err
		}
	}
	return nil
}
