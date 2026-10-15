// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"

	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// KeyPackageCount は端末の KeyPackage の残り（GET /me/devices/{id}/keypackages）。
type KeyPackageCount struct {
	// Count は使い切りの KeyPackage の数（最後の手段を含まない）。
	Count int
	// LastResort は最後の手段があるか。
	LastResort bool
}

// Taken は取り出した KeyPackage（MLSMessage の符号化）。LastResort なら応答に KEY-006 の警告を添える（最後の手段は消さない）。
type Taken struct {
	Package    []byte
	LastResort bool
}

// KeyPackages は KeyPackage の保管と確かめを持つ（B-082。第12章 5.2）。
type KeyPackages struct {
	d         *Deps
	reader    *CommitReader
	devices   *DeviceKeys
	retention *store.Retention
}

// Push は POST /me/devices/{id}/keypackages。端末の device_keys の行の錠で同じ端末の Push を直列にし（無ければ KEY-001）、各要素を
// CommitReader.ParseKeyPackage で読み、第12章 5.2「KeyPackage の保管」の 9 つを確かめる（1 つでも通らなければ KEY-009 で全部を拒む）。
// 使い切りは最大 20（超えたら古い物から消す）、最後の手段は 1 つ（新しい物で置き換える）。期限は置いた時から 90 日と not_after の
// 1 日前の早い方。主キーは KeyPackageRef。戻りは期限の内の使い切りの数。
func (p *KeyPackages) Push(ctx context.Context, tx *store.Tx, deviceId uuid.UUID, packages [][]byte, lastResort []byte) (int, error) {
	reg, ok, err := readRegistration(ctx, tx, deviceId, true)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ev(cgen.KEY001)
	}
	now := p.d.now()
	type item struct {
		kp      ParsedKeyPackage
		raw     []byte
		expires time.Time
	}
	parse := func(raw []byte) (item, error) {
		kp, err := p.reader.ParseKeyPackage(raw)
		if err != nil {
			return item{}, err
		}
		if !checkKeyPackage(kp, reg, now, true) {
			return item{}, ev(cgen.KEY009)
		}
		exp := now.Add(keyPackageTtl)
		if na := time.Unix(int64(kp.Leaf.NotAfter), 0).Add(-24 * time.Hour); na.Before(exp) {
			exp = na
		}
		return item{kp: kp, raw: raw, expires: exp}, nil
	}
	items := make([]item, 0, len(packages))
	for _, raw := range packages {
		it, err := parse(raw)
		if err != nil {
			return 0, err
		}
		items = append(items, it)
	}
	var last *item
	if lastResort != nil {
		it, err := parse(lastResort)
		if err != nil {
			return 0, err
		}
		last = &it
	}
	for _, it := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO keypackages (ref, device_id, package, last_resort, expires_at, created_at) VALUES ($1, $2, $3, false, $4, $5)
ON CONFLICT (ref) DO NOTHING`, it.kp.Ref, deviceId, it.raw, it.expires, now); err != nil {
			return 0, wrap(err)
		}
	}
	if last != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM keypackages WHERE device_id = $1 AND last_resort AND ref <> $2`, deviceId, last.kp.Ref); err != nil {
			return 0, wrap(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO keypackages (ref, device_id, package, last_resort, expires_at, created_at) VALUES ($1, $2, $3, true, $4, $5)
ON CONFLICT (ref) DO UPDATE SET last_resort = true`, last.kp.Ref, deviceId, last.raw, last.expires, now); err != nil {
			return 0, wrap(err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM keypackages WHERE device_id = $1 AND NOT last_resort AND ref NOT IN
  (SELECT ref FROM keypackages WHERE device_id = $1 AND NOT last_resort ORDER BY created_at DESC, ref DESC LIMIT $2)`, deviceId, keyPackageMax); err != nil {
		return 0, wrap(err)
	}
	c, err := p.count(ctx, tx, deviceId, now)
	return c.Count, err
}

// Count は端末の残り（期限の内）。
func (p *KeyPackages) Count(ctx context.Context, deviceId uuid.UUID) (KeyPackageCount, error) {
	return p.count(ctx, p.d.Pool, deviceId, p.d.now())
}

func (p *KeyPackages) count(ctx context.Context, q store.Querier, deviceId uuid.UUID, now time.Time) (KeyPackageCount, error) {
	var c KeyPackageCount
	err := q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT last_resort), coalesce(bool_or(last_resort), false) FROM keypackages
WHERE device_id = $1 AND expires_at > $2`, deviceId, now).Scan(&c.Count, &c.LastResort)
	return c, wrap(err)
}

// Take は POST /users/{uid}/devices/{did}/keypackage。取り出しと削除を同じトランザクションで（I-11。FOR UPDATE SKIP LOCKED）。
// userId の構成員が無いか削除・抹消なら MEM-001。deviceId が userId の端末でないか、承認済みか休眠でなければ KEY-001。尽きたら最後の
// 手段（消さない。RFC 9420 16.8）。使い切りも最後の手段も無ければ KEY-001。
func (p *KeyPackages) Take(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) (Taken, error) {
	usr, err := p.d.Identity.Users.Get(ctx, tx, userId)
	if err != nil {
		return Taken{}, err
	}
	if usr.State == identity.UserDeleted || usr.State == identity.UserErased {
		return Taken{}, ev(cgen.MEM001)
	}
	dv, err := p.d.Identity.Devices.Get(ctx, tx, deviceId)
	if err != nil {
		return Taken{}, err
	}
	if dv.UserId != userId || (dv.State != identity.DeviceApproved && dv.State != identity.DeviceDormant) {
		return Taken{}, ev(cgen.KEY001)
	}
	now := p.d.now()
	var ref, pkg []byte
	err = tx.QueryRow(ctx, `SELECT ref, package FROM keypackages WHERE device_id = $1 AND NOT last_resort AND expires_at > $2
ORDER BY created_at, ref LIMIT 1 FOR UPDATE SKIP LOCKED`, deviceId, now).Scan(&ref, &pkg)
	if err == nil {
		if _, err := tx.Exec(ctx, `DELETE FROM keypackages WHERE ref = $1`, ref); err != nil {
			return Taken{}, wrap(err)
		}
		return Taken{Package: pkg}, nil
	}
	if !noRows(err) {
		return Taken{}, wrap(err)
	}
	err = tx.QueryRow(ctx, `SELECT package FROM keypackages WHERE device_id = $1 AND last_resort AND expires_at > $2`, deviceId, now).Scan(&pkg)
	if noRows(err) {
		return Taken{}, ev(cgen.KEY001)
	}
	if err != nil {
		return Taken{}, wrap(err)
	}
	return Taken{Package: pkg, LastResort: true}, nil
}

// PruneExpired は期限を過ぎた KeyPackage を消す（srv-worker。srv-store Retention で 1 万行ずつ）。戻りは消した数。
func (p *KeyPackages) PruneExpired(ctx context.Context) (int64, error) {
	return p.retention.DeleteOlder(ctx, "keypackages", "expires_at", p.d.now())
}

// DeleteAll は端末の KeyPackage を全部消す（hook OnDeviceRevoked）。
func (p *KeyPackages) DeleteAll(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM keypackages WHERE device_id = $1`, deviceId)
	return wrap(err)
}
