// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

// Sessions はセッションと短いトークンを持つ（B-066。第2章 4.1、4.4、第3章 4.3）。
// Cookie の値は 256 ビットの乱数で、DB にはハッシュを置く。寿命は絶対 90 日・無操作 30 日（設定 auth.session_*）。
type Sessions struct {
	d       *Deps
	devices *Devices
	expiry  *expiryCache
}

// sessionRow はセッションと構成員と端末を 1 回で読んだ物。
type sessionRow struct {
	id, userId, deviceId uuid.UUID
	absolute, idle       time.Time
	reason               *string
	reauthAt             *time.Time
	role                 boundary.Role
	userState            UserState
	deviceState          DeviceState
	ackVersion           *string
}

const sessionSelect = `SELECT s.id, s.user_id, s.device_id, s.absolute_expires_at, s.idle_expires_at, s.revoked_reason, s.reauth_at,
  u.role, u.state, d.state, u.disclosure_ack_version
FROM sessions s JOIN users u ON u.id = s.user_id JOIN devices d ON d.id = s.device_id `

func scanSession(row interface{ Scan(...any) error }) (sessionRow, error) {
	var r sessionRow
	var role, us, ds string
	err := row.Scan(&r.id, &r.userId, &r.deviceId, &r.absolute, &r.idle, &r.reason, &r.reauthAt, &role, &us, &ds, &r.ackVersion)
	r.role, r.userState, r.deviceState = boundary.Role(role), UserState(us), DeviceState(ds)
	r.absolute, r.idle, r.reauthAt = r.absolute.UTC(), r.idle.UTC(), utcPtr(r.reauthAt)
	return r, err
}

// durations は設定の絶対と無操作の期限。
func (s *Sessions) durations() (time.Duration, time.Duration) {
	return s.d.Config.Get(cgen.KeyAuthSessionAbsolute).Duration(), s.d.Config.Get(cgen.KeyAuthSessionIdle).Duration()
}

// Issue はセッションを作り Cookie の値を返す。reauth は UV のあるパスキーの登録・ログインの直後（reauth_at を今に。第3章 4.5）。
func (s *Sessions) Issue(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, reauth bool) (string, gen.Session, error) {
	v, sess, _, err := s.issue(ctx, tx, userId, deviceId, reauth)
	return v, sess, err
}

// issue は Issue と同じで、セッションの識別子も返す（QR の poll が device_codes に記録する）。
func (s *Sessions) issue(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID, reauth bool) (string, gen.Session, uuid.UUID, error) {
	tok := s.d.Ids.NewToken()
	defer secret.Zero(tok)
	now := s.d.now()
	abs, idle := s.durations()
	var reauthAt *time.Time
	if reauth {
		reauthAt = &now
	}
	id := s.d.Ids.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO sessions (id, token_sha256, user_id, device_id, created_at, absolute_expires_at, idle_expires_at, reauth_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, ids.HashToken(tok), userId, deviceId, now, now.Add(abs), now.Add(idle), reauthAt); err != nil {
		return "", gen.Session{}, uuid.Nil, wrap(err)
	}
	row, err := scanSession(tx.QueryRow(ctx, sessionSelect+`WHERE s.id = $1`, id))
	if err != nil {
		return "", gen.Session{}, uuid.Nil, wrap(err)
	}
	return ids.EncodeToken(tok), row.view(), id, nil
}

// view は GET /me/session と LoginResult の形。
func (r sessionRow) view() gen.Session {
	return gen.Session{UserId: r.userId.String(), DeviceId: r.deviceId.String(), Role: gen.SessionRole(r.role),
		Approved: r.deviceState.approved(), ExpiresAt: r.absolute, IdleExpiresAt: r.idle, DisclosureAckVersion: r.ackVersion}
}

// approved は認可で承認済みとして扱う状態（承認済みと休眠。休眠は承認済みのまま leaf を失った端末で、rejoin の経路を使う。第3章 5.1）。
func (s DeviceState) approved() bool {
	return s == DeviceApproved || s == DeviceDormant
}

// decodeCookie は Cookie の値（base64url の 32 バイト）を読む。
func decodeCookie(v string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	return b, err == nil && len(b) == ids.TokenBytes
}

// tokenHash は URL や本文のトークン（base64url の 32 バイト）を読んでハッシュにし、読んだ値を 0 で埋める。
func tokenHash(v string) ([]byte, bool) {
	raw, ok := decodeCookie(v)
	defer secret.Zero(raw)
	if !ok {
		return nil, false
	}
	return ids.HashToken(raw), true
}

// asEvent は Verifier の口の Event にする（予期しない失敗は SRV-003）。
func asEvent(err error) *fault.Event {
	var e *fault.Event
	if errors.As(err, &e) {
		return e
	}
	var f *fault.Fault
	if errors.As(err, &f) {
		return f.AsEvent()
	}
	return fault.Wrap(err).AsEvent()
}

// revoked は AUTH-002（理由を params に）。
func revoked(reason RevokeReason) *fault.Event {
	return fault.New(cgen.AUTH002, map[string]any{"reason": string(reason)})
}

// check は行の有効さを確かめ、期限切れなら自分のトランザクションで失効（expired）にする（session.revoked は書かない：
// 要求を出した端末は応答で分かり、接続したままの端末は ExpireDue が拾う）。
func (s *Sessions) check(ctx context.Context, r sessionRow) *fault.Event {
	switch {
	case r.reason != nil:
		return revoked(RevokeReason(*r.reason))
	case r.userState == UserSuspended:
		return revoked(RevokeSuspended)
	case r.userState == UserDeleted:
		return revoked(RevokeDeleted)
	case r.userState == UserErased:
		return revoked(RevokeErased)
	case r.deviceState == DeviceRevoked:
		return revoked(RevokeDeviceRevoked)
	case r.deviceState == DeviceExpired:
		return revoked(RevokeExpired)
	}
	now := s.d.now()
	if !now.Before(r.absolute) || !now.Before(r.idle) {
		if err := s.d.separateTx(ctx, func(tx *store.Tx) error {
			return s.revokeRow(ctx, tx, r.id, RevokeExpired, false)
		}); err != nil {
			return asEvent(err)
		}
		return revoked(RevokeExpired)
	}
	return nil
}

// principal は行から主体を作る。
func (r sessionRow) principal(kind boundary.PrincipalKind) boundary.Principal {
	uid, did, sid := r.userId, r.deviceId, r.id
	return boundary.Principal{Kind: kind, UserId: &uid, DeviceId: &did, SessionId: &sid, Role: r.role,
		DeviceApproved: r.deviceState.approved(), ReauthAt: r.reauthAt}
}

// Verify は Cookie を確かめる。AUTH-001 か AUTH-002（理由を params に）。無操作の期限を延ばす（前の延長から 1 分以上の時だけ書く）。
// 同じ書き込みで端末の最終の利用（Devices.Touch。IP は ClientIpFrom）。予期しない失敗は SRV-003 の Event。
// Principal.DeviceApproved は devices.state が approved か dormant（承認の正。全体実装設計書 3.1）。
func (s *Sessions) Verify(ctx context.Context, cookie string) (boundary.Principal, *fault.Event) {
	tok, ok := decodeCookie(cookie)
	if !ok {
		return boundary.Principal{}, ev(cgen.AUTH001)
	}
	r, err := scanSession(s.d.Pool.QueryRow(ctx, sessionSelect+`WHERE s.token_sha256 = $1`, ids.HashToken(tok)))
	secret.Zero(tok)
	if noRows(err) {
		return boundary.Principal{}, ev(cgen.AUTH001)
	}
	if err != nil {
		return boundary.Principal{}, asEvent(wrap(err))
	}
	if e := s.check(ctx, r); e != nil {
		return boundary.Principal{}, e
	}
	if err := s.extend(ctx, r); err != nil {
		return boundary.Principal{}, asEvent(err)
	}
	s.expiry.put(r.id, r.absolute, s.d.now())
	return r.principal(boundary.KindUser), nil
}

// extend は無操作の期限を延ばし、端末の最終の利用を書く（1 分に 1 回まで）。
func (s *Sessions) extend(ctx context.Context, r sessionRow) error {
	now := s.d.now()
	_, idle := s.durations()
	if now.Sub(r.idle.Add(-idle)) < touchEvery {
		return nil
	}
	// 錠の順は端末、セッション（端末の取り消しと同じ順）。
	return s.d.separateTx(ctx, func(tx *store.Tx) error {
		if err := s.devices.Touch(ctx, tx, r.deviceId, boundary.ClientIpFrom(ctx), now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sessions SET idle_expires_at = $2 WHERE id = $1 AND revoked_at IS NULL`, r.id, now.Add(idle))
		return wrap(err)
	})
}

// ExpiresAt は X-Session-Expires の時刻（絶対の期限。7 日前から端末が作り直す。第2章 4.1）。Verify が記憶の上に置いた値を読み、
// 無ければ DB を読む（読めなければ空の時刻で、ヘッダを付けない）。
func (s *Sessions) ExpiresAt(p boundary.Principal) time.Time {
	if p.SessionId == nil {
		return time.Time{}
	}
	if t, ok := s.expiry.get(*p.SessionId, s.d.now()); ok {
		return t
	}
	var t time.Time
	if err := s.d.Pool.QueryRow(context.Background(), `SELECT absolute_expires_at FROM sessions WHERE id = $1`, *p.SessionId).Scan(&t); err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Describe は GET /me/session（端末の識別子、承認、期限、明示の確認の版）。
func (s *Sessions) Describe(ctx context.Context, p boundary.Principal) (gen.Session, error) {
	if p.SessionId == nil {
		return gen.Session{}, ev(cgen.AUTH001)
	}
	r, err := scanSession(s.d.Pool.QueryRow(ctx, sessionSelect+`WHERE s.id = $1`, *p.SessionId))
	if noRows(err) {
		return gen.Session{}, ev(cgen.AUTH001)
	}
	if err != nil {
		return gen.Session{}, wrap(err)
	}
	return r.view(), nil
}

// Rotate は絶対の期限の前の作り直し（ログインの検証の中で古いものを失効させ新しい値を返す。シーケンス 04）。
// 古い行は expired（logout ではない）。session.revoked は書かない（同じ端末が新しいセッションを持つ。第2章 6.2）。
func (s *Sessions) Rotate(ctx context.Context, tx *store.Tx, sessionId uuid.UUID) (string, error) {
	v, _, err := s.rotate(ctx, tx, sessionId)
	return v, err
}

func (s *Sessions) rotate(ctx context.Context, tx *store.Tx, sessionId uuid.UUID) (string, gen.Session, error) {
	var userId, deviceId uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT user_id, device_id FROM sessions WHERE id = $1 FOR UPDATE`, sessionId).Scan(&userId, &deviceId); err != nil {
		return "", gen.Session{}, wrap(err)
	}
	if err := s.revokeRow(ctx, tx, sessionId, RevokeExpired, false); err != nil {
		return "", gen.Session{}, err
	}
	return s.Issue(ctx, tx, userId, deviceId, true)
}

// Revoke は 1 つのセッションを失効させる。既に失効なら何もしない（冪等）。そのセッションの短いトークンを使用済みにし、
// 同じトランザクションでその構成員の feed に session.revoked{reason} を宛先の端末 = そのセッションの端末で 1 行書く
// （その端末にだけ届く。全体実装設計書 2.4。hub が書いた後に 4001 で閉じる。本ブロックは hub を呼ばない）。
func (s *Sessions) Revoke(ctx context.Context, tx *store.Tx, sessionId uuid.UUID, reason RevokeReason) error {
	return s.revokeRow(ctx, tx, sessionId, reason, true)
}

// revokeRow は失効の書き込み。notify が偽なら session.revoked を書かない（作り直しと、要求で見つけた期限切れ）。
func (s *Sessions) revokeRow(ctx context.Context, tx *store.Tx, sessionId uuid.UUID, reason RevokeReason, notify bool) error {
	now := s.d.now()
	var userId, deviceId uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE sessions SET revoked_at = $2, revoked_reason = $3 WHERE id = $1 AND revoked_at IS NULL RETURNING user_id, device_id`,
		sessionId, now, string(reason)).Scan(&userId, &deviceId)
	if noRows(err) {
		return nil
	}
	if err != nil {
		return wrap(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE short_tokens SET used_at = $2 WHERE session_id = $1 AND used_at IS NULL`, sessionId, now); err != nil {
		return wrap(err)
	}
	if !notify {
		return nil
	}
	return s.d.emit(ctx, tx, []uuid.UUID{userId}, "session.revoked", deviceId, sessionRevoked{Reason: reason})
}

// RevokeAllOfDevice は端末の全セッションを失効させる（端末の取り消し）。行ごとに session.revoked。
func (s *Sessions) RevokeAllOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID, reason RevokeReason) error {
	return s.revokeAll(ctx, tx, `SELECT id FROM sessions WHERE device_id = $1 AND revoked_at IS NULL ORDER BY id FOR UPDATE`, deviceId, reason)
}

// RevokeAllOfUser は構成員の全セッションを失効させる（停止、削除、抹消、鍵の交代）。端末ごとに session.revoked。
func (s *Sessions) RevokeAllOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID, reason RevokeReason) error {
	return s.revokeAll(ctx, tx, `SELECT id FROM sessions WHERE user_id = $1 AND revoked_at IS NULL ORDER BY id FOR UPDATE`, userId, reason)
}

func (s *Sessions) revokeAll(ctx context.Context, tx *store.Tx, sql string, arg uuid.UUID, reason RevokeReason) error {
	list, err := uuids(ctx, tx, sql, arg)
	if err != nil {
		return err
	}
	for _, id := range list {
		if err := s.revokeRow(ctx, tx, id, reason, true); err != nil {
			return err
		}
	}
	return nil
}

// ExpireDue は srv-worker の session_expire（1 時間）から。絶対か無操作の期限を過ぎた有効な行を自分のトランザクションで
// expired にし、Revoke と同じく session.revoked{expired} をその端末宛てに書く（接続したままで要求を出さない端末に期限を届ける。
// 第3章 4.3）。1,000 行ずつ（長い錠を持たない）。戻りは失効させた数。
func (s *Sessions) ExpireDue(ctx context.Context) (int, error) {
	total := 0
	for {
		n := 0
		err := s.d.separateTx(ctx, func(tx *store.Tx) error {
			list, err := uuids(ctx, tx, `SELECT id FROM sessions WHERE revoked_at IS NULL AND (absolute_expires_at <= $1 OR idle_expires_at <= $1)
ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED`, s.d.now(), expireBatch)
			if err != nil {
				return err
			}
			for _, id := range list {
				if err := s.revokeRow(ctx, tx, id, RevokeExpired, true); err != nil {
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

// 短いトークンの種類（short_tokens.kind）。
const (
	shortWs    = "ws"
	shortShell = "shell"
)

// shortTokenMac は session-keys の鍵で「ft.short_token/1、0、kind、0、乱数」を署名する（第12章 10）。
func shortTokenMac(key []byte, kind string, nonce []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("ft.short_token/1"))
	m.Write([]byte{0})
	m.Write([]byte(kind))
	m.Write([]byte{0})
	m.Write(nonce)
	return m.Sum(nil)
}

// issueShort は短いトークンを作る。値は <鍵 ID>.<乱数>.<HMAC>（base64url を . で結ぶ）。発行は created_at の新しい鍵。
// short_tokens に値の SHA-256 を置く（使い切りと期限とセッションの失効の確かめ）。
func (s *Sessions) issueShort(ctx context.Context, tx *store.Tx, p boundary.Principal, kind string, ttl time.Duration) (gen.TokenGrant, error) {
	if p.SessionId == nil {
		return gen.TokenGrant{}, ev(cgen.AUTH001)
	}
	keys, err := s.d.Secrets.SessionKeys()
	if err != nil {
		return gen.TokenGrant{}, wrap(err)
	}
	defer zeroSessionKeys(keys)
	cur := keys[0]
	for _, k := range keys[1:] {
		if k.CreatedAt.After(cur.CreatedAt) {
			cur = k
		}
	}
	nonce := s.d.Ids.NewToken()
	enc := base64.RawURLEncoding
	value := cur.Id + "." + enc.EncodeToString(nonce) + "." + enc.EncodeToString(shortTokenMac(cur.Key, kind, nonce))
	exp := s.d.now().Add(ttl)
	if _, err := tx.Exec(ctx, `INSERT INTO short_tokens (token_sha256, kind, session_id, expires_at) VALUES ($1, $2, $3, $4)`,
		ids.HashToken([]byte(value)), kind, *p.SessionId, exp); err != nil {
		return gen.TokenGrant{}, wrap(err)
	}
	return gen.TokenGrant{Token: value, ExpiresAt: exp}, nil
}

// signedShort は形と鍵 ID と HMAC を確かめる（DB を読まない）。
func (s *Sessions) signedShort(value, kind string) (bool, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false, nil
	}
	enc := base64.RawURLEncoding
	nonce, err1 := enc.DecodeString(parts[1])
	mac, err2 := enc.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(nonce) != ids.TokenBytes {
		return false, nil
	}
	keys, err := s.d.Secrets.SessionKeys()
	if err != nil {
		return false, wrap(err)
	}
	defer zeroSessionKeys(keys)
	for _, k := range keys {
		if k.Id == parts[0] {
			return hmac.Equal(mac, shortTokenMac(k.Key, kind, nonce)), nil
		}
	}
	return false, nil
}

// zeroSessionKeys は読んだ鍵を 0 で埋める。
func zeroSessionKeys(keys []secret.KeyWithId) {
	for _, k := range keys {
		secret.Zero(k.Key)
	}
}

// IssueWsToken は WebSocket の auth のトークン（60 秒、使い切り）。
func (s *Sessions) IssueWsToken(ctx context.Context, tx *store.Tx, p boundary.Principal) (gen.TokenGrant, error) {
	return s.issueShort(ctx, tx, p, shortWs, wsTokenTtl)
}

// VerifyWsToken はトークンを使い切って主体を返す。AUTH-001（形、署名、使用済み、期限切れ）、セッションの失効は AUTH-002。
// WebSocket の接続の時（要求のトランザクションの外）なので、使い切りの書き込みは自分のトランザクションで行う。
func (s *Sessions) VerifyWsToken(ctx context.Context, token string) (boundary.Principal, *fault.Event) {
	ok, err := s.signedShort(token, shortWs)
	if err != nil {
		return boundary.Principal{}, asEvent(err)
	}
	if !ok {
		return boundary.Principal{}, ev(cgen.AUTH001)
	}
	var sid uuid.UUID
	err = s.d.separateTx(ctx, func(tx *store.Tx) error {
		now := s.d.now()
		return tx.QueryRow(ctx, `UPDATE short_tokens SET used_at = $2 WHERE token_sha256 = $1 AND kind = 'ws' AND used_at IS NULL AND expires_at > $2
RETURNING session_id`, ids.HashToken([]byte(token)), now).Scan(&sid)
	})
	if noRows(err) {
		return boundary.Principal{}, ev(cgen.AUTH001)
	}
	if err != nil {
		return boundary.Principal{}, asEvent(wrap(err))
	}
	return s.principalOf(ctx, sid, boundary.KindUser)
}

// principalOf はセッションの識別子から主体を作る（Verify と同じ確かめ）。
func (s *Sessions) principalOf(ctx context.Context, sid uuid.UUID, kind boundary.PrincipalKind) (boundary.Principal, *fault.Event) {
	r, err := scanSession(s.d.Pool.QueryRow(ctx, sessionSelect+`WHERE s.id = $1`, sid))
	if noRows(err) {
		return boundary.Principal{}, ev(cgen.AUTH001)
	}
	if err != nil {
		return boundary.Principal{}, asEvent(wrap(err))
	}
	if e := s.check(ctx, r); e != nil {
		return boundary.Principal{}, e
	}
	return r.principal(kind), nil
}

// IssueShellToken はシェルのトークン（24 時間）。
func (s *Sessions) IssueShellToken(ctx context.Context, tx *store.Tx, p boundary.Principal) (gen.TokenGrant, error) {
	return s.issueShort(ctx, tx, p, shortShell, shellTokenTtl)
}

// VerifyShellToken はシェルのトークンを確かめて主体（Kind は shell）を返す。AUTH-001 か AUTH-002。使い切りではない。
func (s *Sessions) VerifyShellToken(ctx context.Context, token string) (boundary.Principal, *fault.Event) {
	ok, err := s.signedShort(token, shortShell)
	if err != nil {
		return boundary.Principal{}, asEvent(err)
	}
	if !ok {
		return boundary.Principal{}, ev(cgen.AUTH001)
	}
	var sid uuid.UUID
	err = s.d.Pool.QueryRow(ctx, `SELECT session_id FROM short_tokens WHERE token_sha256 = $1 AND kind = 'shell' AND expires_at > $2`,
		ids.HashToken([]byte(token)), s.d.now()).Scan(&sid)
	if noRows(err) {
		return boundary.Principal{}, ev(cgen.AUTH001)
	}
	if err != nil {
		return boundary.Principal{}, asEvent(wrap(err))
	}
	return s.principalOf(ctx, sid, boundary.KindShell)
}

// Prune は期限を過ぎた短いトークン、challenge、端末のコード（期限から 1 日）と、失効から 90 日のセッションを消す（srv-worker）。
// 参照の順（short_tokens と device_codes が sessions を指す）に消す。
func (s *Sessions) Prune(ctx context.Context) (int64, error) {
	now := s.d.now()
	var total int64
	for _, t := range []struct {
		table, column string
		before        time.Time
	}{
		{"short_tokens", "expires_at", now},
		{"login_challenges", "expires_at", now},
		{"device_codes", "expires_at", now.Add(-24 * time.Hour)},
		{"sessions", "revoked_at", now.Add(-sessionKeepAfter)},
	} {
		n, err := s.d.Retention.DeleteOlder(ctx, t.table, t.column, t.before)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// cookieName は Cookie の名（第2章 4.1）。
const cookieName = "__Host-session"

// SessionCookie は __Host-session の Set-Cookie の値（Secure、HttpOnly、SameSite=Lax、Path=/。第2章 4.1）。
// Max-Age は now から expires までの秒。
func SessionCookie(value string, expires, now time.Time) string {
	secs := int64(expires.Sub(now) / time.Second)
	if secs < 0 {
		secs = 0
	}
	var b bytes.Buffer
	b.WriteString(cookieName + "=" + value + "; Path=/; Expires=" + expires.UTC().Format(http.TimeFormat))
	b.WriteString("; Max-Age=" + strconv.FormatInt(secs, 10) + "; Secure; HttpOnly; SameSite=Lax")
	return b.String()
}

// ClearSessionCookie は __Host-session を消す Set-Cookie の値（Max-Age=0）。
func ClearSessionCookie() string {
	return cookieName + "=; Path=/; Expires=" + time.Unix(0, 0).UTC().Format(http.TimeFormat) + "; Max-Age=0; Secure; HttpOnly; SameSite=Lax"
}

// expiryCache は Verify が読んだ絶対の期限の短い記憶（SessionId から。1 分で消える。LRU 1,024）。
// SessionVerifier.ExpiresHeader は Principal だけを受けるので、Verify の読みを使い回す。
type expiryCache struct {
	mu    sync.Mutex
	order *list.List
	items map[uuid.UUID]*list.Element
}

type expiryEntry struct {
	id       uuid.UUID
	absolute time.Time
	at       time.Time
}

const (
	expiryCap = 1024
	expiryTtl = time.Minute
)

func newExpiryCache() *expiryCache {
	return &expiryCache{order: list.New(), items: map[uuid.UUID]*list.Element{}}
}

func (c *expiryCache) put(id uuid.UUID, absolute, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		el.Value = expiryEntry{id: id, absolute: absolute, at: now}
		c.order.MoveToFront(el)
		return
	}
	c.items[id] = c.order.PushFront(expiryEntry{id: id, absolute: absolute, at: now})
	for c.order.Len() > expiryCap {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(expiryEntry).id)
	}
}

func (c *expiryCache) get(id uuid.UUID, now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[id]
	if !ok {
		return time.Time{}, false
	}
	e := el.Value.(expiryEntry)
	if now.Sub(e.at) > expiryTtl {
		c.order.Remove(el)
		delete(c.items, id)
		return time.Time{}, false
	}
	return e.absolute, true
}
