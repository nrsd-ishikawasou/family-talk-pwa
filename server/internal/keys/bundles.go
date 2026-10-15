// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// BundleKind は鍵バンドルのコピーの系統（fkb/1 の kind。第12章 5.4、12）。
type BundleKind string

// 系統の 5 つ。(a) device、(b) recovery-code、(c) share、(d) family、(e) prf。
const (
	BundleDevice       BundleKind = "device"
	BundleRecoveryCode BundleKind = "recovery-code"
	BundleShare        BundleKind = "share"
	BundleFamily       BundleKind = "family"
	BundlePrf          BundleKind = "prf"
)

// targetSelf は本人に 1 つの系統（recovery-code と family）の target_id（第12章 12）。
const targetSelf = "self"

// BundleMeta はコピーの一覧の 1 行（GET /me/keybundle。中身は返さない）。
type BundleMeta = gen.KeyBundleCopy

// KeyBundles は鍵バンドルのコピー（fkb/1）を持つ（B-083。第12章 5.4）。
type KeyBundles struct {
	d       *Deps
	devices *DeviceKeys
	signing *UserSigningKeys
}

// Put は PUT /me/keybundle/{kind}/{target}。承認済みの端末だけ。対象は kind device なら要求した端末自身、share なら片を持つ
// 構成員（本人以外）の承認済みか休眠の端末（違えば KEY-001）、recovery-code と family は self、prf は本人の資格情報で PRF が
// 使える物（srv-identity Passkeys.OwnerOf。違えば KEY-009）。本文の user_id、kind、target_id が経路と違えば KEY-009。
// key_version が今の版と違えば KEY-011（params.version）。created_at はサーバーの時刻。本人の他の端末に keybundle.changed。
func (b *KeyBundles) Put(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, kind BundleKind, targetId string, fkb gen.Fkb1) error {
	panic("未実装")
}

// List は GET /me/keybundle（本人が持つコピーの系統と対象と版）。
func (b *KeyBundles) List(ctx context.Context, userId uuid.UUID) ([]BundleMeta, error) {
	panic("未実装")
}

// Get は GET /me/keybundle/{kind}/{target}。無ければ KEY-005。規則 prf_or_approved（kind prf は未承認の端末にも返し、他は承認済みの
// 端末だけ。違えば KEY-004）は呼ぶ側（Apis）が端末の状態を読んで判定する。
func (b *KeyBundles) Get(ctx context.Context, userId uuid.UUID, kind BundleKind, targetId string) (gen.Fkb1, error) {
	panic("未実装")
}

// Delete は DELETE /me/keybundle/{kind}/{target}（再認証は呼ぶ側）。無ければ KEY-005。kind device は byDevice の分だけ（target が
// byDevice でなければ自分の分として無いので KEY-005）。本人の他の端末に keybundle.changed。
func (b *KeyBundles) Delete(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, kind BundleKind, targetId string) error {
	panic("未実装")
}

// remove は本人の 1 つのコピーを消す（無ければ偽で、誤りにしない）。Delete と hook OnPasskeyDeleted が使う。
func (b *KeyBundles) remove(ctx context.Context, tx *store.Tx, userId uuid.UUID, kind BundleKind, targetId string) (bool, error) {
	panic("未実装")
}

// DeleteAllOfUser は抹消で本人のコピー (a)〜(e) と本人が置いた片を消す（hook OnUserErased）。
func (b *KeyBundles) DeleteAllOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}

// DeleteOfDevice は取り消し・休眠・削除でその端末が対象のコピー（kind device のコピー (a) と、その端末へ包んだ kind share の片）を消す。
func (b *KeyBundles) DeleteOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	panic("未実装")
}

// DeleteOlderVersions は S の交代で本人の key_version が version より小さい全系統の行を消す（第12章 5.4）。
func (b *KeyBundles) DeleteOlderVersions(ctx context.Context, tx *store.Tx, userId uuid.UUID, version int) error {
	panic("未実装")
}

// RecoveryParams は回復コードの Argon2id の値（PUT /me/recovery-code の params。第12章 5.4 (b)）。salt は本人の識別子。
type RecoveryParams struct {
	M    int
	T    int
	P    int
	Salt []byte
}

// RecoveryCode は回復コードの照合の値を持つ（B-083。第12章 5.4 (b)。サーバーは包む鍵を導けない）。
type RecoveryCode struct {
	d        *Deps
	bundles  *KeyBundles
	recovery *Recovery
}

// PutVerifier は PUT /me/recovery-code。端末が作った照合の値（32 バイト）と params。初回（行が無い）は未承認の端末でも置ける。
// 置き換え（再発行）は承認済みの端末だけ（KEY-004。再認証は呼ぶ側）。
func (r *RecoveryCode) PutVerifier(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, verifier []byte, params RecoveryParams) error {
	panic("未実装")
}

// Verify は POST /me/keybundle/recovery-code/verify。保存した照合の値と定数時間で比べ、成功でコピー (b)（無ければ KEY-005）。
// 試行の制限は境界の policy recovery（KEY-008）。recovery.used（method recovery-code。成功・失敗とも）。照合の値が無ければ失敗。
func (r *RecoveryCode) Verify(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, verifier []byte) (gen.Fkb1, error) {
	panic("未実装")
}

// deleteOfUser は抹消で照合の値の行を消す（hook OnUserErased）。
func (r *RecoveryCode) deleteOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}

// RecoveryRequest は回復の求め（POST /me/recovery/request の応答）。
type RecoveryRequest = gen.RequestRecoveryResponse

// Shares は秘密分散の片の持ち主と回復の求めを持つ（B-083。第12章 5.4 (c)）。
type Shares struct {
	d        *Deps
	inbox    *Inbox
	bundles  *KeyBundles
	recovery *Recovery
}

// Holders は owner の kind share のコピーの対象の端末の持ち主（承認済みか休眠の端末に限る。重複を除く）。
func (s *Shares) Holders(ctx context.Context, q store.Querier, ownerUserId uuid.UUID) ([]uuid.UUID, error) {
	panic("未実装")
}

// Request は POST /me/recovery/request。承認済みの端末は KEY-002。Holders が 2 人に満たなければ KEY-005（params.kind share）。
// 本人の open の求めがあればそれを返す。無ければ 24 時間の求めを置き、recovery.request を Holders へ、知らせ recovery_requested を
// srv-identity Hooks.Notify で Holders の各人へ（Audience self、UserId はその人。1 人に 1 回）。
func (s *Shares) Request(ctx context.Context, tx *store.Tx, requesterDevice uuid.UUID) (RecoveryRequest, error) {
	panic("未実装")
}

// Approve は POST /recovery/{request_id}/approve（規則 share_holder）。open でないか期限切れなら AUTH-007。holderDevice の持ち主が
// 既に応じていれば何もしない（冪等）。無ければ Inbox.Put（kind share、求めた端末へ）と recovery_approvals に 1 行。別の構成員の
// 2 片で approved にし、Recovery.NotifyUsed(share、success)。
func (s *Shares) Approve(ctx context.Context, tx *store.Tx, holderDevice, requestId uuid.UUID, ciphertext, sig []byte) error {
	panic("未実装")
}

// Expire は open で期限を過ぎた求めを expired にする（srv-worker PruneHandler。知らせは出さない）。戻りは数。
func (s *Shares) Expire(ctx context.Context) (int64, error) {
	panic("未実装")
}

// expireOfUser は抹消で本人の open の求めを expired にする（hook OnUserErased）。
func (s *Shares) expireOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}

// PublicKey は家族回復鍵の公開部と版（GET /family/recovery-key/public の応答）。
type PublicKey = gen.GetFamilyRecoveryKeyPublicResponse

// FamilyRecoveryKey は家族回復鍵の公開部と使用を持つ（B-083。第12章 5.4 (d)）。公開部の置き場は srv-identity の families。
type FamilyRecoveryKey struct {
	d        *Deps
	bundles  *KeyBundles
	recovery *Recovery
}

// Public は srv-identity Families.Get の公開部と版。無ければ KEY-005（params.kind family）。
func (f *FamilyRecoveryKey) Public(ctx context.Context) (PublicKey, error) {
	panic("未実装")
}

// SetPublic は初回と作り直し（/admin/family/recovery-key/rotate。srv-admin から）。srv-identity Families.SetRecoveryPub（版を 1 増やす）、
// 古いコピー (d) を全部消し、全員に keybundle.changed（family）。JWK が P-256 の点でなければ KEY-009。戻りは版。
func (f *FamilyRecoveryKey) SetPublic(ctx context.Context, tx *store.Tx, pub gen.CommonJwkP2561, by uuid.UUID) (int, error) {
	panic("未実装")
}

// Use は POST /family/recovery-key/use（本文なし）。本人のコピー (d) を返し（無ければ KEY-005）、返すことを使用と数えて同じ
// トランザクションで Recovery.NotifyUsed(family、success)（第12章 5.4 (d)、H-12-17）。試行の制限は境界の policy recovery。
func (f *FamilyRecoveryKey) Use(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) (gen.Fkb1, error) {
	panic("未実装")
}
