// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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

// NewDevice は参加・ログイン・作り直しで作る最初の端末の指定（要求の device_name と platform）。
type NewDevice struct {
	Name     string
	Platform Platform
}

// Issued はセッションを出した結果。Cookie は応答の Set-Cookie の値（__Host-session。第2章 4.1）。
type Issued struct {
	Cookie string
	Result gen.LoginResult
}

// AcceptResult は管理者の作成と招待の受諾の結果（ページの AcceptResult。端末は次に鍵バンドルの作成へ）。
type AcceptResult struct {
	User    User
	Device  Device
	Passkey Credential
	Issued  Issued
}

// Invite は招待の 1 行（invites）。
type Invite struct {
	Id        uuid.UUID
	TokenHash []byte
	// ExpiresAt は発行から 7 日。
	ExpiresAt time.Time
	// UsesLeft は 1。
	UsesLeft    int
	Role        boundary.Role
	DisplayHint *string
	Child       *ChildSpec
	Issuer      uuid.UUID
	UsedBy      *uuid.UUID
	UsedAt      *time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

// InviteSpec は招待の発行の指定（CreateInviteRequest）。
type InviteSpec struct {
	Role        boundary.Role
	DisplayHint *string
	Child       *ChildSpec
}

// childJSON は invites.child の形。
type childJSON struct {
	IsChild   bool        `json:"is_child"`
	Guardians []uuid.UUID `json:"guardians"`
}

// Invites は招待を持つ（B-063。第3章 3.2、3.3）。
type Invites struct {
	d         *Deps
	users     *Users
	passkeys  *Passkeys
	devices   *Devices
	sessions  *Sessions
	guardians *Guardians
	families  *Families
	hooks     *Hooks
	keys      userKeys
}

// mayIssue は規則 invite_issue（本ブロックが判定）：管理者か、設定 auth.members_can_invite が真の構成員。
// 子ども区分は child_controls の招待の可否も要る。許されなければ MEM-007。管理者の招待（role admin）は管理者だけ。
func (i *Invites) mayIssue(ctx context.Context, q querier, issuer User, role boundary.Role) error {
	if issuer.Role == boundary.RoleAdmin {
		return nil
	}
	if role == boundary.RoleAdmin || !i.d.Config.Get(cgen.KeyAuthMembersCanInvite).Bool() {
		return ev(cgen.MEM007)
	}
	if issuer.IsChild {
		s, err := i.guardians.settings(ctx, q, issuer.Id)
		if err != nil {
			return err
		}
		if s.AllowInvites == nil || !*s.AllowInvites {
			return ev(cgen.MEM007)
		}
	}
	return nil
}

// Issue は招待を出し、トークン（URL に入れる値）を返す。同時に設定 auth.unused_invites_max（20）まで（MEM-004）。
// 子ども区分と「構成員も招待を出せる」の設定（MEM-007）。子の保護者が空なら発行者。発行者が管理者なら Audit（本ブロックのタグから入る管理者の操作の記録）。
func (i *Invites) Issue(ctx context.Context, tx *store.Tx, issuer uuid.UUID, spec InviteSpec) (Invite, string, error) {
	// 発行者の行の錠で同じ発行者の発行を直列にする（同時の 2 つが上限の数えをすり抜けないため）。
	usr, err := i.users.get(ctx, tx, issuer, true)
	if err != nil {
		return Invite{}, "", err
	}
	if err := i.mayIssue(ctx, tx, usr, spec.Role); err != nil {
		return Invite{}, "", err
	}
	now := i.d.now()
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM invites WHERE issuer = $1 AND uses_left > 0 AND revoked_at IS NULL AND expires_at > $2`,
		issuer, now).Scan(&open); err != nil {
		return Invite{}, "", wrap(err)
	}
	if int64(open) >= i.d.Config.Get(cgen.KeyAuthUnusedInvitesMax).Int() {
		return Invite{}, "", ev(cgen.MEM004)
	}
	inv := Invite{Id: i.d.Ids.NewV7(), ExpiresAt: now.Add(i.d.Config.Get(cgen.KeyAuthInviteTtl).Duration()),
		UsesLeft: int(i.d.Config.Get(cgen.KeyAuthInviteUses).Int()), Role: spec.Role, DisplayHint: spec.DisplayHint, Issuer: issuer, CreatedAt: now}
	var child []byte
	if spec.Child != nil && spec.Child.IsChild {
		gs := spec.Child.Guardians
		if len(gs) == 0 {
			gs = []uuid.UUID{issuer}
		}
		for _, g := range gs {
			gu, err := i.users.get(ctx, tx, g, false)
			if err != nil || gu.State != UserNormal {
				return Invite{}, "", fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": "body/child/guardians", "message": "家族の構成員でない"}}})
			}
		}
		inv.Child = &ChildSpec{IsChild: true, Guardians: gs}
		if child, err = json.Marshal(childJSON{IsChild: true, Guardians: gs}); err != nil {
			return Invite{}, "", wrap(err)
		}
	}
	var hint []byte
	if spec.DisplayHint != nil {
		if hint, err = i.keys.seal(ctx, tx, issuer, []byte(*spec.DisplayHint), aad("invites", "display_hint", inv.Id.String())); err != nil {
			return Invite{}, "", err
		}
	}
	tok := i.d.Ids.NewToken()
	defer secret.Zero(tok)
	inv.TokenHash = ids.HashToken(tok)
	if _, err := tx.Exec(ctx, `INSERT INTO invites (id, token_sha256, expires_at, uses_left, role, display_hint, child, issuer, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, inv.Id, inv.TokenHash, inv.ExpiresAt, inv.UsesLeft, string(inv.Role), hint, child, issuer, now); err != nil {
		return Invite{}, "", wrap(err)
	}
	if usr.Role == boundary.RoleAdmin {
		if err := i.d.Audit.Record(ctx, tx, issuer, auditInviteIssued, inv.Id.String(), map[string]any{"role": string(inv.Role)}); err != nil {
			return Invite{}, "", wrap(err)
		}
	}
	return inv, ids.EncodeToken(tok), nil
}

// Revoke は招待を取り消す（規則 invite_issuer_or_admin。無い・発行者でも管理者でもなければ MEM-001）。
// 使用済み・取り消し済みなら何もしない。管理者が他人の招待を取り消す時は Audit（本ブロックのタグから入る管理者の操作の記録）。
func (i *Invites) Revoke(ctx context.Context, tx *store.Tx, id, by uuid.UUID) error {
	actor, err := i.users.get(ctx, tx, by, false)
	if err != nil {
		return err
	}
	var issuer uuid.UUID
	var used int
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT issuer, uses_left, revoked_at FROM invites WHERE id = $1 FOR UPDATE`, id).Scan(&issuer, &used, &revokedAt)
	if noRows(err) {
		return ev(cgen.MEM001)
	}
	if err != nil {
		return wrap(err)
	}
	if issuer != by && actor.Role != boundary.RoleAdmin {
		return ev(cgen.MEM001)
	}
	if used == 0 || revokedAt != nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET revoked_at = $2 WHERE id = $1`, id, i.d.now()); err != nil {
		return wrap(err)
	}
	if issuer != by {
		return wrap(i.d.Audit.Record(ctx, tx, by, auditInviteRevoked, id.String(), map[string]any{"issuer": issuer.String()}))
	}
	return nil
}

// inviteRow は使える招待の読み。
type inviteRow struct {
	id       uuid.UUID
	issuer   uuid.UUID
	role     boundary.Role
	child    *ChildSpec
	expires  time.Time
	usesLeft int
}

// inviteUsable は使える招待を読む（無効・期限切れ・使用済み・取り消し済みは AUTH-007）。lock なら FOR UPDATE。
func inviteUsable(ctx context.Context, q querier, hash []byte, now time.Time, lock bool) (inviteRow, error) {
	sql := `SELECT id, issuer, role, child, expires_at, uses_left, revoked_at FROM invites WHERE token_sha256 = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var r inviteRow
	var role string
	var child []byte
	var revokedAt *time.Time
	err := q.QueryRow(ctx, sql, hash).Scan(&r.id, &r.issuer, &role, &child, &r.expires, &r.usesLeft, &revokedAt)
	if noRows(err) {
		return inviteRow{}, ev(cgen.AUTH007)
	}
	if err != nil {
		return inviteRow{}, wrap(err)
	}
	if revokedAt != nil || r.usesLeft <= 0 || !r.expires.After(now) {
		return inviteRow{}, ev(cgen.AUTH007)
	}
	r.role, r.expires = boundary.Role(role), r.expires.UTC()
	if child != nil {
		var c childJSON
		if err := json.Unmarshal(child, &c); err != nil {
			return inviteRow{}, wrap(err)
		}
		r.child = &ChildSpec{IsChild: c.IsChild, Guardians: c.Guardians}
	}
	return r, nil
}

// waitUntil は受けてから d が経つまで待つ（成功も失敗も同じ応答の時間。推測の対策）。
func (d *Deps) waitUntil(ctx context.Context, start time.Time, total time.Duration) {
	rest := total - d.now().Sub(start)
	if rest <= 0 {
		return
	}
	t := time.NewTimer(rest)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// Peek は未認証で招待の限られた情報を返す（家族の名前、アイコン、招待者の表示名、期限、子なら保護者の名前）。
// 照合は DB の一意の索引で、応答の前に受けてから 250 ミリ秒まで待つ（成功も失敗も同じ）。使えなければ AUTH-007。
func (i *Invites) Peek(ctx context.Context, token string) (gen.GetInviteTokenResponse, error) {
	start := i.d.now()
	defer i.d.waitUntil(ctx, start, peekDelay)
	hash, ok := tokenHash(token)
	if !ok {
		return gen.GetInviteTokenResponse{}, ev(cgen.AUTH007)
	}
	r, err := inviteUsable(ctx, i.d.Pool, hash, start, false)
	if err != nil {
		return gen.GetInviteTokenResponse{}, err
	}
	fam, err := i.families.Get(ctx)
	if err != nil {
		return gen.GetInviteTokenResponse{}, err
	}
	inviter, err := i.users.Get(ctx, r.issuer)
	if err != nil {
		return gen.GetInviteTokenResponse{}, err
	}
	out := gen.GetInviteTokenResponse{FamilyName: fam.Name, FamilyIconUrl: familyIconUrl, InviterName: inviter.DisplayName, ExpiresAt: r.expires}
	if r.child != nil && r.child.IsChild {
		names := []string{}
		for _, g := range r.child.Guardians {
			gu, err := i.users.Get(ctx, g)
			if err != nil {
				return gen.GetInviteTokenResponse{}, err
			}
			names = append(names, gu.DisplayName)
		}
		out.Child = &struct {
			GuardianNames []string `json:"guardian_names"`
		}{GuardianNames: names}
	}
	return out, nil
}

// familyIconUrl は家族のアイコンの経路（GET /family/icon。192 の any）。
const familyIconUrl = "/api/v1/family/icon"

// Accept は招待を受ける。challenge は kind invite で同じトークンのハッシュの物だけ。構成員、パスキー、最初の端末、
// セッション、hook OnUserCreated の順に作る。写真は受けない（第3章 3.3）。AUTH-007、AUTH-003、MEM-005。
func (i *Invites) Accept(ctx context.Context, tx *store.Tx, token string, challengeId uuid.UUID, name string, passkey gen.WebAuthnResponse, device NewDevice) (AcceptResult, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return AcceptResult{}, ev(cgen.AUTH007)
	}
	inv, err := inviteUsable(ctx, tx, hash, i.d.now(), true)
	if err != nil {
		return AcceptResult{}, err
	}
	c, session, err := i.passkeys.takeChallenge(ctx, tx, challengeId, ChallengeInvite)
	if err != nil {
		return AcceptResult{}, err
	}
	if !bytesEqual(c.TokenSha256, hash) {
		return AcceptResult{}, ev(cgen.AUTH003)
	}
	if e := badDeviceName(device.Name); e != nil {
		return AcceptResult{}, e
	}
	child, err := normalGuardians(ctx, tx, inv.child)
	if err != nil {
		return AcceptResult{}, err
	}
	usr, err := i.users.create(ctx, tx, session.UserID, name, inv.role, child, inv.issuer)
	if err != nil {
		return AcceptResult{}, err
	}
	cred, err := i.passkeys.finishRegister(ctx, tx, webauthnUser{handle: session.UserID, name: name}, session, passkey, usr.Id, nil)
	if err != nil {
		return AcceptResult{}, err
	}
	res, err := firstDevice(ctx, tx, i.d, i.devices, i.sessions, i.hooks, usr, device)
	if err != nil {
		return AcceptResult{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET used_by = $2, used_at = $3, uses_left = uses_left - 1 WHERE id = $1`, inv.id, usr.Id, i.d.now()); err != nil {
		return AcceptResult{}, wrap(err)
	}
	res.User, res.Passkey = usr, cred
	return res, nil
}

// normalGuardians は招待に記録した保護者のうち、受諾の時に状態が通常の人だけを残す（発行から受諾までに停止・削除・抹消された
// 人を保護者にしない。Guardians.Set と同じ条件）。誰も残らなければ保護者のいない子ども区分になる（管理者の画面の印。第3章 9.1）。
func normalGuardians(ctx context.Context, q querier, c *ChildSpec) (*ChildSpec, error) {
	if c == nil || !c.IsChild || len(c.Guardians) == 0 {
		return c, nil
	}
	gs := []uuid.UUID{}
	for _, g := range c.Guardians {
		var st string
		err := q.QueryRow(ctx, `SELECT state FROM users WHERE id = $1`, g).Scan(&st)
		if err != nil && !noRows(err) {
			return nil, wrap(err)
		}
		if err == nil && UserState(st) == UserNormal && !slices.Contains(gs, g) {
			gs = append(gs, g)
		}
	}
	return &ChildSpec{IsChild: true, Guardians: gs}, nil
}

// firstDevice は作ったばかりの構成員の最初の端末とセッションを作り、hook OnUserCreated と家族の全員への
// user.changed と device.changed（added）を出す（設置と招待の受諾）。
func firstDevice(ctx context.Context, tx *store.Tx, d *Deps, devices *Devices, sessions *Sessions, hooks *Hooks, usr User, device NewDevice) (AcceptResult, error) {
	dv, err := devices.Register(ctx, tx, usr.Id, device.Platform, device.Name)
	if err != nil {
		return AcceptResult{}, err
	}
	cookie, sess, err := sessions.Issue(ctx, tx, usr.Id, dv.Id, true)
	if err != nil {
		return AcceptResult{}, err
	}
	if err := hooks.onUserCreated(ctx, tx, usr.Id, dv.Id); err != nil {
		return AcceptResult{}, err
	}
	if err := d.emitUserChanged(ctx, tx, usr.Id, fieldDisplayName, fieldRole, fieldState); err != nil {
		return AcceptResult{}, err
	}
	by := usr.Id
	if err := d.emitDeviceChanged(ctx, tx, usr.Id, dv.Id, changeAdded, &by); err != nil {
		return AcceptResult{}, err
	}
	return AcceptResult{Device: dv, Issued: Issued{Cookie: cookie, Result: gen.LoginResult{NewDevice: true, Session: sess}}}, nil
}

// bytesEqual は 2 つのハッシュが同じか。
func bytesEqual(a, b []byte) bool {
	return len(a) > 0 && string(a) == string(b)
}

// ListMine は発行者の招待（GET /invites）。
func (i *Invites) ListMine(ctx context.Context, userId uuid.UUID, page Page) ([]Invite, bool, error) {
	return i.list(ctx, &userId, page)
}

// ListAll は全員の招待（/admin/invites）。宛名は発行者の DEK で開く（管理者の画面に宛名を出すのは第3章 3.2 の記録の範囲）。
func (i *Invites) ListAll(ctx context.Context, page Page) ([]Invite, bool, error) {
	return i.list(ctx, nil, page)
}

func (i *Invites) list(ctx context.Context, issuer *uuid.UUID, page Page) ([]Invite, bool, error) {
	c, err := page.uuids()
	if err != nil {
		return nil, false, err
	}
	sql := `SELECT id, token_sha256, expires_at, uses_left, role, display_hint, child, issuer, used_by, used_at, revoked_at, created_at FROM invites WHERE true`
	args := []any{}
	if issuer != nil {
		args = append(args, *issuer)
		sql += ` AND issuer = $1`
	}
	sql, args = whereUUID(c, sql, "id", args)
	rows, err := i.d.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, wrap(err)
	}
	type rowT struct {
		inv  Invite
		hint []byte
	}
	var list []rowT
	for rows.Next() {
		var r rowT
		var role string
		var child []byte
		if err := rows.Scan(&r.inv.Id, &r.inv.TokenHash, &r.inv.ExpiresAt, &r.inv.UsesLeft, &role, &r.hint, &child, &r.inv.Issuer,
			&r.inv.UsedBy, &r.inv.UsedAt, &r.inv.RevokedAt, &r.inv.CreatedAt); err != nil {
			rows.Close()
			return nil, false, wrap(err)
		}
		r.inv.Role = boundary.Role(role)
		if child != nil {
			var cj childJSON
			if err := json.Unmarshal(child, &cj); err != nil {
				rows.Close()
				return nil, false, wrap(err)
			}
			r.inv.Child = &ChildSpec{IsChild: cj.IsChild, Guardians: cj.Guardians}
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	list, more := finish(list, c)
	out := make([]Invite, len(list))
	for n, r := range list {
		inv := r.inv
		inv.ExpiresAt, inv.CreatedAt, inv.UsedAt, inv.RevokedAt = inv.ExpiresAt.UTC(), inv.CreatedAt.UTC(), utcPtr(inv.UsedAt), utcPtr(inv.RevokedAt)
		if r.hint != nil {
			pt, err := i.keys.open(ctx, i.d.Pool, inv.Issuer, r.hint, aad("invites", "display_hint", inv.Id.String()))
			var f *fault.Fault
			switch {
			case err == nil:
				s := string(pt)
				inv.DisplayHint = &s
			case errors.As(err, &f) && errors.Is(f.Cause, errNoDek):
				// 発行者が抹消済み（DEK の破棄の後）。宛名は開けない。
			default:
				return nil, false, err
			}
		}
		out[n] = inv
	}
	return out, more, nil
}

// Prune は記録の保持（設定 retention.invite_records。1 年）を過ぎた招待の記録を消す（srv-worker）。
func (i *Invites) Prune(ctx context.Context) (int64, error) {
	return i.d.Retention.DeleteOlder(ctx, "invites", "created_at", i.d.now().Add(-i.d.Config.Get(cgen.KeyRetentionInviteRecords).Duration()))
}

// Page は一覧の引数（第2章 3 の before、after、limit）。
type Page struct {
	Before *string
	After  *string
	Limit  int
}

// SetupState は初回の状態（GET /setup/status）。
type SetupState = gen.GetSetupStatusResponseState

// SetupLink は初回の管理者のリンクを持つ（B-064。第3章 3.1）。
type SetupLink struct {
	d        *Deps
	users    *Users
	families *Families
	passkeys *Passkeys
	devices  *Devices
	sessions *Sessions
	hooks    *Hooks
}

// hasAdmin は管理者の行があるか（第1章 7.1 の 8 の初回の判定）。
func hasAdmin(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&ok)
	return ok, wrap(err)
}

// setupUsable は設置のリンクが使えるか（無い・期限切れ・使用済み・初回でない時は AUTH-007。第3章 3.1 の 2）。lock なら行の錠。
func setupUsable(ctx context.Context, q querier, hash []byte, now time.Time, lock bool) error {
	sql := `SELECT expires_at, used_at FROM setup_links WHERE token_sha256 = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var exp time.Time
	var used *time.Time
	err := q.QueryRow(ctx, sql, hash).Scan(&exp, &used)
	if noRows(err) {
		return ev(cgen.AUTH007)
	}
	if err != nil {
		return wrap(err)
	}
	if used != nil || !exp.After(now) {
		return ev(cgen.AUTH007)
	}
	admin, err := hasAdmin(ctx, q)
	if err != nil {
		return err
	}
	if admin {
		return ev(cgen.AUTH007)
	}
	return nil
}

// Create はリンクのトークンを作る（1 回限り・7 日）。ft setup の最後と ft admin create-link だけが呼ぶ（srv-admin HostTools。G-08）。
// 管理者がいれば作らない（HostTools が先に断る。ここでは Fault）。未使用の古いリンクは消す（作り直しで古いリンクを無効にする）。
func (s *SetupLink) Create(ctx context.Context, tx *store.Tx) (string, error) {
	admin, err := hasAdmin(ctx, tx)
	if err != nil {
		return "", err
	}
	if admin {
		return "", fault.Wrap(errors.New("identity: 管理者がいるので設置のリンクを作らない"))
	}
	if _, err := tx.Exec(ctx, `DELETE FROM setup_links WHERE used_at IS NULL`); err != nil {
		return "", wrap(err)
	}
	tok := s.d.Ids.NewToken()
	defer secret.Zero(tok)
	now := s.d.now()
	if _, err := tx.Exec(ctx, `INSERT INTO setup_links (token_sha256, created_at, expires_at) VALUES ($1, $2, $3)`, ids.HashToken(tok), now, now.Add(setupLinkTtl)); err != nil {
		return "", wrap(err)
	}
	return ids.EncodeToken(tok), nil
}

// Status は /setup/status。サーバーが読み取り専用なら read_only、管理者の行が無ければ first_run（第1章 7.1 の 8）、あれば ready。
func (s *SetupLink) Status(ctx context.Context) (SetupState, error) {
	if s.d.State != nil && s.d.State.State() == boundary.StateReadOnly {
		return gen.ReadOnly, nil
	}
	admin, err := hasAdmin(ctx, s.d.Pool)
	if err != nil {
		return "", err
	}
	if admin {
		return gen.Ready, nil
	}
	return gen.FirstRun, nil
}

// Check は GET /setup/admin/{token}。使えるトークンなら期限。無い・期限切れ・使用済み・初回でない時は AUTH-007。
func (s *SetupLink) Check(ctx context.Context, token string) (gen.GetSetupAdminResponse, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return gen.GetSetupAdminResponse{}, ev(cgen.AUTH007)
	}
	if err := setupUsable(ctx, s.d.Pool, hash, s.d.now(), false); err != nil {
		return gen.GetSetupAdminResponse{}, err
	}
	var exp time.Time
	err := s.d.Pool.QueryRow(ctx, `SELECT expires_at FROM setup_links WHERE token_sha256 = $1`, hash).Scan(&exp)
	return gen.GetSetupAdminResponse{ExpiresAt: exp.UTC()}, wrap(err)
}

// Consume は家族、管理者、パスキー、最初の端末、セッションを作る。初回でなければ AUTH-007（同時の 2 回はリンクの行の錠で直列）。
// challenge は kind setup。家族のアイコンはコミットの後に作る。写真、回復コード、カードは作成の後（第3章 3.1）。
func (s *SetupLink) Consume(ctx context.Context, tx *store.Tx, token string, challengeId uuid.UUID, familyName, adminName string, passkey gen.WebAuthnResponse, device NewDevice) (AcceptResult, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return AcceptResult{}, ev(cgen.AUTH007)
	}
	if err := setupUsable(ctx, tx, hash, s.d.now(), true); err != nil {
		return AcceptResult{}, err
	}
	c, session, err := s.passkeys.takeChallenge(ctx, tx, challengeId, ChallengeSetup)
	if err != nil {
		return AcceptResult{}, err
	}
	if !bytesEqual(c.TokenSha256, hash) {
		return AcceptResult{}, ev(cgen.AUTH003)
	}
	if e := badDeviceName(device.Name); e != nil {
		return AcceptResult{}, e
	}
	if _, err := tx.Exec(ctx, `UPDATE setup_links SET used_at = $2 WHERE token_sha256 = $1`, hash, s.d.now()); err != nil {
		return AcceptResult{}, wrap(err)
	}
	fam, err := s.families.create(ctx, tx, familyName)
	if err != nil {
		return AcceptResult{}, err
	}
	usr, err := s.users.create(ctx, tx, session.UserID, adminName, boundary.RoleAdmin, nil, uuid.Nil)
	if err != nil {
		return AcceptResult{}, err
	}
	cred, err := s.passkeys.finishRegister(ctx, tx, webauthnUser{handle: session.UserID, name: adminName}, session, passkey, usr.Id, nil)
	if err != nil {
		return AcceptResult{}, err
	}
	res, err := firstDevice(ctx, tx, s.d, s.devices, s.sessions, s.hooks, usr, device)
	if err != nil {
		return AcceptResult{}, err
	}
	tx.AfterCommit(func() {
		for _, sz := range iconSizes {
			_ = s.families.icon.ensure(fam.Color, sz.size, sz.variant)
		}
	})
	res.User, res.Passkey = usr, cred
	return res, nil
}
