// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// DeviceSig は S の交代で端末ごとに付け直す端末の証（PUT /me/signing-key の device_sigs の 1 つ。第12章 5.4）。
type DeviceSig struct {
	DeviceId uuid.UUID
	// UserSig は r と s の 64 バイト（base64url を解いた後。第12章 5.3）。
	UserSig []byte
}

// UserSigningKeys は本人の署名鍵の公開部（fusr/1）と S の版を持つ（B-080。第12章 5.2、5.4、12）。
type UserSigningKeys struct {
	d       *Deps
	devices *DeviceKeys
	bundles *KeyBundles
}

// Put は PUT /me/signing-key。初回（行が無い）は版 1 で置く（deviceSigs は空でよい。自己承認は /approve）。置き換え（S の交代）は
// byDevice が承認済みの端末で、承認済みと休眠の全端末の user_sig が deviceSigs に揃い、全部が新しい公開部で第12章 5.3 の対象に
// 対して通る時だけ（欠けるか通らなければ KEY-003、知らない端末は KEY-001、承認済みでない byDevice は KEY-004、JWK が P-256 の点で
// なければ KEY-009）。置き換えで版を 1 増やし、古い版のコピーを消し、家族へ user.changed（signing_key）、本人へ keybundle.changed。
func (s *UserSigningKeys) Put(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, pub gen.CommonJwkP2561, deviceSigs []DeviceSig) (gen.Fusr1, error) {
	panic("未実装")
}

// Get は fusr/1（版を含む）。無ければ nil。DeviceKeyReader.SigningKeys の元。
func (s *UserSigningKeys) Get(ctx context.Context, userId uuid.UUID) (*gen.Fusr1, error) {
	panic("未実装")
}

// Version は今の S の版。行が無ければ 0。KeyBundles.Put と KeyBackup.Put が key_version と比べる（KEY-011）。
func (s *UserSigningKeys) Version(ctx context.Context, q store.Querier, userId uuid.UUID) (int, error) {
	panic("未実装")
}

// revoke は抹消で本人の署名鍵を失効させる（revoked_at。OnUserErased）。
func (s *UserSigningKeys) revoke(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}
