// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"net/netip"
	"strconv"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Reports は端末からの報告（B-072。POST /me/report。G-05。シェルのトークンも可）。本文なし。管理者に知らせる（第7章 9）。
type Reports struct {
	d     *Deps
	hooks *Hooks
}

// Create は報告を知らせにする。kind は update_failed（api の列挙）。管理者へ update_failed（user_id、name、code。第7章 9）。
// 同じ構成員・同じ kind の重なりの抑止は srv-push の Notices.Emit。
func (r *Reports) Create(ctx context.Context, tx *store.Tx, p boundary.Principal, req gen.CreateReportRequest) error {
	if p.UserId == nil || p.DeviceId == nil {
		return ev(cgen.AUTH001)
	}
	var name string
	if err := tx.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, *p.UserId).Scan(&name); err != nil {
		return wrap(err)
	}
	params := map[string]any{"user_id": p.UserId.String(), "name": name}
	code := ""
	if req.Code != nil {
		code = *req.Code
		params["code"] = code
	}
	switch req.Kind {
	case gen.UpdateFailed:
		return r.hooks.notifyAdmins(ctx, tx, noticeUpdateFailed, noticeKey(p.UserId.String(), code), params)
	}
	return ev(cgen.NET001)
}

// ResetLink はパスキーの作り直しのリンク（B-069。第3章 4.6。24 時間、1 回）。
type ResetLink struct {
	d        *Deps
	passkeys *Passkeys
	devices  *Devices
	sessions *Sessions
	hooks    *Hooks
}

// resetUsable は使えるリンクの構成員を返す（無い・期限切れ・使用済み・構成員が状態の通常でなければ AUTH-007）。lock なら行の錠。
func resetUsable(ctx context.Context, q querier, hash []byte, now time.Time, lock bool) (uuid.UUID, error) {
	sql := `SELECT r.user_id, r.expires_at, r.used_at, u.state FROM reset_links r JOIN users u ON u.id = r.user_id WHERE r.token_sha256 = $1`
	if lock {
		sql += ` FOR UPDATE OF r`
	}
	var userId uuid.UUID
	var exp time.Time
	var used *time.Time
	var state string
	err := q.QueryRow(ctx, sql, hash).Scan(&userId, &exp, &used, &state)
	if noRows(err) {
		return uuid.Nil, ev(cgen.AUTH007)
	}
	if err != nil {
		return uuid.Nil, wrap(err)
	}
	if used != nil || !exp.After(now) || UserState(state) != UserNormal {
		return uuid.Nil, ev(cgen.AUTH007)
	}
	return userId, nil
}

// Create は管理者がリンクのトークンを出す（srv-admin の operation から。監査は srv-admin）。対象が状態の通常でなければ MEM-002。
// 未使用の古いリンクは消す。
func (r *ResetLink) Create(ctx context.Context, tx *store.Tx, adminId, userId uuid.UUID) (string, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM users WHERE id = $1`, userId).Scan(&state)
	if noRows(err) {
		return "", ev(cgen.MEM001)
	}
	if err != nil {
		return "", wrap(err)
	}
	if UserState(state) != UserNormal {
		return "", ev(cgen.MEM002)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM reset_links WHERE user_id = $1 AND used_at IS NULL`, userId); err != nil {
		return "", wrap(err)
	}
	tok := r.d.Ids.NewToken()
	defer secret.Zero(tok)
	now := r.d.now()
	if _, err := tx.Exec(ctx, `INSERT INTO reset_links (token_sha256, user_id, admin_id, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		ids.HashToken(tok), userId, adminId, now, now.Add(resetLinkTtl)); err != nil {
		return "", wrap(err)
	}
	return ids.EncodeToken(tok), nil
}

// Peek は GET /auth/reset/{token}（表示名と期限）。使えなければ AUTH-007。
func (r *ResetLink) Peek(ctx context.Context, token string) (gen.GetPasskeyResetResponse, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return gen.GetPasskeyResetResponse{}, ev(cgen.AUTH007)
	}
	if _, err := resetUsable(ctx, r.d.Pool, hash, r.d.now(), false); err != nil {
		return gen.GetPasskeyResetResponse{}, err
	}
	var name string
	var exp time.Time
	err := r.d.Pool.QueryRow(ctx, `SELECT u.display_name, l.expires_at FROM reset_links l JOIN users u ON u.id = l.user_id WHERE l.token_sha256 = $1`, hash).
		Scan(&name, &exp)
	if err != nil {
		return gen.GetPasskeyResetResponse{}, wrap(err)
	}
	return gen.GetPasskeyResetResponse{DisplayName: name, ExpiresAt: exp.UTC()}, nil
}

// Consume は challenge（kind reset）でパスキーを作り直し、未承認の端末として登録してセッションを出す。既存のパスキーは消さない
// （失ったパスキーは本人か管理者が後で消す。最後の 1 つの規則を守るため。第3章 4.6）。最後に同じトランザクションで
// hook OnPasskeyReset（srv-keys が recovery.used（method reset）を本人の全端末と管理者へ書く）。
func (r *ResetLink) Consume(ctx context.Context, tx *store.Tx, token string, challengeId uuid.UUID, passkey gen.WebAuthnResponse, device NewDevice) (Issued, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return Issued{}, ev(cgen.AUTH007)
	}
	userId, err := resetUsable(ctx, tx, hash, r.d.now(), true)
	if err != nil {
		return Issued{}, err
	}
	c, session, err := r.passkeys.takeChallenge(ctx, tx, challengeId, ChallengeReset)
	if err != nil {
		return Issued{}, err
	}
	if !bytesEqual(c.TokenSha256, hash) || c.UserId == nil || *c.UserId != userId {
		return Issued{}, ev(cgen.AUTH003)
	}
	if _, err := tx.Exec(ctx, `UPDATE reset_links SET used_at = $2 WHERE token_sha256 = $1`, hash, r.d.now()); err != nil {
		return Issued{}, wrap(err)
	}
	usr, handle, err := r.passkeys.userHandle(ctx, tx, userId)
	if err != nil {
		return Issued{}, err
	}
	if _, err := r.passkeys.finishRegister(ctx, tx, webauthnUser{handle: handle, name: usr.DisplayName}, session, passkey, userId, nil); err != nil {
		return Issued{}, err
	}
	dv, err := r.passkeys.newDevice(ctx, tx, usr, device)
	if err != nil {
		return Issued{}, err
	}
	cookie, sess, err := r.sessions.Issue(ctx, tx, userId, dv.Id, true)
	if err != nil {
		return Issued{}, err
	}
	if err := r.hooks.onPasskeyReset(ctx, tx, userId, dv.Id); err != nil {
		return Issued{}, err
	}
	return Issued{Cookie: cookie, Result: gen.LoginResult{NewDevice: true, Session: sess}}, nil
}

// IpLog は生の IP の記録（B-071。第12章 11。設定 retention.ip の 28 日）。IP は端末の持ち主の DEK のエンベロープ。
type IpLog struct {
	d    *Deps
	keys userKeys
}

// Record は 1 件を書く。
func (l *IpLog) Record(ctx context.Context, tx *store.Tx, deviceId uuid.UUID, ip netip.Addr, at time.Time) error {
	var owner uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT user_id FROM devices WHERE id = $1`, deviceId).Scan(&owner); err != nil {
		if noRows(err) {
			return ev(cgen.KEY001)
		}
		return wrap(err)
	}
	return l.record(ctx, tx, owner, deviceId, ip, at)
}

// record は持ち主の分かった 1 件を書く（認証の追加データは ft/ip_log/ip/<端末>/<時刻の UnixNano>）。
func (l *IpLog) record(ctx context.Context, tx *store.Tx, owner, deviceId uuid.UUID, ip netip.Addr, at time.Time) error {
	blob, err := l.keys.seal(ctx, tx, owner, []byte(ip.String()), aad("ip_log", "ip", deviceId.String(), strconv.FormatInt(at.UnixNano(), 10)))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ip_log (device_id, ip, at) VALUES ($1, $2, $3)`, deviceId, blob, at)
	return wrap(err)
}

// Prune は保持（retention.ip）を過ぎた記録を消す（srv-worker）。
func (l *IpLog) Prune(ctx context.Context) (int64, error) {
	return l.d.Retention.DeleteOlder(ctx, "ip_log", "at", l.d.now().Add(-l.d.Config.Get(cgen.KeyRetentionIp).Duration()))
}
