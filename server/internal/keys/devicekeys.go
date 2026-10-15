// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// DeviceKeys は端末の鍵の登録と承認（fdev/1）を持つ（B-081。第12章 5.3、5.5、第3章 4.4）。端末の承認の有無の正は
// srv-identity の devices.state で、本ブロックは持たない。
type DeviceKeys struct {
	d       *Deps
	signing *UserSigningKeys
	groups  *Groups
	shares  *Shares
}

// fdevCols は fdev/1 を組む列。
const fdevCols = `device_id, user_id, sig_pub, kem_pub, credential, user_sig, created_at, approved_at`

// scanFdev は 1 行を fdev/1 にする。
func scanFdev(row interface{ Scan(...any) error }) (gen.Fdev1, error) {
	var f gen.Fdev1
	var dev, usr uuid.UUID
	var cred, sig []byte
	var created time.Time
	var approved *time.Time
	if err := row.Scan(&dev, &usr, &f.SigPub, &f.KemPub, &cred, &sig, &created, &approved); err != nil {
		return gen.Fdev1{}, err
	}
	f.V, f.DeviceId, f.UserId, f.MlsCredential, f.CreatedAt = 1, dev.String(), usr.String(), b64.EncodeToString(cred), created.UTC()
	if sig != nil {
		s := b64.EncodeToString(sig)
		f.UserSig = &s
	}
	if approved != nil {
		a := approved.UTC()
		f.ApprovedAt = &a
	}
	return f, nil
}

// Put は PUT /me/devices/{id}/keys。fdev/1 を未承認で登録する。端末が無いか本人の物でないか、取り消し済み・期限切れなら KEY-001。
// JWK が P-256 の点でない、credential が第12章 5.3 の 35 バイトと一致しない、署名鍵か鍵合意鍵の公開部が他の端末に登録済み
// （第12章 5.3）なら KEY-009。既に登録があり、鍵と credential が要求と同じなら置き換えず同じ物を返し、違えば KEY-009。
func (k *DeviceKeys) Put(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, sigPub, kemPub gen.CommonJwkP2561, credential []byte) (gen.Fdev1, error) {
	dv, err := k.d.Identity.Devices.Get(ctx, tx, deviceId)
	if err != nil {
		return gen.Fdev1{}, err
	}
	if dv.UserId != userId || dv.State == identity.DeviceRevoked || dv.State == identity.DeviceExpired {
		return gen.Fdev1{}, ev(cgen.KEY001)
	}
	sigSec1, _, ok1 := jwkPoint(sigPub)
	kemSec1, _, ok2 := jwkPoint(kemPub)
	if !ok1 || !ok2 || !bytes.Equal(credential, credentialFor(userId, deviceId)) {
		return gen.Fdev1{}, ev(cgen.KEY009)
	}
	sj, kj := canonicalJwk(sigSec1), canonicalJwk(kemSec1)
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM device_keys WHERE device_id <> $1 AND (sig_pub IN ($2::jsonb, $3::jsonb) OR kem_pub IN ($2::jsonb, $3::jsonb)) LIMIT 1`,
		deviceId, sj, kj).Scan(&one)
	if err == nil {
		return gen.Fdev1{}, ev(cgen.KEY009)
	}
	if !noRows(err) {
		return gen.Fdev1{}, wrap(err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO device_keys (device_id, user_id, sig_pub, kem_pub, credential, created_at) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (device_id) DO NOTHING`, deviceId, userId, sj, kj, credential, k.d.now())
	if uniqueViolation(err) {
		return gen.Fdev1{}, ev(cgen.KEY009)
	}
	if err != nil {
		return gen.Fdev1{}, wrap(err)
	}
	f, err := k.Get(ctx, tx, deviceId)
	if err != nil {
		return gen.Fdev1{}, err
	}
	if tag.RowsAffected() == 0 && (f.SigPub != sj || f.KemPub != kj || f.MlsCredential != b64.EncodeToString(credential)) {
		return gen.Fdev1{}, ev(cgen.KEY009)
	}
	return *f, nil
}

// Approve は POST /me/devices/{id}/approve（ブロックのページの注記の順）。1. 錠を取らずに読む：対象の端末が本人の物（違えば KEY-001）、
// 他の端末の承認は byDevice が承認済みか休眠の時だけ（KEY-003。再認証は呼ぶ側）。2. 子ども区分で保護者の端末の追加の承認が有効で、
// 保護者が通していない端末なら、別のトランザクションで srv-identity Guardians.RequestApproval を確定してから MEM-006。3. srv-identity
// Devices.Approve（KEY-002、KEY-001）。4. device_keys の行の錠の後に本人の署名鍵の公開部で userSig を確かめ（無いか通らなければ
// KEY-003）、user_sig と approved_at を置く。5. その端末の open の回復の求めを expired に。6. Groups.RequestAdd。
func (k *DeviceKeys) Approve(ctx context.Context, tx *store.Tx, userId, byDevice, deviceId uuid.UUID, userSig []byte) (gen.Fdev1, error) {
	devs := k.d.Identity.Devices
	target, err := devs.Get(ctx, tx, deviceId)
	if err != nil {
		return gen.Fdev1{}, err
	}
	if target.UserId != userId {
		return gen.Fdev1{}, ev(cgen.KEY001)
	}
	// 3 の Devices.Approve と同じ番号を先に返す（承認の要らない端末に保護者への求めを出さない）。錠の後の 3 がもう一度確かめる。
	switch target.State {
	case identity.DeviceApproved, identity.DeviceDormant:
		return gen.Fdev1{}, ev(cgen.KEY002)
	case identity.DeviceRevoked, identity.DeviceExpired:
		return gen.Fdev1{}, ev(cgen.KEY001)
	}
	if byDevice != deviceId {
		by, err := devs.Get(ctx, tx, byDevice)
		if err != nil || by.UserId != userId || (by.State != identity.DeviceApproved && by.State != identity.DeviceDormant) {
			return gen.Fdev1{}, ev(cgen.KEY003)
		}
	}
	usr, err := k.d.Identity.Users.Get(ctx, tx, userId)
	if err != nil {
		return gen.Fdev1{}, err
	}
	if usr.IsChild {
		set, err := k.d.Identity.Guardians.Settings(ctx, tx, userId)
		if err != nil {
			return gen.Fdev1{}, err
		}
		cleared, err := devs.GuardianCleared(ctx, tx, deviceId)
		if err != nil {
			return gen.Fdev1{}, err
		}
		if (set.DeviceApproval == nil || *set.DeviceApproval) && !cleared {
			did := deviceId
			if err := k.d.separateTx(ctx, func(stx *store.Tx) error {
				_, err := k.d.Identity.Guardians.RequestApproval(ctx, stx, userId, identity.ApprovalDevice, identity.ApprovalTarget{DeviceId: &did})
				return err
			}); err != nil {
				return gen.Fdev1{}, err
			}
			return gen.Fdev1{}, ev(cgen.MEM006)
		}
	}
	if err := devs.Approve(ctx, tx, deviceId, userId); err != nil {
		return gen.Fdev1{}, err
	}
	row := tx.QueryRow(ctx, `SELECT `+fdevCols+` FROM device_keys WHERE device_id = $1 FOR UPDATE`, deviceId)
	f, err := scanFdev(row)
	if noRows(err) {
		return gen.Fdev1{}, ev(cgen.KEY001)
	}
	if err != nil {
		return gen.Fdev1{}, wrap(err)
	}
	fusr, err := k.signing.Get(ctx, tx, userId)
	if err != nil {
		return gen.Fdev1{}, err
	}
	if fusr == nil {
		return gen.Fdev1{}, ev(cgen.KEY003)
	}
	_, pub, ok := jwkPoint(fusr.UserSigPub)
	if !ok || !verifyFdev(f, userSig, pub) {
		return gen.Fdev1{}, ev(cgen.KEY003)
	}
	if _, err := tx.Exec(ctx, `UPDATE device_keys SET user_sig = $2, approved_at = $3 WHERE device_id = $1`, deviceId, userSig, k.d.now()); err != nil {
		return gen.Fdev1{}, wrap(err)
	}
	if err := k.shares.expireOfDevice(ctx, tx, deviceId); err != nil {
		return gen.Fdev1{}, err
	}
	if _, err := k.groups.RequestAdd(ctx, tx, deviceId); err != nil {
		return gen.Fdev1{}, err
	}
	out, err := k.Get(ctx, tx, deviceId)
	if err != nil {
		return gen.Fdev1{}, err
	}
	return *out, nil
}

// Get は fdev/1。無ければ nil。q は ctx のトランザクションか Pool。
func (k *DeviceKeys) Get(ctx context.Context, q store.Querier, deviceId uuid.UUID) (*gen.Fdev1, error) {
	f, err := scanFdev(q.QueryRow(ctx, `SELECT `+fdevCols+` FROM device_keys WHERE device_id = $1`, deviceId))
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrap(err)
	}
	return &f, nil
}

// ListOfUser は構成員の端末の fdev/1（端末の識別子の順）。approvedOnly なら承認済みと休眠の端末だけ（DeviceKeyReader.DevicesOf）、
// 偽なら登録のある全端末（未承認を含む。DeviceKeyReader.KeysOf）。q は ctx のトランザクションか Pool。
func (k *DeviceKeys) ListOfUser(ctx context.Context, q store.Querier, userId uuid.UUID, approvedOnly bool) ([]gen.Fdev1, error) {
	all, err := k.listOfUser(ctx, q, userId)
	if err != nil {
		return nil, err
	}
	keep := map[uuid.UUID]bool{}
	if approvedOnly {
		devs, err := k.d.Identity.Devices.OfUser(ctx, q, userId)
		if err != nil {
			return nil, err
		}
		for _, dv := range devs {
			if dv.State == identity.DeviceApproved || dv.State == identity.DeviceDormant {
				keep[dv.Id] = true
			}
		}
	}
	out := []gen.Fdev1{}
	for _, id := range sortedIds(mapKeys(all)) {
		if !approvedOnly || keep[id] {
			out = append(out, all[id])
		}
	}
	return out, nil
}

// listOfUser は構成員の device_keys の全行（端末の識別子から引く）。
func (k *DeviceKeys) listOfUser(ctx context.Context, q store.Querier, userId uuid.UUID) (map[uuid.UUID]gen.Fdev1, error) {
	rows, err := q.Query(ctx, `SELECT `+fdevCols+` FROM device_keys WHERE user_id = $1 ORDER BY device_id`, userId)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]gen.Fdev1{}
	for rows.Next() {
		f, err := scanFdev(rows)
		if err != nil {
			return nil, wrap(err)
		}
		out[uuid.FromStringOrNil(f.DeviceId)] = f
	}
	return out, wrap(rows.Err())
}

// VerifyProof は fdev/1 の署名鍵の公開部で ECDSA P-256（SHA-256、r と s の 64 バイト）を確かめる（DeviceKeyReader）。
// 公開鍵が無ければ偽。DB の読みの失敗は error。q は ctx のトランザクションか Pool。
func (k *DeviceKeys) VerifyProof(ctx context.Context, q store.Querier, deviceId uuid.UUID, message, sig []byte) (bool, error) {
	reg, ok, err := readRegistration(ctx, q, deviceId, false)
	if err != nil || !ok {
		return false, err
	}
	return verifyRS(reg.sigPub, message, sig), nil
}

// VerifyUserSig は第12章 5.3 の user_sig の対象を組み立てて fusr の公開部で確かめる（記録は読まない）。fdev の user_sig が無いか
// 解けないか、鍵が P-256 の点でなければ偽。
func (k *DeviceKeys) VerifyUserSig(fdev gen.Fdev1, fusr gen.Fusr1) bool {
	if fdev.UserSig == nil {
		return false
	}
	sig, ok := decodeB64(*fdev.UserSig)
	_, pub, okp := jwkPoint(fusr.UserSigPub)
	return ok && okp && verifyFdev(fdev, sig, pub)
}

// verifyFdev は fdev の端末に対する sig を本人の署名鍵 pub で確かめる（第12章 5.3 の 179 バイト）。
func verifyFdev(f gen.Fdev1, sig []byte, pub *ecdsa.PublicKey) bool {
	user, err1 := uuid.FromString(f.UserId)
	dev, err2 := uuid.FromString(f.DeviceId)
	sigSec1, _, ok1 := jwkPoint(f.SigPub)
	kemSec1, _, ok2 := jwkPoint(f.KemPub)
	if err1 != nil || err2 != nil || !ok1 || !ok2 {
		return false
	}
	return verifyRS(pub, userSigMessage(user, dev, sigSec1, kemSec1), sig)
}

// Revoke は端末の鍵の行を消す（hook OnDeviceRevoked から。無ければ何もしない）。
func (k *DeviceKeys) Revoke(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM device_keys WHERE device_id = $1`, deviceId)
	return wrap(err)
}
