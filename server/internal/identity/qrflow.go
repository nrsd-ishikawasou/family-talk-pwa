// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// QrStart は端末の追加の開始の指定（StartQrLoginRequest と送り元の IP）。
type QrStart struct {
	Name     string
	Platform Platform
	// SigPub と KemPub は新しい端末の署名鍵と鍵合意鍵の公開部の JWK（承認の画面の指紋の元。第3章 4.4 の 1、第12章 5.3）。
	SigPub gen.CommonJwkP2561
	KemPub gen.CommonJwkP2561
	Ip     netip.Addr
}

// QrFlow は QR による端末の追加を持つ（B-068。第3章 4.4。RFC 8628 に倣う。10 分、interval 5）。
type QrFlow struct {
	d         *Deps
	users     *Users
	devices   *Devices
	sessions  *Sessions
	guardians *Guardians
}

// userCodeTries は user_code が pending の行と重なった時の作り直しの上限。
const userCodeTries = 5

// AUTH-005 の params.state の 4 つ（第2章 9.2。RFC 8628 3.5）。access_denied は保護者の不承認（第3章 4.4）。
const (
	qrPending      = "authorization_pending"
	qrSlowDown     = "slow_down"
	qrExpired      = "expired"
	qrAccessDenied = "access_denied"
)

// qrEvent は AUTH-005（retryable は状態で決まる：pending と slow_down は真、expired と access_denied は偽）。
func qrEvent(state string) *fault.Event {
	e := fault.New(cgen.AUTH005, map[string]any{"state": state})
	e.Retryable = state == qrPending || state == qrSlowDown
	return e
}

// Start は device_code（256 ビット。ハッシュを置く）、user_code（8 文字）、/d/<code> を作る。端末の鍵の公開部 2 つと
// 名前、種類、IP を置く。user_code が pending の行と重なれば作り直す（SAVEPOINT の中、最大 5 回）。
func (q *QrFlow) Start(ctx context.Context, tx *store.Tx, s QrStart) (gen.StartQrLoginResponse, error) {
	if e := badDeviceName(s.Name); e != nil {
		return gen.StartQrLoginResponse{}, e
	}
	sig, err := json.Marshal(s.SigPub)
	if err != nil {
		return gen.StartQrLoginResponse{}, wrap(err)
	}
	kem, err := json.Marshal(s.KemPub)
	if err != nil {
		return gen.StartQrLoginResponse{}, wrap(err)
	}
	var ip *string
	if s.Ip.IsValid() {
		v := s.Ip.String()
		ip = &v
	}
	tok := q.d.Ids.NewToken()
	defer secret.Zero(tok)
	now := q.d.now()
	for try := 0; ; try++ {
		code := q.d.Ids.UserCode()
		if _, err := tx.Exec(ctx, `SAVEPOINT qr_start`); err != nil {
			return gen.StartQrLoginResponse{}, wrap(err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO device_codes (device_code_sha256, user_code, kind, name, sig_pub, kem_pub, ip, expires_at, state, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::inet, $8, 'pending', $9)`, ids.HashToken(tok), code, string(s.Platform), s.Name, sig, kem, ip, now.Add(qrTtl), now)
		if uniqueViolation(err) && try < userCodeTries-1 {
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT qr_start`); err != nil {
				return gen.StartQrLoginResponse{}, wrap(err)
			}
			continue
		}
		if err != nil {
			return gen.StartQrLoginResponse{}, wrap(err)
		}
		return gen.StartQrLoginResponse{DeviceCode: ids.EncodeToken(tok), UserCode: code, Url: q.d.origin() + "/d/" + strings.ReplaceAll(code, "-", ""),
			ExpiresIn: int(qrTtl / time.Second), Interval: int(qrInterval / time.Second)}, nil
	}
}

// qrRow は device_codes の 1 行。
type qrRow struct {
	hash            []byte
	kind, name      string
	sig, kem        []byte
	ip              *string
	expires         time.Time
	state           string
	approvedUser    *uuid.UUID
	guardianOk      bool
	approvedSession *uuid.UUID
	lastPoll        *time.Time
}

const qrCols = `device_code_sha256, kind, name, sig_pub, kem_pub, host(ip), expires_at, state, approved_user_id, guardian_approved, approved_session_id,
last_poll_at`

func scanQr(row interface{ Scan(...any) error }) (qrRow, error) {
	var r qrRow
	err := row.Scan(&r.hash, &r.kind, &r.name, &r.sig, &r.kem, &r.ip, &r.expires, &r.state, &r.approvedUser, &r.guardianOk, &r.approvedSession, &r.lastPoll)
	return r, err
}

// Poll は承認を待つ（RFC 8628 の device access token の求め）。AUTH-005（authorization_pending、slow_down、expired、access_denied）。
// access_denied は保護者が承認しなかった行（Guardians.Deny が denied にする。第3章 4.4）。本人が断る操作は無い。
// pending の間の last_poll_at は自分のトランザクションで書く（誤りの応答で要求のトランザクションが戻っても残す）。
// 承認の後の最初の poll が、端末の登録（未承認）とセッションを tx で作り、approved_session_id に記録する（シーケンス 04 の S-04b。Cookie の値はハッシュしか残らないので承認の時には作らない）。2 回目からは expired。
// 保護者の承認を経た求め（guardian_approved）なら、登録した端末を「保護者が承認した端末」にする（第3章 4.4）。
func (q *QrFlow) Poll(ctx context.Context, tx *store.Tx, deviceCode string) (Issued, error) {
	hash, ok := tokenHash(deviceCode)
	if !ok {
		return Issued{}, qrEvent(qrExpired)
	}
	var state string
	err := q.d.separateTx(ctx, func(stx *store.Tx) error {
		r, err := scanQr(stx.QueryRow(ctx, `SELECT `+qrCols+` FROM device_codes WHERE device_code_sha256 = $1 FOR UPDATE`, hash))
		if noRows(err) {
			state = qrExpired
			return nil
		}
		if err != nil {
			return wrap(err)
		}
		now := q.d.now()
		switch {
		case r.state == "denied":
			state = qrAccessDenied
		case !r.expires.After(now) || r.state == "expired":
			state = qrExpired
		case r.state == "approved" && r.approvedSession != nil:
			state = qrExpired
		case r.state == "approved":
			state = ""
		case r.lastPoll != nil && now.Sub(*r.lastPoll) < qrInterval:
			state = qrSlowDown
		default:
			state = qrPending
			_, err = stx.Exec(ctx, `UPDATE device_codes SET last_poll_at = $2 WHERE device_code_sha256 = $1`, hash, now)
		}
		return wrap(err)
	})
	if err != nil {
		return Issued{}, err
	}
	if state != "" {
		return Issued{}, qrEvent(state)
	}
	r, err := scanQr(tx.QueryRow(ctx, `SELECT `+qrCols+` FROM device_codes WHERE device_code_sha256 = $1 FOR UPDATE`, hash))
	if err != nil {
		return Issued{}, wrap(err)
	}
	if r.state != "approved" || r.approvedSession != nil || r.approvedUser == nil {
		return Issued{}, qrEvent(qrExpired)
	}
	usr, err := q.users.get(ctx, tx, *r.approvedUser, false)
	if err != nil {
		return Issued{}, err
	}
	dv, err := q.devices.Register(ctx, tx, usr.Id, Platform(r.kind), r.name)
	if err != nil {
		return Issued{}, err
	}
	if r.guardianOk {
		if err := q.devices.MarkGuardianCleared(ctx, tx, dv.Id); err != nil {
			return Issued{}, err
		}
	}
	cookie, sess, sid, err := q.sessions.issue(ctx, tx, usr.Id, dv.Id, false)
	if err != nil {
		return Issued{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE device_codes SET approved_session_id = $2 WHERE device_code_sha256 = $1`, hash, sid); err != nil {
		return Issued{}, wrap(err)
	}
	by := usr.Id
	if err := q.d.emitDeviceChanged(ctx, tx, usr.Id, dv.Id, changeAdded, &by); err != nil {
		return Issued{}, err
	}
	return Issued{Cookie: cookie, Result: gen.LoginResult{NewDevice: true, Session: sess}}, nil
}

// normalizeUserCode は入力の揺れ（小文字、区切りの有無、空白）を 4-4 の形に揃える。
func normalizeUserCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "　", "").Replace(s))
	if len(s) != 8 {
		return s
	}
	return s[:4] + "-" + s[4:]
}

// pendingRow は user_code の pending で期限内の行（無い・期限切れは AUTH-007）。lock なら行の錠。
func (q *QrFlow) pendingRow(ctx context.Context, db querier, userCode string, lock bool) (qrRow, error) {
	sql := `SELECT ` + qrCols + ` FROM device_codes WHERE user_code = $1 AND state = 'pending'`
	if lock {
		sql += ` FOR UPDATE`
	}
	r, err := scanQr(db.QueryRow(ctx, sql, normalizeUserCode(userCode)))
	if noRows(err) {
		return qrRow{}, ev(cgen.AUTH007)
	}
	if err != nil {
		return qrRow{}, wrap(err)
	}
	if !r.expires.After(q.d.now()) {
		return qrRow{}, ev(cgen.AUTH007)
	}
	return r, nil
}

// Approve は本人の承認済みの端末が承認する（x-reauth：再認証から 5 分以内。AUTH-006）。device_codes を approved にし
// approved_user_id を書くだけ（端末とセッションは承認の後の最初の poll。シーケンス 04 の S-04b）。子ども区分で保護者の「端末の追加の承認」が
// 有効なら、保護者への求めを別のトランザクションで確定してから MEM-006（保護者が Guardians.Approve で続ける）。
func (q *QrFlow) Approve(ctx context.Context, tx *store.Tx, p boundary.Principal, userCode string) error {
	me, _, err := self(p)
	if err != nil {
		return err
	}
	if err := q.d.requireReauth(p); err != nil {
		return err
	}
	r, err := q.pendingRow(ctx, tx, userCode, true)
	if err != nil {
		return err
	}
	usr, err := q.users.get(ctx, tx, me, false)
	if err != nil {
		return err
	}
	if usr.IsChild {
		s, err := q.guardians.settings(ctx, tx, me)
		if err != nil {
			return err
		}
		if s.DeviceApproval == nil || *s.DeviceApproval {
			if err := q.d.separateTx(ctx, func(stx *store.Tx) error {
				_, err := q.guardians.RequestApproval(ctx, stx, me, ApprovalDevice, ApprovalTarget{CodeSha256: r.hash})
				return err
			}); err != nil {
				return err
			}
			return ev(cgen.MEM006)
		}
	}
	return q.approve(ctx, tx, me, r.hash)
}

// approveFor は保護者の承認の続き（Guardians.Approve の device。子の端末として承認する）。
func (q *QrFlow) approveFor(ctx context.Context, tx *store.Tx, childId uuid.UUID, hash []byte) error {
	r, err := scanQr(tx.QueryRow(ctx, `SELECT `+qrCols+` FROM device_codes WHERE device_code_sha256 = $1 FOR UPDATE`, hash))
	if noRows(err) {
		return ev(cgen.AUTH007)
	}
	if err != nil {
		return wrap(err)
	}
	if r.state != "pending" || !r.expires.After(q.d.now()) {
		return ev(cgen.AUTH007)
	}
	if err := q.approve(ctx, tx, childId, hash); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE device_codes SET guardian_approved = true WHERE device_code_sha256 = $1`, hash)
	return wrap(err)
}

func (q *QrFlow) approve(ctx context.Context, tx *store.Tx, userId uuid.UUID, hash []byte) error {
	_, err := tx.Exec(ctx, `UPDATE device_codes SET state = 'approved', approved_user_id = $2 WHERE device_code_sha256 = $1`, hash, userId)
	return wrap(err)
}

// Info は GET /auth/qr/{user_code}（G-13）。本人の承認済みの端末だけ（認可 approved）。新しい端末の種類、名前、IP（生の値）、
// 端末の鍵の公開部 2 つ（device_codes の sig_pub と kem_pub。指紋は既存の端末が求める。第12章 5.3）。無い・期限切れは AUTH-007。
func (q *QrFlow) Info(ctx context.Context, p boundary.Principal, userCode string) (gen.GetQrLoginResponse, error) {
	r, err := q.pendingRow(ctx, q.d.Pool, userCode, false)
	if err != nil {
		return gen.GetQrLoginResponse{}, err
	}
	out := gen.GetQrLoginResponse{DeviceName: r.name, Platform: gen.GetQrLoginResponsePlatform(r.kind)}
	if r.ip != nil {
		out.Ip = *r.ip
	}
	if err := json.Unmarshal(r.sig, &out.SigPub); err != nil {
		return gen.GetQrLoginResponse{}, wrap(err)
	}
	if err := json.Unmarshal(r.kem, &out.KemPub); err != nil {
		return gen.GetQrLoginResponse{}, wrap(err)
	}
	return out, nil
}
