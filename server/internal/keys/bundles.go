// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
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

// 包みの方式（fkb/1 の alg。第12章 5.4 の系統の表）。
const (
	algAes  = "A256GCM"
	algHpke = "HPKE-P256"
)

// shareIndexMax は片の番号の上限（GF(256) の x。第12章 5.4 (c)）。
const shareIndexMax = 255

// BundleMeta はコピーの一覧の 1 行（GET /me/keybundle。中身は返さない）。
type BundleMeta = gen.KeyBundleCopy

// KeyBundles は鍵バンドルのコピー（fkb/1）を持つ（B-083。第12章 5.4）。
type KeyBundles struct {
	d       *Deps
	devices *DeviceKeys
	signing *UserSigningKeys
}

// kindErr は KEY-005（コピーが無い。params.kind）。
func kindErr(kind BundleKind) error {
	return fault.New(cgen.KEY005, map[string]any{"kind": string(kind)})
}

// Put は PUT /me/keybundle/{kind}/{target}（byDevice は承認済み。境界の approved）。本文の user_id、kind、target_id が経路と違えば
// KEY-009。対象は kind device なら要求した端末自身、share なら片を持つ構成員（本人以外）の承認済みか休眠で鍵の登録のある端末
// （違えば KEY-001）、recovery-code と family は self、prf は本人の資格情報で PRF が使える物（違えば KEY-009）。形（alg、kdf、share、
// nonce と ciphertext）が第12章 5.4 の系統の表と違えば KEY-009。片の番号は、同じ構成員の端末の片と同じで、別の構成員の片と違う
// （違えば KEY-009）。key_version が今の版と違えば KEY-011（params.version）。created_at はサーバーの時刻。本人へ keybundle.changed。
// 再認証は呼ぶ側。
func (b *KeyBundles) Put(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, kind BundleKind, targetId string, fkb gen.Fkb1) error {
	if u, err := uuid.FromString(fkb.UserId); err != nil || u != userId || string(fkb.Kind) != string(kind) || fkb.TargetId != targetId {
		return ev(cgen.KEY009)
	}
	var holder uuid.UUID
	switch kind {
	case BundleDevice:
		if targetId != byDevice.String() {
			return ev(cgen.KEY001)
		}
	case BundleShare:
		h, err := b.shareTarget(ctx, tx, userId, targetId)
		if err != nil {
			return err
		}
		holder = h
	case BundleRecoveryCode, BundleFamily:
		if targetId != targetSelf {
			return ev(cgen.KEY001)
		}
	case BundlePrf:
		cred, ok := decodeB64(targetId)
		if !ok {
			return ev(cgen.KEY009)
		}
		o, err := b.d.Identity.Passkeys.OwnerOf(ctx, tx, cred)
		if err != nil {
			return err
		}
		if o.UserId != userId || !o.Prf {
			return ev(cgen.KEY009)
		}
	default:
		return ev(cgen.KEY009)
	}
	if err := b.checkForm(ctx, tx, userId, kind, fkb); err != nil {
		return err
	}
	if kind == BundleShare {
		if err := b.checkShareIndex(ctx, tx, userId, targetId, holder, fkb.Share.Index); err != nil {
			return err
		}
	}
	v, err := b.signing.Version(ctx, tx, userId)
	if err != nil {
		return err
	}
	if fkb.KeyVersion != v {
		return fault.New(cgen.KEY011, map[string]any{"version": v})
	}
	fkb.CreatedAt = b.d.now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO keybundles (user_id, kind, target_id, key_version, body, created_at) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, kind, target_id) DO UPDATE SET key_version = EXCLUDED.key_version, body = EXCLUDED.body, created_at = EXCLUDED.created_at`,
		userId, string(kind), targetId, fkb.KeyVersion, fkb, fkb.CreatedAt); err != nil {
		return wrap(err)
	}
	return b.d.emit(tx, []uuid.UUID{userId}, "keybundle.changed", uuid.Nil, uuid.Nil, keybundleChanged{Kinds: []BundleKind{kind}})
}

// shareTarget は kind share の対象の端末を確かめ、その持ち主（片を持つ構成員）を返す。本人以外の構成員の、承認済みか休眠で鍵の
// 登録のある端末でなければ KEY-001。
func (b *KeyBundles) shareTarget(ctx context.Context, q store.Querier, owner uuid.UUID, targetId string) (uuid.UUID, error) {
	id, err := uuid.FromString(targetId)
	if err != nil || id.String() != targetId {
		return uuid.Nil, ev(cgen.KEY001)
	}
	dv, err := b.d.Identity.Devices.Get(ctx, q, id)
	if err != nil {
		return uuid.Nil, err
	}
	if dv.UserId == owner || (dv.State != identity.DeviceApproved && dv.State != identity.DeviceDormant) {
		return uuid.Nil, ev(cgen.KEY001)
	}
	f, err := b.devices.Get(ctx, q, id)
	if err != nil {
		return uuid.Nil, err
	}
	if f == nil {
		return uuid.Nil, ev(cgen.KEY001)
	}
	return dv.UserId, nil
}

// checkForm は fkb/1 の形を系統の表と照らす（第12章 5.4、12）。違えば KEY-009。
func (b *KeyBundles) checkForm(ctx context.Context, q store.Querier, userId uuid.UUID, kind BundleKind, fkb gen.Fkb1) error {
	wantAlg := algAes
	if kind == BundleShare || kind == BundleFamily {
		wantAlg = algHpke
	}
	if string(fkb.Alg) != wantAlg || (fkb.Kdf != nil) != (kind == BundleRecoveryCode) || (fkb.Share != nil) != (kind == BundleShare) {
		return ev(cgen.KEY009)
	}
	if _, ok := decodeB64(fkb.Nonce); !ok {
		return ev(cgen.KEY009)
	}
	if ct, ok := decodeB64(fkb.Ciphertext); !ok || len(ct) == 0 {
		return ev(cgen.KEY009)
	}
	if fkb.Share != nil && (fkb.Share.Index < 1 || fkb.Share.Index > shareIndexMax) {
		return ev(cgen.KEY009)
	}
	if kind == BundleRecoveryCode {
		p, ok, err := readRecoveryParams(ctx, q, userId)
		if err != nil {
			return err
		}
		k := fkb.Kdf
		if alg, _ := k.Alg.(string); !ok || alg != "argon2id" || k.M != p.M || k.T != p.T || k.P != p.P || k.Salt != b64.EncodeToString(p.Salt) {
			return ev(cgen.KEY009)
		}
	}
	return nil
}

// checkShareIndex は片の番号（第12章 5.4 (c)）：同じ持ち主の既にある片のうち、同じ構成員の端末の片と同じ番号で、別の構成員の片と
// 違う番号。違う 2 人が同じ番号の片を持つと、2 人の承認で S が戻らない。
func (b *KeyBundles) checkShareIndex(ctx context.Context, q store.Querier, owner uuid.UUID, targetId string, holder uuid.UUID, index int) error {
	rows, err := q.Query(ctx, `SELECT target_id, (body->'share'->>'index')::int FROM keybundles WHERE user_id = $1 AND kind = 'share' AND target_id <> $2`,
		owner, targetId)
	if err != nil {
		return wrap(err)
	}
	type row struct {
		target string
		index  int
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.target, &r.index); err != nil {
			rows.Close()
			return wrap(err)
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrap(err)
	}
	for _, r := range list {
		dev, err := uuid.FromString(r.target)
		if err != nil {
			continue
		}
		dv, err := b.d.Identity.Devices.Get(ctx, q, dev)
		if isEvent(err, cgen.KEY001) {
			continue
		}
		if err != nil {
			return err
		}
		if (dv.UserId == holder) != (r.index == index) {
			return ev(cgen.KEY009)
		}
	}
	return nil
}

// List は GET /me/keybundle（本人が持つコピーの系統と対象と版と作成日。系統と対象の順）。
func (b *KeyBundles) List(ctx context.Context, userId uuid.UUID) ([]BundleMeta, error) {
	rows, err := b.d.Pool.Query(ctx, `SELECT kind, target_id, key_version, created_at FROM keybundles WHERE user_id = $1 ORDER BY kind, target_id`, userId)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	out := []BundleMeta{}
	for rows.Next() {
		var m BundleMeta
		var kind string
		var created time.Time
		if err := rows.Scan(&kind, &m.Target, &m.KeyVersion, &created); err != nil {
			return nil, wrap(err)
		}
		m.Kind, m.CreatedAt = gen.KeyBundleCopyKind(kind), created.UTC()
		out = append(out, m)
	}
	return out, wrap(rows.Err())
}

// Get は GET /me/keybundle/{kind}/{target} の本体（本人の 1 つのコピー）。無ければ KEY-005（params.kind）。規則 prf_or_approved は
// 呼ぶ側（Apis）が判定する。q は ctx のトランザクションか Pool。
func (b *KeyBundles) Get(ctx context.Context, q store.Querier, userId uuid.UUID, kind BundleKind, targetId string) (gen.Fkb1, error) {
	var f gen.Fkb1
	err := q.QueryRow(ctx, `SELECT body FROM keybundles WHERE user_id = $1 AND kind = $2 AND target_id = $3`, userId, string(kind), targetId).Scan(&f)
	if noRows(err) {
		return gen.Fkb1{}, kindErr(kind)
	}
	return f, wrap(err)
}

// Delete は DELETE /me/keybundle/{kind}/{target}（再認証は呼ぶ側）。無ければ KEY-005。kind device は byDevice の分だけ（target が
// byDevice でなければ自分の分として無いので KEY-005）。本人へ keybundle.changed。
func (b *KeyBundles) Delete(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, kind BundleKind, targetId string) error {
	if kind == BundleDevice && targetId != byDevice.String() {
		return kindErr(kind)
	}
	ok, err := b.remove(ctx, tx, userId, kind, targetId)
	if err != nil {
		return err
	}
	if !ok {
		return kindErr(kind)
	}
	return b.d.emit(tx, []uuid.UUID{userId}, "keybundle.changed", uuid.Nil, uuid.Nil, keybundleChanged{Kinds: []BundleKind{kind}})
}

// remove は本人の 1 つのコピーを消す（無ければ偽で、誤りにしない）。Delete と hook OnPasskeyDeleted が使う。
func (b *KeyBundles) remove(ctx context.Context, tx *store.Tx, userId uuid.UUID, kind BundleKind, targetId string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM keybundles WHERE user_id = $1 AND kind = $2 AND target_id = $3`, userId, string(kind), targetId)
	if err != nil {
		return false, wrap(err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteAllOfUser は抹消で本人のコピー (a)〜(e) と本人が置いた片を消す（hook OnUserErased）。
func (b *KeyBundles) DeleteAllOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM keybundles WHERE user_id = $1`, userId)
	return wrap(err)
}

// DeleteOfDevice は取り消し・削除でその端末が対象のコピー（kind device のコピー (a) と、その端末へ包んだ kind share の片）を消す。
func (b *KeyBundles) DeleteOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM keybundles WHERE target_id = $1 AND kind IN ('device', 'share')`, deviceId.String())
	return wrap(err)
}

// deleteDeviceCopy は休眠でその端末のコピー (a) だけを消す（その端末へ包んだ他人の片は残す。第12章 5.2）。
func (b *KeyBundles) deleteDeviceCopy(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM keybundles WHERE target_id = $1 AND kind = 'device'`, deviceId.String())
	return wrap(err)
}

// DeleteOlderVersions は S の交代で本人の key_version が version より小さい全系統の行を消す（第12章 5.4）。
func (b *KeyBundles) DeleteOlderVersions(ctx context.Context, tx *store.Tx, userId uuid.UUID, version int) error {
	_, err := tx.Exec(ctx, `DELETE FROM keybundles WHERE user_id = $1 AND key_version < $2`, userId, version)
	return wrap(err)
}

// RecoveryParams は回復コードの Argon2id の値（PUT /me/recovery-code の params。第12章 5.4 (b)）。salt は本人の識別子。
type RecoveryParams struct {
	M    int
	T    int
	P    int
	Salt []byte
}

// storedParams は recovery_codes.params の形（fkb/1 の kdf と同じ欄）。
type storedParams struct {
	Alg  string `json:"alg"`
	M    int    `json:"m"`
	T    int    `json:"t"`
	P    int    `json:"p"`
	Salt string `json:"salt"`
}

// readRecoveryParams は本人の照合の値の params（無ければ ok は偽）。
func readRecoveryParams(ctx context.Context, q store.Querier, userId uuid.UUID) (RecoveryParams, bool, error) {
	var sp storedParams
	err := q.QueryRow(ctx, `SELECT params FROM recovery_codes WHERE user_id = $1`, userId).Scan(&sp)
	if noRows(err) {
		return RecoveryParams{}, false, nil
	}
	if err != nil {
		return RecoveryParams{}, false, wrap(err)
	}
	salt, _ := decodeB64(sp.Salt)
	return RecoveryParams{M: sp.M, T: sp.T, P: sp.P, Salt: salt}, true, nil
}

// RecoveryCode は回復コードの照合の値を持つ（B-083。第12章 5.4 (b)。サーバーは包む鍵を導けない）。
type RecoveryCode struct {
	d        *Deps
	bundles  *KeyBundles
	recovery *Recovery
}

// PutVerifier は PUT /me/recovery-code。端末が作った照合の値（32 バイト）と params。verifier が 32 バイトでないか、params.salt が本人の
// 識別子の 16 バイトでなければ NET-001。初回（行が無い）は未承認の端末でも置ける。置き換え（再発行）は承認済みか休眠の端末だけ
// （KEY-004。再認証は呼ぶ側）。イベントは無い（コピー (b) の置き直しは端末が KeyBundles.Put で行う）。
func (r *RecoveryCode) PutVerifier(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, verifier []byte, params RecoveryParams) error {
	if len(verifier) != 32 {
		return errNet001("body/verifier", "32 バイトでない")
	}
	if !bytes.Equal(params.Salt, userId.Bytes()) {
		return errNet001("body/params/salt", "本人の識別子でない")
	}
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM recovery_codes WHERE user_id = $1 FOR UPDATE`, userId).Scan(&one)
	if err != nil && !noRows(err) {
		return wrap(err)
	}
	if err == nil {
		dv, err := r.d.Identity.Devices.Get(ctx, tx, byDevice)
		if err != nil {
			return err
		}
		if dv.UserId != userId || (dv.State != identity.DeviceApproved && dv.State != identity.DeviceDormant) {
			return ev(cgen.KEY004)
		}
	}
	sp := storedParams{Alg: "argon2id", M: params.M, T: params.T, P: params.P, Salt: b64.EncodeToString(params.Salt)}
	_, err = tx.Exec(ctx, `INSERT INTO recovery_codes (user_id, verifier, params, issued_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE SET verifier = EXCLUDED.verifier, params = EXCLUDED.params, issued_at = EXCLUDED.issued_at`,
		userId, verifier, sp, r.d.now())
	return wrap(err)
}

// Verify は POST /me/keybundle/recovery-code/verify。保存した照合の値と定数時間で比べる。成功：コピー (b)（無ければ KEY-005）を返し、
// 同じトランザクションで recovery.used(recovery-code、success)。失敗：recovery.used(recovery-code、failure) を別のトランザクションで
// 確定してから、照合の値が無ければ KEY-005、合わなければ KEY-012（32 バイトでない値も合わない物）。試行の制限は境界の policy
// recovery（KEY-008）。
func (r *RecoveryCode) Verify(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, verifier []byte) (gen.Fkb1, error) {
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT verifier FROM recovery_codes WHERE user_id = $1`, userId).Scan(&stored)
	if err != nil && !noRows(err) {
		return gen.Fkb1{}, wrap(err)
	}
	if err != nil || subtle.ConstantTimeCompare(stored, verifier) != 1 {
		code := cgen.KEY012
		if err != nil {
			code = cgen.KEY005
		}
		if ferr := r.d.separateTx(ctx, func(stx *store.Tx) error {
			return r.recovery.NotifyUsed(ctx, stx, userId, deviceId, MethodRecoveryCode, ResultFailure)
		}); ferr != nil {
			return gen.Fkb1{}, ferr
		}
		if code == cgen.KEY005 {
			return gen.Fkb1{}, kindErr(BundleRecoveryCode)
		}
		return gen.Fkb1{}, ev(code)
	}
	f, err := r.bundles.Get(ctx, tx, userId, BundleRecoveryCode, targetSelf)
	if err != nil {
		return gen.Fkb1{}, err
	}
	if err := r.recovery.NotifyUsed(ctx, tx, userId, deviceId, MethodRecoveryCode, ResultSuccess); err != nil {
		return gen.Fkb1{}, err
	}
	return f, nil
}

// deleteOfUser は抹消で照合の値の行を消す（hook OnUserErased）。
func (r *RecoveryCode) deleteOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM recovery_codes WHERE user_id = $1`, userId)
	return wrap(err)
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

// Holders は owner の kind share のコピーの対象の端末の持ち主（承認済みか休眠の端末に限る。重複を除き識別子の順）。
func (s *Shares) Holders(ctx context.Context, q store.Querier, ownerUserId uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `SELECT target_id FROM keybundles WHERE user_id = $1 AND kind = 'share'`, ownerUserId)
	if err != nil {
		return nil, wrap(err)
	}
	var targets []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, wrap(err)
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, wrap(err)
	}
	var out []uuid.UUID
	for _, t := range targets {
		id, err := uuid.FromString(t)
		if err != nil {
			continue
		}
		dv, err := s.d.Identity.Devices.Get(ctx, q, id)
		if isEvent(err, cgen.KEY001) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if dv.State == identity.DeviceApproved || dv.State == identity.DeviceDormant {
			out = append(out, dv.UserId)
		}
	}
	return sortedIds(out), nil
}

// Request は POST /me/recovery/request。承認済みか休眠の端末は KEY-002。本人の open の求めがあればそれを返す（冪等）。Holders が 2 人に
// 満たなければ KEY-005（params.kind share）。無ければ 24 時間の求めを置き、recovery.request を Holders へ、知らせ recovery_requested を
// srv-identity Hooks.Notify で Holders の各人へ（Audience self、UserId はその人。1 人に 1 回）。
func (s *Shares) Request(ctx context.Context, tx *store.Tx, requesterDevice uuid.UUID) (RecoveryRequest, error) {
	dv, err := s.d.Identity.Devices.Get(ctx, tx, requesterDevice)
	if err != nil {
		return RecoveryRequest{}, err
	}
	if dv.State == identity.DeviceApproved || dv.State == identity.DeviceDormant {
		return RecoveryRequest{}, ev(cgen.KEY002)
	}
	userId := dv.UserId
	now := s.d.now()
	if _, err := tx.Exec(ctx, `UPDATE recovery_requests SET state = 'expired' WHERE user_id = $1 AND state = 'open' AND expires_at <= $2`, userId, now); err != nil {
		return RecoveryRequest{}, wrap(err)
	}
	if open, ok, err := s.openOf(ctx, tx, userId); err != nil || ok {
		return open, err
	}
	holders, err := s.Holders(ctx, tx, userId)
	if err != nil {
		return RecoveryRequest{}, err
	}
	if len(holders) < shareThreshold {
		return RecoveryRequest{}, kindErr(BundleShare)
	}
	id := s.d.Ids.NewV7()
	expires := now.Add(recoveryRequestTtl).UTC()
	tag, err := tx.Exec(ctx, `INSERT INTO recovery_requests (id, user_id, device_id, state, expires_at, created_at) VALUES ($1, $2, $3, 'open', $4, $5)
ON CONFLICT (user_id) WHERE state = 'open' DO NOTHING`, id, userId, requesterDevice, expires, now)
	if err != nil {
		return RecoveryRequest{}, wrap(err)
	}
	if tag.RowsAffected() == 0 {
		// 同時の求めが先に置いた。
		open, _, err := s.openOf(ctx, tx, userId)
		return open, err
	}
	name, err := s.d.Identity.Devices.NameOf(ctx, tx, requesterDevice)
	if err != nil {
		return RecoveryRequest{}, err
	}
	usr, err := s.d.Identity.Users.Get(ctx, tx, userId)
	if err != nil {
		return RecoveryRequest{}, err
	}
	payload := recoveryRequestEvent{RequestId: id, UserId: userId, DeviceId: requesterDevice, DeviceName: name, ExpiresAt: expires.Format(time.RFC3339)}
	if err := s.d.emit(tx, holders, "recovery.request", uuid.Nil, uuid.Nil, payload); err != nil {
		return RecoveryRequest{}, err
	}
	for _, h := range holders {
		hid := h
		n := identity.Notice{Kind: noticeRecoveryRequested, Key: id.String(), Audience: identity.Audience{Kind: identity.AudienceSelf, UserId: &hid},
			Params: map[string]any{"request_id": id.String(), "user_id": userId.String(), "name": usr.DisplayName, "device_name": name,
				"expires_at": expires.Format(time.RFC3339)}}
		if err := s.d.Identity.Hooks.Notify(ctx, tx, n); err != nil {
			return RecoveryRequest{}, err
		}
	}
	return RecoveryRequest{RequestId: id.String(), ExpiresAt: expires}, nil
}

// openOf は本人の open で期限の内の求め（行の錠）。
func (s *Shares) openOf(ctx context.Context, tx *store.Tx, userId uuid.UUID) (RecoveryRequest, bool, error) {
	var id uuid.UUID
	var expires time.Time
	err := tx.QueryRow(ctx, `SELECT id, expires_at FROM recovery_requests WHERE user_id = $1 AND state = 'open' AND expires_at > $2 FOR UPDATE`,
		userId, s.d.now()).Scan(&id, &expires)
	if noRows(err) {
		return RecoveryRequest{}, false, nil
	}
	if err != nil {
		return RecoveryRequest{}, false, wrap(err)
	}
	return RecoveryRequest{RequestId: id.String(), ExpiresAt: expires.UTC()}, true, nil
}

// request は回復の求めの 1 行。
type request struct {
	userId   uuid.UUID
	deviceId uuid.UUID
	state    string
	expires  time.Time
}

// readRequest は求めの行を読む（lock なら FOR UPDATE）。無い、open でない、期限を過ぎたなら AUTH-007。
func (s *Shares) readRequest(ctx context.Context, q store.Querier, requestId uuid.UUID, lock bool) (request, error) {
	sql := `SELECT user_id, device_id, state, expires_at FROM recovery_requests WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var r request
	err := q.QueryRow(ctx, sql, requestId).Scan(&r.userId, &r.deviceId, &r.state, &r.expires)
	if noRows(err) {
		return request{}, ev(cgen.AUTH007)
	}
	if err != nil {
		return request{}, wrap(err)
	}
	if r.state != "open" || !r.expires.After(s.d.now()) {
		return request{}, ev(cgen.AUTH007)
	}
	return r, nil
}

// holderOf は holderDevice の持ち主が owner の片を持つ構成員か（規則 share_holder）。違えば AUTH-007（他人の求めの有無を漏らさない）。
func (s *Shares) holderOf(ctx context.Context, q store.Querier, owner, holderDevice uuid.UUID) (uuid.UUID, error) {
	dv, err := s.d.Identity.Devices.Get(ctx, q, holderDevice)
	if isEvent(err, cgen.KEY001) {
		return uuid.Nil, ev(cgen.AUTH007)
	}
	if err != nil {
		return uuid.Nil, err
	}
	holders, err := s.Holders(ctx, q, owner)
	if err != nil {
		return uuid.Nil, err
	}
	for _, h := range holders {
		if h == dv.UserId {
			return h, nil
		}
	}
	return uuid.Nil, ev(cgen.AUTH007)
}

// ShareFor は GET /recovery/{request_id}/share。承認する端末が自分宛ての片のコピー（求めた本人の kind share で target が holderDevice）
// を取る。求めが無いか open でないか期限切れ、または holderDevice の持ち主が片を持つ構成員でなければ AUTH-007。コピーが無ければ KEY-005
// （params.kind share）。使用の記録は書かない（片 1 つでは S は戻らない）。q は ctx のトランザクションか Pool。
func (s *Shares) ShareFor(ctx context.Context, q store.Querier, requestId, holderDevice uuid.UUID) (gen.Fkb1, error) {
	r, err := s.readRequest(ctx, q, requestId, false)
	if err != nil {
		return gen.Fkb1{}, err
	}
	if _, err := s.holderOf(ctx, q, r.userId, holderDevice); err != nil {
		return gen.Fkb1{}, err
	}
	return s.bundles.Get(ctx, q, r.userId, BundleShare, holderDevice.String())
}

// Approve は POST /recovery/{request_id}/approve（規則 share_holder。再認証は呼ぶ側）。求めの行の錠。open でないか期限切れ、holderDevice の
// 持ち主が片を持つ構成員でない、求めた端末が未承認でないなら AUTH-007。holderDevice の持ち主が既に応じていれば何もしない（冪等）。
// 無ければ Inbox の内の put（kind share、求めた端末へ。件数の上限は掛けない）と recovery_approvals に 1 行。別の構成員の 2 片で
// approved にし、Recovery.NotifyUsed(share、success)。
func (s *Shares) Approve(ctx context.Context, tx *store.Tx, holderDevice, requestId uuid.UUID, ciphertext, sig []byte) error {
	r, err := s.readRequest(ctx, tx, requestId, true)
	if err != nil {
		return err
	}
	holder, err := s.holderOf(ctx, tx, r.userId, holderDevice)
	if err != nil {
		return err
	}
	req, err := s.d.Identity.Devices.Get(ctx, tx, r.deviceId)
	if isEvent(err, cgen.KEY001) || (err == nil && req.State != identity.DeviceUnapproved) {
		return ev(cgen.AUTH007)
	}
	if err != nil {
		return err
	}
	// 求めた端末に鍵の登録が無ければ片を包めない（求めは使えない物として AUTH-007。approveRecovery の x-errors の中）。
	if _, ok, err := readRegistration(ctx, tx, r.deviceId, false); err != nil || !ok {
		if err != nil {
			return err
		}
		return ev(cgen.AUTH007)
	}
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM recovery_approvals WHERE request_id = $1 AND holder_user_id = $2`, requestId, holder).Scan(&one)
	if err == nil {
		return nil
	}
	if !noRows(err) {
		return wrap(err)
	}
	if _, err := s.inbox.put(ctx, tx, holderDevice, r.deviceId, InboxShare, ciphertext, sig, false); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO recovery_approvals (request_id, holder_user_id, holder_device_id, created_at) VALUES ($1, $2, $3, $4)`,
		requestId, holder, holderDevice, s.d.now()); err != nil {
		return wrap(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM recovery_approvals WHERE request_id = $1`, requestId).Scan(&n); err != nil {
		return wrap(err)
	}
	if n != shareThreshold {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE recovery_requests SET state = 'approved' WHERE id = $1`, requestId); err != nil {
		return wrap(err)
	}
	return s.recovery.NotifyUsed(ctx, tx, r.userId, r.deviceId, MethodShare, ResultSuccess)
}

// Expire は open で期限を過ぎた求めを expired にする（srv-worker PruneHandler。知らせは出さない）。戻りは数。
func (s *Shares) Expire(ctx context.Context) (int64, error) {
	tag, err := s.d.Pool.Exec(ctx, `UPDATE recovery_requests SET state = 'expired' WHERE state = 'open' AND expires_at <= $1`, s.d.now())
	if err != nil {
		return 0, wrap(err)
	}
	return tag.RowsAffected(), nil
}

// expireOfUser は抹消で本人の open の求めを expired にする（hook OnUserErased）。
func (s *Shares) expireOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE recovery_requests SET state = 'expired' WHERE user_id = $1 AND state = 'open'`, userId)
	return wrap(err)
}

// expireOfDevice は端末の承認と取り消しで、その端末の open の求めを expired にする（承認で要らなくなり、取り消しで宛先が無くなる）。
func (s *Shares) expireOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE recovery_requests SET state = 'expired' WHERE device_id = $1 AND state = 'open'`, deviceId)
	return wrap(err)
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
	fam, err := f.d.Identity.Families.Get(ctx)
	if err != nil {
		return PublicKey{}, err
	}
	if len(fam.RecoveryPub) == 0 {
		return PublicKey{}, kindErr(BundleFamily)
	}
	var jwk gen.CommonJwkP2561
	if err := json.Unmarshal(fam.RecoveryPub, &jwk); err != nil {
		return PublicKey{}, wrap(err)
	}
	return PublicKey{PublicKey: jwk, Version: fam.RecoveryKeyVersion}, nil
}

// SetPublic は初回と作り直し（/admin/family/recovery-key/rotate。srv-admin から）。JWK が P-256 の点でなければ KEY-009。srv-identity
// Families.SetRecoveryPub（版を 1 増やす）、古いコピー (d) を全部消し、全員に keybundle.changed（family）。戻りは版。by は記録に使わない
// （監査は srv-admin が書く）。
func (f *FamilyRecoveryKey) SetPublic(ctx context.Context, tx *store.Tx, pub gen.CommonJwkP2561, by uuid.UUID) (int, error) {
	sec1, _, ok := jwkPoint(pub)
	if !ok {
		return 0, ev(cgen.KEY009)
	}
	raw, err := json.Marshal(canonicalJwk(sec1))
	if err != nil {
		return 0, wrap(err)
	}
	v, err := f.d.Identity.Families.SetRecoveryPub(ctx, tx, raw)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM keybundles WHERE kind = 'family'`); err != nil {
		return 0, wrap(err)
	}
	family, err := f.d.Identity.Users.FamilyIds(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := f.d.emit(tx, family, "keybundle.changed", uuid.Nil, uuid.Nil, keybundleChanged{Kinds: []BundleKind{BundleFamily}}); err != nil {
		return 0, err
	}
	return v, nil
}

// Use は POST /family/recovery-key/use（本文なし）。本人のコピー (d) を返し（無ければ KEY-005。記録は書かない）、返すことを使用と数えて
// 同じトランザクションで Recovery.NotifyUsed(family、success)（第12章 5.4 (d)、H-12-17）。試行の制限は境界の policy recovery。
func (f *FamilyRecoveryKey) Use(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) (gen.Fkb1, error) {
	c, err := f.bundles.Get(ctx, tx, userId, BundleFamily, targetSelf)
	if err != nil {
		return gen.Fkb1{}, err
	}
	if err := f.recovery.NotifyUsed(ctx, tx, userId, deviceId, MethodFamily, ResultSuccess); err != nil {
		return gen.Fkb1{}, err
	}
	return c, nil
}
