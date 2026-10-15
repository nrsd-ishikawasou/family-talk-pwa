// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
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

// Put は PUT /me/signing-key。初回（行が無い）は版 1 で置く（deviceSigs は空でなければ KEY-009。自己承認は /approve）。置き換え（S の
// 交代）は byDevice が承認済みか休眠の端末で、本人の全端末の device_keys の行の錠を識別子の順に取った後に状態を読み、承認済みと休眠の
// 全端末の user_sig が deviceSigs に揃い、全部が新しい公開部で第12章 5.3 の対象に対して通る時だけ（欠けるか通らなければ KEY-003、
// 知らない端末は KEY-001、同じ端末が 2 つは KEY-009、承認済みでない byDevice は KEY-004、JWK が P-256 の点でなければ KEY-009）。
// 置き換えで版を 1 増やし、古い版のコピーを消し、家族へ user.changed（signing_key）、本人へ keybundle.changed（5 つ全部）。
// 錠は device_keys（端末の識別子の順）、keybundles、user_signing_keys の順。再認証は呼ぶ側。
func (s *UserSigningKeys) Put(ctx context.Context, tx *store.Tx, userId, byDevice uuid.UUID, pub gen.CommonJwkP2561, deviceSigs []DeviceSig) (gen.Fusr1, error) {
	sec1, key, ok := jwkPoint(pub)
	if !ok {
		return gen.Fusr1{}, ev(cgen.KEY009)
	}
	jwk := canonicalJwk(sec1)
	v, err := s.Version(ctx, tx, userId)
	if err != nil {
		return gen.Fusr1{}, err
	}
	now := s.d.now()
	if v == 0 {
		if len(deviceSigs) != 0 {
			return gen.Fusr1{}, ev(cgen.KEY009)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO user_signing_keys (user_id, pub, version, created_at) VALUES ($1, $2, 1, $3) ON CONFLICT (user_id) DO NOTHING`,
			userId, jwk, now)
		if err != nil {
			return gen.Fusr1{}, wrap(err)
		}
		if tag.RowsAffected() == 1 {
			return gen.Fusr1{V: 1, UserId: userId.String(), UserSigPub: jwk, Version: 1, CreatedAt: now.UTC()}, nil
		}
		// 同時の初回が先に済んだ：置き換えとして続ける。
		if v, err = s.Version(ctx, tx, userId); err != nil {
			return gen.Fusr1{}, err
		}
	}
	by, err := s.d.Identity.Devices.Get(ctx, tx, byDevice)
	if err != nil {
		return gen.Fusr1{}, err
	}
	if by.UserId != userId || (by.State != identity.DeviceApproved && by.State != identity.DeviceDormant) {
		return gen.Fusr1{}, ev(cgen.KEY004)
	}
	if _, err := tx.Exec(ctx, `SELECT device_id FROM device_keys WHERE user_id = $1 ORDER BY device_id FOR UPDATE`, userId); err != nil {
		return gen.Fusr1{}, wrap(err)
	}
	devs, err := s.d.Identity.Devices.OfUser(ctx, tx, userId)
	if err != nil {
		return gen.Fusr1{}, err
	}
	keyed, err := s.devices.listOfUser(ctx, tx, userId)
	if err != nil {
		return gen.Fusr1{}, err
	}
	want := map[uuid.UUID]gen.Fdev1{}
	for _, dv := range devs {
		if dv.State != identity.DeviceApproved && dv.State != identity.DeviceDormant {
			continue
		}
		if f, ok := keyed[dv.Id]; ok {
			want[dv.Id] = f
		}
	}
	given := map[uuid.UUID][]byte{}
	for _, ds := range deviceSigs {
		if _, dup := given[ds.DeviceId]; dup {
			return gen.Fusr1{}, ev(cgen.KEY009)
		}
		if _, ok := want[ds.DeviceId]; !ok {
			return gen.Fusr1{}, ev(cgen.KEY001)
		}
		given[ds.DeviceId] = ds.UserSig
	}
	for id, f := range want {
		sig, ok := given[id]
		if !ok || !verifyFdev(f, sig, key) {
			return gen.Fusr1{}, ev(cgen.KEY003)
		}
	}
	for _, id := range sortedIds(mapKeys(want)) {
		if _, err := tx.Exec(ctx, `UPDATE device_keys SET user_sig = $2 WHERE device_id = $1`, id, given[id]); err != nil {
			return gen.Fusr1{}, wrap(err)
		}
	}
	if err := s.bundles.DeleteOlderVersions(ctx, tx, userId, v+1); err != nil {
		return gen.Fusr1{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE user_signing_keys SET pub = $2, version = version + 1, rotated_at = $3 WHERE user_id = $1 AND version = $4 AND revoked_at IS NULL`,
		userId, jwk, now, v)
	if err != nil {
		return gen.Fusr1{}, wrap(err)
	}
	if tag.RowsAffected() != 1 {
		// 版を読んだ後に同時の交代が先に済んだ（端末は新しい版を受け取ってから作り直す。第12章 13）。
		return gen.Fusr1{}, ev(cgen.KEY003)
	}
	family, err := s.d.Identity.Users.FamilyIds(ctx, tx)
	if err != nil {
		return gen.Fusr1{}, err
	}
	if err := s.d.emit(tx, family, "user.changed", uuid.Nil, uuid.Nil, userChanged{UserId: userId, Fields: []string{fieldSigningKey}}); err != nil {
		return gen.Fusr1{}, err
	}
	all := []BundleKind{BundleDevice, BundleRecoveryCode, BundleShare, BundleFamily, BundlePrf}
	if err := s.d.emit(tx, []uuid.UUID{userId}, "keybundle.changed", uuid.Nil, uuid.Nil, keybundleChanged{Kinds: all}); err != nil {
		return gen.Fusr1{}, err
	}
	f, err := s.Get(ctx, tx, userId)
	if err != nil {
		return gen.Fusr1{}, err
	}
	return *f, nil
}

// Get は fusr/1（版を含む）。無ければ nil。DeviceKeyReader.SigningKeys の元。q は ctx のトランザクションか Pool。
func (s *UserSigningKeys) Get(ctx context.Context, q store.Querier, userId uuid.UUID) (*gen.Fusr1, error) {
	var f gen.Fusr1
	var created time.Time
	var rotated *time.Time
	err := q.QueryRow(ctx, `SELECT pub, version, created_at, rotated_at FROM user_signing_keys WHERE user_id = $1 AND revoked_at IS NULL`, userId).
		Scan(&f.UserSigPub, &f.Version, &created, &rotated)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrap(err)
	}
	f.V, f.UserId, f.CreatedAt = 1, userId.String(), created.UTC()
	if rotated != nil {
		r := rotated.UTC()
		f.RotatedAt = &r
	}
	return &f, nil
}

// Version は今の S の版。行が無ければ 0。KeyBundles.Put と KeyBackup.Put が key_version と比べる（KEY-011）。
func (s *UserSigningKeys) Version(ctx context.Context, q store.Querier, userId uuid.UUID) (int, error) {
	var v int
	err := q.QueryRow(ctx, `SELECT version FROM user_signing_keys WHERE user_id = $1 AND revoked_at IS NULL`, userId).Scan(&v)
	if noRows(err) {
		return 0, nil
	}
	return v, wrap(err)
}

// revoke は抹消で本人の署名鍵を失効させる（revoked_at。OnUserErased）。
func (s *UserSigningKeys) revoke(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE user_signing_keys SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userId, s.d.now())
	return wrap(err)
}

// mapKeys は map の鍵の並び（順は呼ぶ側が決める）。
func mapKeys[V any](m map[uuid.UUID]V) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
