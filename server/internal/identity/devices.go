// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"net/netip"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/envelope"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Device は端末の 1 行（devices）。名前と最後の IP は本人の DEK のエンベロープ。
type Device struct {
	Id         uuid.UUID
	UserId     uuid.UUID
	Name       envelope.Fenv
	Kind       Platform
	State      DeviceState
	LastSeenAt time.Time
	LastIp     *envelope.Fenv
	CreatedAt  time.Time
	// FirstApprovedAt は最初の承認の時刻（停止で未承認に戻しても消さない。無ければ承認されたことのない端末。第3章 5.1）。
	FirstApprovedAt *time.Time
}

// Devices は端末を持つ（B-067。第3章 5）。
type Devices struct {
	d        *Deps
	hooks    *Hooks
	sessions *Sessions
	ipLog    *IpLog
	keys     userKeys
}

// badDeviceName は端末の名前の誤り（1〜40 書記素。第3章 5.2）。要求の形の誤りとして NET-001。
func badDeviceName(name string) *fault.Event {
	if checkName(name, deviceNameMax) == nil {
		return nil
	}
	return fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": "body/device_name", "message": "1 から 40 文字"}}})
}

// Register は端末を未承認で作る（ログイン、参加、設置、QR の成功、再設定のリンクで）。名前は本人の DEK で封じる。
// 続けて本人へ new_device、子ども区分なら保護者にも（第3章 5.2、第7章 9。登録の経路の全部がここを通る）。
// device.changed（added）は呼ぶ側が出す。
func (v *Devices) Register(ctx context.Context, tx *store.Tx, userId uuid.UUID, kind Platform, name string) (Device, error) {
	if e := badDeviceName(name); e != nil {
		return Device{}, e
	}
	var userName string
	var child bool
	if err := tx.QueryRow(ctx, `SELECT display_name, is_child FROM users WHERE id = $1`, userId).Scan(&userName, &child); err != nil {
		return Device{}, wrap(err)
	}
	now := v.d.now()
	dv := Device{Id: v.d.Ids.NewV7(), UserId: userId, Kind: kind, State: DeviceUnapproved, LastSeenAt: now, CreatedAt: now}
	blob, err := v.keys.seal(ctx, tx, userId, []byte(name), aad("devices", "name", dv.Id.String()))
	if err != nil {
		return Device{}, err
	}
	if dv.Name, err = envelope.DecodeFenv(blob); err != nil {
		return Device{}, wrap(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO devices (id, user_id, name, kind, state, last_seen_at, created_at) VALUES ($1, $2, $3, $4, 'unapproved', $5, $5)`,
		dv.Id, userId, blob, string(kind), now); err != nil {
		return Device{}, wrap(err)
	}
	params := map[string]any{"user_id": userId.String(), "name": userName, "device_id": dv.Id.String(), "device_name": name, "platform": string(kind)}
	if err := v.hooks.notifySelf(ctx, tx, userId, noticeNewDevice, noticeKey(dv.Id.String()), params); err != nil {
		return Device{}, err
	}
	if child {
		if err := v.hooks.notifyGuardians(ctx, tx, userId, noticeNewDevice, noticeKey(dv.Id.String()), params); err != nil {
			return Device{}, err
		}
	}
	return dv, nil
}

const deviceCols = `id, user_id, name, kind, state, last_seen_at, last_ip, created_at, first_approved_at`

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var dv Device
	var name, ip []byte
	var kind, state string
	if err := row.Scan(&dv.Id, &dv.UserId, &name, &kind, &state, &dv.LastSeenAt, &ip, &dv.CreatedAt, &dv.FirstApprovedAt); err != nil {
		return Device{}, err
	}
	dv.Kind, dv.State, dv.LastSeenAt, dv.CreatedAt = Platform(kind), DeviceState(state), dv.LastSeenAt.UTC(), dv.CreatedAt.UTC()
	if dv.FirstApprovedAt != nil {
		t := dv.FirstApprovedAt.UTC()
		dv.FirstApprovedAt = &t
	}
	var err error
	if dv.Name, err = envelope.DecodeFenv(name); err != nil {
		return Device{}, err
	}
	if ip != nil {
		f, err := envelope.DecodeFenv(ip)
		if err != nil {
			return Device{}, err
		}
		dv.LastIp = &f
	}
	return dv, nil
}

// Get は端末を返す。無ければ KEY-001。q は ctx のトランザクションか Pool（トランザクションの中ではそれを渡す。全体実装設計書 2.4）。
func (v *Devices) Get(ctx context.Context, q store.Querier, deviceId uuid.UUID) (Device, error) {
	return v.get(ctx, q, deviceId, false)
}

// LockShared は端末を共有の錠（FOR SHARE）で読む。srv-keys が端末を MLS のグループに加える前（Groups.Commit の Add、RequestAdd、
// Reinit）に、トークの錠より先に取る。取り消し（行の FOR UPDATE）と直列になり、取り消しの hook が新しい leaf と待ちを見落とさない
// （I-8）。無ければ KEY-001。
func (v *Devices) LockShared(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) (Device, error) {
	dv, err := scanDevice(tx.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = $1 FOR SHARE`, deviceId))
	if noRows(err) {
		return Device{}, ev(cgen.KEY001)
	}
	return dv, wrap(err)
}

func (v *Devices) get(ctx context.Context, q querier, deviceId uuid.UUID, lock bool) (Device, error) {
	sql := `SELECT ` + deviceCols + ` FROM devices WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	dv, err := scanDevice(q.QueryRow(ctx, sql, deviceId))
	if noRows(err) {
		return Device{}, ev(cgen.KEY001)
	}
	return dv, wrap(err)
}

// mine は本人の、取り消し・期限切れでない端末（違えば KEY-001。device_self の規則と同じく有無を漏らさない）。
func (v *Devices) mine(ctx context.Context, q querier, userId, deviceId uuid.UUID, lock bool) (Device, error) {
	dv, err := v.get(ctx, q, deviceId, lock)
	if err != nil {
		return Device{}, err
	}
	if dv.UserId != userId || dv.State == DeviceRevoked || dv.State == DeviceExpired {
		return Device{}, ev(cgen.KEY001)
	}
	return dv, nil
}

// List は GET /me/devices（名前、種類、最終の利用、生の IP、承認、購読、この端末、鍵の登録 keys）。取り消し済みと期限切れは除く。
// 購読は PushReader.SubscribedDevices、keys は DeviceKeyReader.KeysOf（登録の無い端末は欄を出さない）。
// 指紋は求めない（端末が keys から第12章 5.3 の定義で求める）。q は ctx のトランザクションか Pool（/sync の初回の一覧は
// srv-store Pool.BeginSnapshot の中で読む）。
func (v *Devices) List(ctx context.Context, q store.Querier, userId, thisDevice uuid.UUID, page Page) ([]gen.Device, bool, error) {
	c, err := page.uuids()
	if err != nil {
		return nil, false, err
	}
	sql, args := whereUUID(c, `SELECT `+deviceCols+` FROM devices WHERE user_id = $1 AND state NOT IN ('revoked', 'expired')`, "id", []any{userId})
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, wrap(err)
	}
	var list []Device
	for rows.Next() {
		dv, err := scanDevice(rows)
		if err != nil {
			rows.Close()
			return nil, false, wrap(err)
		}
		list = append(list, dv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	list, more := finish(list, c)
	out, err := v.views(ctx, q, userId, thisDevice, list)
	return out, more, err
}

// views は端末の行を応答の形にする（名前と IP を開き、購読と鍵を重ねる）。
func (v *Devices) views(ctx context.Context, q store.Querier, userId, thisDevice uuid.UUID, list []Device) ([]gen.Device, error) {
	subs, err := v.d.Push.SubscribedDevices(ctx, q, userId)
	if err != nil {
		return nil, wrap(err)
	}
	subscribed := map[uuid.UUID]bool{}
	for _, s := range subs {
		subscribed[s] = true
	}
	keys, err := v.d.Keys.KeysOf(ctx, q, userId)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]gen.Device, 0, len(list))
	err = v.keys.with(ctx, q, userId, func(dek []byte, dekId uuid.UUID) error {
		for _, dv := range list {
			name, err := openWith(v.d, dek, dekId, dv.Name.Encode(), aad("devices", "name", dv.Id.String()))
			if err != nil {
				return err
			}
			seen := dv.LastSeenAt
			g := gen.Device{Id: dv.Id.String(), Name: string(name), Platform: gen.DevicePlatform(dv.Kind), State: gen.DeviceState(dv.State),
				LastSeenAt: &seen, Push: subscribed[dv.Id], ThisDevice: dv.Id == thisDevice}
			if dv.LastIp != nil {
				ip, err := openWith(v.d, dek, dekId, dv.LastIp.Encode(), aad("devices", "last_ip", dv.Id.String()))
				if err != nil {
					return err
				}
				s := string(ip)
				g.LastIp = &s
			}
			if k, ok := keys[dv.Id]; ok {
				g.Keys = &k
			}
			out = append(out, g)
		}
		return nil
	})
	return out, err
}

// view は 1 台の応答の形（PATCH /me/devices/{id} の応答）。
func (v *Devices) view(ctx context.Context, userId, deviceId, thisDevice uuid.UUID) (gen.Device, error) {
	dv, err := v.mine(ctx, v.d.Pool, userId, deviceId, false)
	if err != nil {
		return gen.Device{}, err
	}
	list, err := v.views(ctx, v.d.Pool, userId, thisDevice, []Device{dv})
	if err != nil {
		return gen.Device{}, err
	}
	return list[0], nil
}

// Rename は PATCH /me/devices/{id}。本人の物でなければ KEY-001。device.changed（renamed。家族の全員）。
func (v *Devices) Rename(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, name string) error {
	if e := badDeviceName(name); e != nil {
		return e
	}
	if _, err := v.mine(ctx, tx, userId, deviceId, true); err != nil {
		return err
	}
	blob, err := v.keys.seal(ctx, tx, userId, []byte(name), aad("devices", "name", deviceId.String()))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET name = $2 WHERE id = $1`, deviceId, blob); err != nil {
		return wrap(err)
	}
	by := userId
	return v.d.emitDeviceChanged(ctx, tx, userId, deviceId, changeRenamed, &by)
}

// Approve は未承認を承認済みにする。srv-keys が fdev/1 の user_sig を置いた同じトランザクションで呼ぶ。
// 承認済みと休眠は KEY-002（休眠は承認が要らず rejoin の経路）、取り消し済み・期限切れ・無しは KEY-001。first_approved_at が
// 無ければ今を置く（最初の承認の記録。停止で未承認に戻しても消さない。期限切れと休眠の対象の判定に使う）。device.changed。
func (v *Devices) Approve(ctx context.Context, tx *store.Tx, deviceId, by uuid.UUID) error {
	dv, err := v.get(ctx, tx, deviceId, true)
	if err != nil {
		return err
	}
	switch dv.State {
	case DeviceUnapproved:
	case DeviceApproved, DeviceDormant:
		return ev(cgen.KEY002)
	default:
		return ev(cgen.KEY001)
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET state = 'approved', first_approved_at = coalesce(first_approved_at, $2) WHERE id = $1`,
		deviceId, v.d.now()); err != nil {
		return wrap(err)
	}
	return v.d.emitDeviceChanged(ctx, tx, dv.UserId, deviceId, changeApproved, &by)
}

// UnapproveAllOfUser は停止で、承認済みと休眠の端末を未承認に戻す（取り消しではない。hook は呼ばない。第3章 8.1）。
// device.changed は出さない（change の列挙に当たる値が無く、本人のセッションは同じトランザクションで失効し、家族は user.changed で知る）。
func (v *Devices) UnapproveAllOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE devices SET state = 'unapproved' WHERE user_id = $1 AND state IN ('approved', 'dormant')`, userId)
	return wrap(err)
}

// Revoke は状態を revoked にし、Sessions.RevokeAllOfDevice(device_revoked)、hook OnDeviceRevoked、device.changed
// （家族の全員）を行う。既に取り消し済み・期限切れなら何もしない。self は自分の端末（再認証なし、応答で Cookie を消す。第3章 4.5）。
func (v *Devices) Revoke(ctx context.Context, tx *store.Tx, deviceId, by uuid.UUID, self bool) error {
	dv, err := v.get(ctx, tx, deviceId, true)
	if err != nil {
		return err
	}
	if dv.State == DeviceRevoked || dv.State == DeviceExpired {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET state = 'revoked', revoked_at = $2, revoked_by = $3 WHERE id = $1`, deviceId, v.d.now(), by); err != nil {
		return wrap(err)
	}
	if err := v.sessions.RevokeAllOfDevice(ctx, tx, deviceId, RevokeDeviceRevoked); err != nil {
		return err
	}
	if err := v.hooks.onDeviceRevoked(ctx, tx, deviceId); err != nil {
		return err
	}
	return v.d.emitDeviceChanged(ctx, tx, dv.UserId, deviceId, changeRevoked, &by)
}

// Touch は最終の利用と最後の IP（本人の DEK）を書き、ip_log に記録する。前の書き込みから 1 分以内なら書かない。
// ip が空の値（要求の外）なら IP は書かない。
func (v *Devices) Touch(ctx context.Context, tx *store.Tx, deviceId uuid.UUID, ip netip.Addr, now time.Time) error {
	var userId uuid.UUID
	var last time.Time
	if err := tx.QueryRow(ctx, `SELECT user_id, last_seen_at FROM devices WHERE id = $1`, deviceId).Scan(&userId, &last); err != nil {
		return wrap(err)
	}
	if now.Sub(last) < touchEvery {
		return nil
	}
	if !ip.IsValid() {
		_, err := tx.Exec(ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, deviceId, now)
		return wrap(err)
	}
	blob, err := v.keys.seal(ctx, tx, userId, []byte(ip.String()), aad("devices", "last_ip", deviceId.String()))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET last_seen_at = $2, last_ip = $3 WHERE id = $1`, deviceId, now, blob); err != nil {
		return wrap(err)
	}
	return v.ipLog.record(ctx, tx, userId, deviceId, ip, now)
}

// ExpireUnapproved は承認されたことのない未承認の端末（first_approved_at が無い）のうち、作成が olderThan より古い物を期限切れに
// する（7 日。srv-worker）。停止で未承認に戻った端末（first_approved_at あり）は対象にしない（再開の後の自己承認を待つ。第3章 5.1、
// 8.1）。そのセッションを失効（expired）させ、
// hook OnDeviceRevoked で他のブロックの記録（鍵の登録、メールボックス、購読）を消す。自分のトランザクションで 1,000 台ずつ。
func (v *Devices) ExpireUnapproved(ctx context.Context, olderThan time.Time) (int64, error) {
	var total int64
	for {
		n := 0
		err := v.d.separateTx(ctx, func(tx *store.Tx) error {
			list, err := uuids(ctx, tx, `SELECT id FROM devices WHERE state = 'unapproved' AND first_approved_at IS NULL AND created_at < $1
				ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED`,
				olderThan, expireBatch)
			if err != nil {
				return err
			}
			for _, id := range list {
				if _, err := tx.Exec(ctx, `UPDATE devices SET state = 'expired' WHERE id = $1`, id); err != nil {
					return wrap(err)
				}
				if err := v.sessions.RevokeAllOfDevice(ctx, tx, id, RevokeExpired); err != nil {
					return err
				}
				if err := v.hooks.onDeviceRevoked(ctx, tx, id); err != nil {
					return err
				}
			}
			n = len(list)
			return nil
		})
		if err != nil {
			return total, err
		}
		total += int64(n)
		if n < expireBatch {
			return total, nil
		}
	}
}

// MarkDormant は srv-keys Groups.RetireDormant から。devices の行の錠を取り、休眠の扱い（leaf とコピー (a) を外す）をするかを返す。
// last_seen_at が olderThan より古い時だけ真：承認済みなら休眠にする。承認されたことのある未承認の端末（停止で戻った端末）なら
// 状態を変えない（休眠にすると再開の後に鍵の承認を経ずに承認済みと同じ扱いになる。第12章 5.2）。他の状態と、last_seen_at が
// 新しい時（休眠の判定の後に使われた端末）は偽。
func (v *Devices) MarkDormant(ctx context.Context, tx *store.Tx, deviceId uuid.UUID, olderThan time.Time) (bool, error) {
	dv, err := v.get(ctx, tx, deviceId, true)
	if err != nil {
		return false, err
	}
	if !dv.LastSeenAt.Before(olderThan) {
		return false, nil
	}
	switch {
	case dv.State == DeviceApproved:
		if _, err := tx.Exec(ctx, `UPDATE devices SET state = 'dormant' WHERE id = $1`, deviceId); err != nil {
			return false, wrap(err)
		}
		return true, nil
	case dv.State == DeviceUnapproved && dv.FirstApprovedAt != nil:
		return true, nil
	}
	return false, nil
}

// MarkActive は srv-keys Groups.Commit から。休眠の端末が Add で leaf を得た時に承認済みへ戻す（休眠の時だけ。他の状態は何もしない。
// 第3章 5.1）。device.changed は出さない（change の列挙に当たる値が無く、家族は room.member の added で知る）。
func (v *Devices) MarkActive(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE devices SET state = 'approved' WHERE id = $1 AND state = 'dormant'`, deviceId)
	return wrap(err)
}

// GuardianCleared は保護者が承認した端末か（devices.guardian_approved_at がある。第3章 4.4）。srv-keys DeviceKeys.Approve が
// 子の端末で確かめる。端末が無ければ偽。
func (v *Devices) GuardianCleared(ctx context.Context, q store.Querier, deviceId uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT guardian_approved_at IS NOT NULL FROM devices WHERE id = $1`, deviceId).Scan(&ok)
	if noRows(err) {
		return false, nil
	}
	return ok, wrap(err)
}

// MarkGuardianCleared は devices.guardian_approved_at を置く（既にあれば変えない）。QrFlow.Poll（保護者が承認した求めから登録した時）と
// Guardians.Approve（対象が端末の識別子の時）から。
func (v *Devices) MarkGuardianCleared(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE devices SET guardian_approved_at = $2 WHERE id = $1 AND guardian_approved_at IS NULL`, deviceId, v.d.now())
	return wrap(err)
}

// OfUser は構成員の全端末（全状態。識別子の順）。srv-keys の承認と鍵の一覧（DevicesOf、KeysOf）、削除の hook の leaf の削除待ち。
func (v *Devices) OfUser(ctx context.Context, q store.Querier, userId uuid.UUID) ([]Device, error) {
	return v.scanAll(ctx, q, `SELECT `+deviceCols+` FROM devices WHERE user_id = $1 ORDER BY id`, userId)
}

// NameOf は端末の名前（本人の DEK で開く）。srv-keys の recovery.request の device_name と知らせの params。端末が無ければ KEY-001。
// q は ctx のトランザクションか Pool。
func (v *Devices) NameOf(ctx context.Context, q store.Querier, deviceId uuid.UUID) (string, error) {
	dv, err := v.get(ctx, q, deviceId, false)
	if err != nil {
		return "", err
	}
	name, err := v.keys.open(ctx, q, dv.UserId, dv.Name.Encode(), aad("devices", "name", dv.Id.String()))
	if err != nil {
		return "", err
	}
	return string(name), nil
}

// IdleSeenBefore は last_seen_at が olderThan より古い端末のうち、承認済みの物と、承認されたことのある未承認の物（停止で戻った
// 端末）。識別子の順（休眠の判定。srv-keys Groups.DormantLeafs。第12章 5.2）。
func (v *Devices) IdleSeenBefore(ctx context.Context, olderThan time.Time) ([]Device, error) {
	return v.scanAll(ctx, v.d.Pool, `SELECT `+deviceCols+` FROM devices
		WHERE (state = 'approved' OR (state = 'unapproved' AND first_approved_at IS NOT NULL)) AND last_seen_at < $1 ORDER BY id`, olderThan)
}

// ApprovedAmong は deviceIds のうち状態が承認済みの端末（識別子の順）。srv-keys Groups.Epoch と Reinit の「Commit を作れる端末」の
// 判定（第12章 5.2 の (d)）。q は ctx のトランザクションか Pool（Epoch は srv-store Pool.BeginSnapshot の中で読む）。
func (v *Devices) ApprovedAmong(ctx context.Context, q store.Querier, deviceIds []uuid.UUID) ([]uuid.UUID, error) {
	if len(deviceIds) == 0 {
		return []uuid.UUID{}, nil
	}
	return uuids(ctx, q, `SELECT id FROM devices WHERE id = ANY($1) AND state = 'approved' ORDER BY id`, deviceIds)
}

// scanAll は端末の行の並びを読む。
func (v *Devices) scanAll(ctx context.Context, q store.Querier, sql string, args ...any) ([]Device, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		dv, err := scanDevice(rows)
		if err != nil {
			return nil, wrap(err)
		}
		out = append(out, dv)
	}
	return out, wrap(rows.Err())
}
