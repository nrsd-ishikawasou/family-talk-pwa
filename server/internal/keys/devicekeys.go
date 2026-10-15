// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// DeviceKeys は端末の鍵の登録と承認（fdev/1）を持つ（B-081。第12章 5.3、5.5、第3章 4.4）。端末の承認の有無の正は
// srv-identity の devices.state で、本ブロックは持たない。
type DeviceKeys struct {
	d       *Deps
	signing *UserSigningKeys
	groups  *Groups
}

// Put は PUT /me/devices/{id}/keys。fdev/1 を未承認で登録する（既にあれば置き換えず同じ物を返す）。credential が第12章 5.3 の
// 35 バイト（basic、identity は userId と deviceId の 16 バイトずつ）と一致しない、または JWK が P-256 の点でなければ KEY-009。
// 端末が無ければ KEY-001。
func (k *DeviceKeys) Put(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, sigPub, kemPub gen.CommonJwkP2561, credential []byte) (gen.Fdev1, error) {
	panic("未実装")
}

// Approve は POST /me/devices/{id}/approve。他の端末の承認は本人の承認済みの端末だけ（KEY-003。再認証は呼ぶ側）、自己承認は
// 未承認でも可。userSig を本人の署名鍵の公開部で第12章 5.3 の対象に対して確かめ（無いか通らなければ KEY-003）、子ども区分で
// 保護者の承認が有効でまだ保護者が通していない端末なら、別のトランザクションで srv-identity Guardians.RequestApproval を確定して
// から MEM-006。user_sig と approved_at を置き、同じトランザクションで srv-identity Devices.Approve と Groups.RequestAdd。
func (k *DeviceKeys) Approve(ctx context.Context, tx *store.Tx, byDevice, deviceId uuid.UUID, userSig []byte) (gen.Fdev1, error) {
	panic("未実装")
}

// Get は fdev/1。無ければ nil。q は ctx のトランザクションか Pool。
func (k *DeviceKeys) Get(ctx context.Context, q store.Querier, deviceId uuid.UUID) (*gen.Fdev1, error) {
	panic("未実装")
}

// ListOfUser は構成員の端末の fdev/1。approvedOnly なら承認済みと休眠の端末だけ（DeviceKeyReader.DevicesOf）、偽なら登録の
// ある全端末（未承認を含む。DeviceKeyReader.KeysOf）。
func (k *DeviceKeys) ListOfUser(ctx context.Context, userId uuid.UUID, approvedOnly bool) ([]gen.Fdev1, error) {
	panic("未実装")
}

// VerifyProof は fdev/1 の署名鍵の公開部で ECDSA P-256（SHA-256、r と s の 64 バイト）を確かめる（DeviceKeyReader）。
// 公開鍵が無ければ偽。DB の読みの失敗は error。
func (k *DeviceKeys) VerifyProof(ctx context.Context, deviceId uuid.UUID, message, sig []byte) (bool, error) {
	panic("未実装")
}

// VerifyUserSig は第12章 5.3 の user_sig の対象（ft.device_cert/1、0、user_id、device_id、sig_pub と kem_pub の SEC 1 の 65 バイト）を
// 組み立てて fusr の公開部で確かめる（Approve と UserSigningKeys.Put が使う）。対象の欄は全部 fdev にあり、記録は読まない。
// fdev の user_sig が無いか、鍵が P-256 の点でなければ偽。
func (k *DeviceKeys) VerifyUserSig(fdev gen.Fdev1, fusr gen.Fusr1) bool {
	panic("未実装")
}

// Revoke は端末の鍵の行を消す（hook OnDeviceRevoked から。無ければ何もしない）。
func (k *DeviceKeys) Revoke(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	panic("未実装")
}
