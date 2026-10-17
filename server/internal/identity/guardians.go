// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// ApprovalKind は保護者の承認の求めの種類（guardian_approvals.kind）。
type ApprovalKind string

// 承認の求めの 2 つ。期限は erase 7 日、device 10 分（第3章 8.3、9.2）。
const (
	ApprovalErase  ApprovalKind = "erase"
	ApprovalDevice ApprovalKind = "device"
)

// askAgeEvery は申告だけの子の年齢を保護者に問う間隔（第3章 9.1）。
const askAgeEvery = 2 * 365 * 24 * time.Hour

// ApprovalTarget は端末の追加の求めの対象（第3章 4.4）。QR の承認なら CodeSha256（端末のコードのハッシュ。device_codes の主キー）、
// 鍵の承認（srv-keys DeviceKeys.Approve）なら DeviceId のどちらか 1 つ。抹消の求めは空。
type ApprovalTarget struct {
	CodeSha256 []byte
	DeviceId   *uuid.UUID
}

// Guardians は子どもと保護者を持つ（B-062。第3章 9）。
type Guardians struct {
	d       *Deps
	hooks   *Hooks
	users   *Users
	qr      *QrFlow
	devices *Devices
	keys    userKeys
}

// Set は子の保護者の組を置き直す（PUT /admin/users/{id}/guardians。srv-admin から）。子が無い・子ども区分でなければ
// MEM-001。保護者は状態が通常の構成員で子以外（違えば MEM-001）。子と新旧の保護者へ guardians_changed
// （guardian_names は新しい保護者の表示名を渡された順に）。
func (g *Guardians) Set(ctx context.Context, tx *store.Tx, childId uuid.UUID, guardianIds []uuid.UUID, by uuid.UUID) error {
	child, err := g.users.get(ctx, tx, childId, true)
	if err != nil {
		return err
	}
	if !child.IsChild {
		return ev(cgen.MEM001)
	}
	names := []string{}
	named := map[uuid.UUID]bool{}
	for _, gid := range guardianIds {
		if gid == childId {
			return ev(cgen.MEM001)
		}
		usr, err := g.users.get(ctx, tx, gid, false)
		if err != nil {
			return err
		}
		if usr.State != UserNormal {
			return ev(cgen.MEM001)
		}
		if !named[gid] {
			named[gid] = true
			names = append(names, usr.DisplayName)
		}
	}
	old, err := uuids(ctx, tx, `SELECT guardian_id FROM guardians WHERE child_id = $1`, childId)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM guardians WHERE child_id = $1`, childId); err != nil {
		return wrap(err)
	}
	now := g.d.now()
	for _, gid := range guardianIds {
		if _, err := tx.Exec(ctx, `INSERT INTO guardians (child_id, guardian_id, set_by, set_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
			childId, gid, by, now); err != nil {
			return wrap(err)
		}
	}
	params := map[string]any{"child_id": childId.String(), "child_name": child.DisplayName, "guardian_names": names}
	to := append(append([]uuid.UUID{childId}, old...), guardianIds...)
	return g.hooks.notifyEach(ctx, tx, to, noticeGuardiansChanged, noticeKey(childId.String()), params)
}

// Of は子の保護者を返す。
func (g *Guardians) Of(ctx context.Context, childId uuid.UUID) ([]uuid.UUID, error) {
	return uuids(ctx, g.d.Pool, `SELECT guardian_id FROM guardians WHERE child_id = $1 ORDER BY guardian_id`, childId)
}

// IsGuardianOf は userId が childId の保護者か。
func (g *Guardians) IsGuardianOf(ctx context.Context, userId, childId uuid.UUID) (bool, error) {
	var ok bool
	err := g.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM guardians WHERE child_id = $1 AND guardian_id = $2)`, childId, userId).Scan(&ok)
	return ok, wrap(err)
}

// authorize は規則 guardian の判定（本ブロックが判定する。srv-boundary の authz.go の Rule は「ハンドラが資源を読んでから確かめる」）。
// 経路の子が子ども区分でない、または本人がその子の保護者でなければ MEM-001（有無を漏らさない）。
func (g *Guardians) authorize(ctx context.Context, userId, childId uuid.UUID) error {
	var ok bool
	err := g.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM guardians gd JOIN users u ON u.id = gd.child_id
WHERE gd.child_id = $1 AND gd.guardian_id = $2 AND u.is_child)`, childId, userId).Scan(&ok)
	if err != nil {
		return wrap(err)
	}
	if !ok {
		return ev(cgen.MEM001)
	}
	return nil
}

// Settings は子の制限（child_controls の 1 行。無ければ ft.settings.guardian/1 の既定。生成の gen.Items）。
func (g *Guardians) Settings(ctx context.Context, q store.Querier, childId uuid.UUID) (gen.FtSettingsGuardian1, error) {
	return g.settings(ctx, q, childId)
}

func (g *Guardians) settings(ctx context.Context, q querier, childId uuid.UUID) (gen.FtSettingsGuardian1, error) {
	merged := settingsDefaults("guardian.")
	var raw []byte
	err := q.QueryRow(ctx, `SELECT settings FROM child_controls WHERE child_id = $1`, childId).Scan(&raw)
	if err != nil && !noRows(err) {
		return gen.FtSettingsGuardian1{}, wrap(err)
	}
	if raw != nil {
		stored := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &stored); err != nil {
			return gen.FtSettingsGuardian1{}, wrap(err)
		}
		for k, v := range stored {
			merged[k] = v
		}
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return gen.FtSettingsGuardian1{}, wrap(err)
	}
	var out gen.FtSettingsGuardian1
	return out, wrap(json.Unmarshal(b, &out))
}

// PutSettings は PUT /me/guardian/children/{id}/controls。保護者が何人でも 1 行。与えた項目だけを置き換え、
// 子と書いた人以外の保護者へ guardian_settings_changed。
func (g *Guardians) PutSettings(ctx context.Context, tx *store.Tx, childId, by uuid.UUID, s gen.FtSettingsGuardian1) error {
	if err := g.write(ctx, tx, childId, by, s); err != nil {
		return err
	}
	child, err := g.users.get(ctx, tx, childId, false)
	if err != nil {
		return err
	}
	writer, err := g.users.get(ctx, tx, by, false)
	if err != nil {
		return err
	}
	others, err := uuids(ctx, tx, `SELECT guardian_id FROM guardians WHERE child_id = $1 AND guardian_id <> $2 ORDER BY guardian_id`, childId, by)
	if err != nil {
		return err
	}
	params := map[string]any{"child_id": childId.String(), "child_name": child.DisplayName, "by_user_id": by.String(), "by_name": writer.DisplayName}
	return g.hooks.notifyEach(ctx, tx, append([]uuid.UUID{childId}, others...), noticeGuardianSettings, noticeKey(childId.String(), by.String()), params)
}

// write は与えた項目を今の値に重ねて child_controls に置く（upsert）。
func (g *Guardians) write(ctx context.Context, tx *store.Tx, childId, by uuid.UUID, s gen.FtSettingsGuardian1) error {
	cur, err := g.settings(ctx, tx, childId)
	if err != nil {
		return err
	}
	if s.QuietHours != nil {
		cur.QuietHours = s.QuietHours
	}
	if s.LinkConfirm != nil {
		cur.LinkConfirm = s.LinkConfirm
	}
	if s.DeviceApproval != nil {
		cur.DeviceApproval = s.DeviceApproval
	}
	if s.AllowInvites != nil {
		cur.AllowInvites = s.AllowInvites
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return wrap(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO child_controls (child_id, settings, updated_by, updated_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (child_id) DO UPDATE SET settings = EXCLUDED.settings, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		childId, b, by, g.d.now())
	return wrap(err)
}

// relax は 16 歳の解除（利用の時間帯と外部リンクの確認を外す。第3章 9.1）。Users.applyAge から。updated_by は子
// （保護者の設定の変更と読まれない）。
func (g *Guardians) relax(ctx context.Context, tx *store.Tx, childId uuid.UUID) error {
	empty, off := "", false
	return g.write(ctx, tx, childId, childId, gen.FtSettingsGuardian1{QuietHours: &empty, LinkConfirm: &off})
}

// RequestApproval は guardian_approvals に求めを置き、保護者に知らせる。target は device の時だけで、QR の承認なら端末のコードの
// ハッシュ、鍵の承認なら端末の識別子（ApprovalTarget のどちらか 1 つ。第3章 4.4）。
// 同じ子・同じ種類・同じ対象の open で期限内の求めがあれば、期限を延ばさずそれを返し、知らせは出さない（同じ要求の再送で 1 つ）。
func (g *Guardians) RequestApproval(ctx context.Context, tx *store.Tx, childId uuid.UUID, kind ApprovalKind, target ApprovalTarget) (uuid.UUID, error) {
	now := g.d.now()
	var hash []byte
	var device *uuid.UUID
	if kind == ApprovalDevice {
		hash, device = target.CodeSha256, target.DeviceId
		if (hash == nil) == (device == nil) {
			return uuid.Nil, fault.Wrap(errors.New("identity: 端末の追加の求めの対象はコードか端末のどちらか 1 つ"))
		}
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM guardian_approvals WHERE child_id = $1 AND kind = $2 AND state = 'open' AND expires_at > $3
  AND (target_sha256 IS NOT DISTINCT FROM $4) AND (target_device_id IS NOT DISTINCT FROM $5) ORDER BY created_at DESC LIMIT 1`,
		childId, string(kind), now, hash, device).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !noRows(err) {
		return uuid.Nil, wrap(err)
	}
	ttl := approvalEraseTtl
	if kind == ApprovalDevice {
		ttl = approvalDeviceTtl
	}
	id = g.d.Ids.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO guardian_approvals (id, child_id, kind, target_sha256, target_device_id, state, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, 'open', $6, $7)`, id, childId, string(kind), hash, device, now, now.Add(ttl)); err != nil {
		return uuid.Nil, wrap(err)
	}
	child, err := g.users.get(ctx, tx, childId, false)
	if err != nil {
		return uuid.Nil, err
	}
	params := map[string]any{"approval_id": id.String(), "child_id": childId.String(), "child_name": child.DisplayName,
		"expires_at": now.Add(ttl).Format(time.RFC3339)}
	notice := noticeEraseApprovalRequested
	if kind == ApprovalDevice {
		notice = noticeDeviceApprovalRequested
		name, platform, err := g.targetDevice(ctx, tx, childId, hash, device)
		if err != nil {
			return uuid.Nil, err
		}
		params["device_name"], params["platform"] = name, platform
	}
	return id, g.hooks.notifyGuardians(ctx, tx, childId, notice, noticeKey(id.String()), params)
}

// targetDevice は求めの対象の端末の名前と種類（QR の承認は device_codes の行、鍵の承認は devices の行を子の DEK で開く）。
func (g *Guardians) targetDevice(ctx context.Context, tx *store.Tx, childId uuid.UUID, hash []byte, device *uuid.UUID) (string, string, error) {
	var name, platform string
	if device == nil {
		err := tx.QueryRow(ctx, `SELECT name, kind FROM device_codes WHERE device_code_sha256 = $1`, hash).Scan(&name, &platform)
		return name, platform, wrap(err)
	}
	dv, err := g.devices.get(ctx, tx, *device, false)
	if err != nil {
		return "", "", err
	}
	if dv.UserId != childId {
		return "", "", fault.Wrap(errors.New("identity: 求めの端末が子の物でない"))
	}
	plain, err := g.keys.open(ctx, tx, childId, dv.Name.Encode(), aad("devices", "name", dv.Id.String()))
	if err != nil {
		return "", "", err
	}
	return string(plain), string(dv.Kind), nil
}

// SetBirthday は PUT /me/guardian/children/{id}/birthday（第3章 9.1）。保護者であることは呼ぶ側が authorize で確かめる。
// 年は必須で、未来の日付は不可（MEM-005）。user_private.birthday に書き、その誕生日で今日までに迎えた 16 歳・18 歳の解除を同じトランザクションで
// 適用する（Users.applyAge）。
func (g *Guardians) SetBirthday(ctx context.Context, tx *store.Tx, childId uuid.UUID, s string) error {
	b, e := parseBirthday(s)
	if e != nil {
		return e
	}
	if b.Year == 0 || b.ageOn(g.d.now()) < 0 {
		return ev(cgen.MEM005)
	}
	if err := g.users.writeBirthday(ctx, tx, childId, b); err != nil {
		return err
	}
	_, err := g.users.applyAge(ctx, tx, childId, b, g.d.now())
	return err
}

// Approve は POST /me/guardian/approvals/{id}（G-17）。子の保護者だけ（違えば、または求めが無ければ MEM-001）。
// 期限切れは AUTH-007（失効の書き込みと知らせは ExpireDue）。erase なら 24 時間の猶予を始める。device で対象が端末のコードなら
// QR の承認を続け、対象が端末の識別子なら「保護者が承認した端末」にして device.changed（guardian_approved。子の端末が鍵の承認を
// 続ける合図。第3章 4.4）。承認済みの求めをもう一度承認しても何もしない（冪等）。
func (g *Guardians) Approve(ctx context.Context, tx *store.Tx, guardianId, approvalId uuid.UUID) error {
	child, err := g.lockFor(ctx, tx, guardianId, approvalId)
	if err != nil {
		return err
	}
	var kind, state string
	var exp time.Time
	var target []byte
	var device *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT kind, state, expires_at, target_sha256, target_device_id FROM guardian_approvals WHERE id = $1 FOR UPDATE`,
		approvalId).Scan(&kind, &state, &exp, &target, &device); err != nil {
		return wrap(err)
	}
	switch {
	case state == "approved":
		return nil
	case state != "open":
		return ev(cgen.AUTH007)
	case !exp.After(g.d.now()):
		// 失効の書き込みと知らせは ExpireDue の 1 か所（この行の錠を持ったまま別のトランザクションで書くと待ち合って止まる）。
		return ev(cgen.AUTH007)
	}
	if _, err := tx.Exec(ctx, `UPDATE guardian_approvals SET state = 'approved', approved_by = $2 WHERE id = $1`, approvalId, guardianId); err != nil {
		return wrap(err)
	}
	switch ApprovalKind(kind) {
	case ApprovalErase:
		usr, err := g.users.get(ctx, tx, child, true)
		if err != nil {
			return err
		}
		if usr.State != UserNormal {
			return nil
		}
		_, err = g.users.startErase(ctx, tx, child)
		return err
	default:
		if device != nil {
			return g.clearDevice(ctx, tx, child, *device, guardianId)
		}
		return g.qr.approveFor(ctx, tx, child, target)
	}
}

// clearDevice は鍵の承認の求めを保護者が通した時の続き。端末が取り消し済み・期限切れなら何もしない（求めだけが承認済みになる）。
func (g *Guardians) clearDevice(ctx context.Context, tx *store.Tx, childId, deviceId, guardianId uuid.UUID) error {
	dv, err := g.devices.get(ctx, tx, deviceId, true)
	if err != nil {
		return err
	}
	if dv.UserId != childId || dv.State == DeviceRevoked || dv.State == DeviceExpired {
		return nil
	}
	if err := g.devices.MarkGuardianCleared(ctx, tx, deviceId); err != nil {
		return err
	}
	by := guardianId
	return g.d.emitDeviceChanged(ctx, tx, childId, deviceId, changeGuardianApproved, &by)
}

// lockFor は承認と不承認の前段。求めの子を錠なしで読み（child_id は変わらない欄）、保護者であることを確かめ、子の構成員の行の錠を取る。
// 錠の順は構成員、承認の求め、端末、セッション（全体実装設計書 2.4。子ども区分を外す操作は構成員の行の後に求めを expired にする）。
// 求めが無い・保護者でなければ MEM-001。
func (g *Guardians) lockFor(ctx context.Context, tx *store.Tx, guardianId, approvalId uuid.UUID) (uuid.UUID, error) {
	var child uuid.UUID
	err := tx.QueryRow(ctx, `SELECT child_id FROM guardian_approvals WHERE id = $1`, approvalId).Scan(&child)
	if noRows(err) {
		return uuid.Nil, ev(cgen.MEM001)
	}
	if err != nil {
		return uuid.Nil, wrap(err)
	}
	if err := g.authorize(ctx, guardianId, child); err != nil {
		return uuid.Nil, err
	}
	if _, err := g.users.get(ctx, tx, child, true); err != nil {
		return uuid.Nil, err
	}
	return child, nil
}

// Deny は保護者が承認しない（POST /me/guardian/approvals/{id} の approve が偽）。子の保護者だけ（違えば MEM-001）。
// open で期限内の求めを denied で閉じる。端末の追加で対象が端末のコードなら QR の行も denied にする（新しい端末の poll は AUTH-005 の
// access_denied で終わる）。対象が端末の識別子ならその端末を取り消す（by は保護者。端末は session.revoked でログインの画面に戻る）。
// 第3章 4.4、12。拒否はこの保護者の不承認だけで、本人が断る操作は無い。既に閉じた求めには何もしない（冪等）。
func (g *Guardians) Deny(ctx context.Context, tx *store.Tx, guardianId, approvalId uuid.UUID) error {
	if _, err := g.lockFor(ctx, tx, guardianId, approvalId); err != nil {
		return err
	}
	var kind, state string
	var target []byte
	var device *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT kind, state, target_sha256, target_device_id FROM guardian_approvals WHERE id = $1 FOR UPDATE`, approvalId).
		Scan(&kind, &state, &target, &device); err != nil {
		return wrap(err)
	}
	if state != "open" {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE guardian_approvals SET state = 'denied', approved_by = $2 WHERE id = $1`, approvalId, guardianId); err != nil {
		return wrap(err)
	}
	if ApprovalKind(kind) != ApprovalDevice {
		return nil
	}
	if device != nil {
		return g.devices.Revoke(ctx, tx, *device, guardianId, false)
	}
	_, err := tx.Exec(ctx, `UPDATE device_codes SET state = 'denied' WHERE device_code_sha256 = $1 AND state = 'pending'`, target)
	return wrap(err)
}

// expire は open の求めを expired にする（ExpireDue から）。抹消の求めなら子と保護者へ
// erase_approval_expired（第3章 8.3）。端末の追加の求めは知らせない（新しい端末の画面で分かる。第7章 9）。
// 既に閉じていれば何もしない（同時の 2 つの失効で知らせを 1 回にする）。
func (g *Guardians) expire(ctx context.Context, tx *store.Tx, approvalId uuid.UUID, kind ApprovalKind, childId uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE guardian_approvals SET state = 'expired' WHERE id = $1 AND state = 'open'`, approvalId)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 || kind != ApprovalErase {
		return nil
	}
	child, err := g.users.get(ctx, tx, childId, false)
	if err != nil {
		return err
	}
	params := map[string]any{"approval_id": approvalId.String(), "child_id": childId.String(), "child_name": child.DisplayName}
	if err := g.hooks.notifySelf(ctx, tx, childId, noticeEraseApprovalExpired, noticeKey(approvalId.String()), params); err != nil {
		return err
	}
	return g.hooks.notifyGuardians(ctx, tx, childId, noticeEraseApprovalExpired, noticeKey(approvalId.String()), params)
}

// ExpireDue は srv-worker の PruneHandler から。open で期限を過ぎた求めを自分のトランザクションで 1,000 行ずつ expire する。
// 戻りは数。
func (g *Guardians) ExpireDue(ctx context.Context) (int, error) {
	total := 0
	for {
		n := 0
		err := g.d.separateTx(ctx, func(tx *store.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id, kind, child_id FROM guardian_approvals WHERE state = 'open' AND expires_at <= $1
ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED`, g.d.now(), expireBatch)
			if err != nil {
				return wrap(err)
			}
			type due struct {
				id, child uuid.UUID
				kind      ApprovalKind
			}
			var list []due
			for rows.Next() {
				var d due
				var kind string
				if err := rows.Scan(&d.id, &kind, &d.child); err != nil {
					rows.Close()
					return wrap(err)
				}
				d.kind = ApprovalKind(kind)
				list = append(list, d)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return wrap(err)
			}
			for _, d := range list {
				if err := g.expire(ctx, tx, d.id, d.kind, d.child); err != nil {
					return err
				}
			}
			n = len(list)
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < expireBatch {
			return total, nil
		}
	}
}

// AskAgeEveryTwoYears は誕生日が無く申告だけの子のうち、最後の問いから 2 年（無ければ参加から 2 年）を過ぎた子を返し、
// 問うた時刻を記録する（保護者への問いは srv-worker が出す。第3章 9.1）。同じ日の 2 回目は返さない（I-10）。
func (g *Guardians) AskAgeEveryTwoYears(ctx context.Context) ([]uuid.UUID, error) {
	now := g.d.now()
	var out []uuid.UUID
	err := g.d.separateTx(ctx, func(tx *store.Tx) error {
		ids, err := uuids(ctx, tx, `SELECT u.id FROM users u JOIN user_private p ON p.user_id = u.id
LEFT JOIN child_controls c ON c.child_id = u.id
WHERE u.is_child AND u.state = 'normal' AND p.birthday IS NULL AND COALESCE(c.age_asked_at, u.created_at) <= $1 ORDER BY u.id`, now.Add(-askAgeEvery))
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `INSERT INTO child_controls (child_id, settings, updated_by, updated_at, age_asked_at) VALUES ($1, '{}', $1, $2, $2)
ON CONFLICT (child_id) DO UPDATE SET age_asked_at = EXCLUDED.age_asked_at`, id, now); err != nil {
				return wrap(err)
			}
		}
		out = ids
		return nil
	})
	return out, err
}
