// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"context"
	"encoding/base64"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Apis は本ブロックが実装する生成のタグの interface（ブロックのページ srv-identity の受ける節）。gen.Implementations の 7 つの欄に渡す。
// 各操作は主体を boundary.PrincipalFrom(ctx) で取り、書き込みは store.TxFrom(ctx) のトランザクション（無ければ開く）で行う。
// *fault.Event はそのまま error で返す（srv-boundary の strict の口が problem にする）。x-reauth の 5 分と x-authz-rule は本ブロックが判定する。
type Apis struct {
	Setup   *SetupApi
	Invites *InvitesApi
	Auth    *AuthApi
	Me      *MeApi
	Family  *FamilyApi
	Users   *UsersApi
	Devices *DevicesApi
}

// parseId は経路と本文の識別子を読む（形は srv-boundary の検証が済ませている。読めなければ NET-001）。
func parseId(s, path string) (uuid.UUID, error) {
	u, err := uuid.FromString(s)
	if err != nil {
		return uuid.Nil, fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": path, "message": "UUIDv7 でない"}}})
	}
	return u, nil
}

// page は生成の一覧の引数から Page を作る。
func page(limit *int, before, after *string) Page {
	p := Page{Before: before, After: after}
	if limit != nil {
		p.Limit = *limit
	}
	return p
}

// cookieOf は Set-Cookie の値（期限はセッションの絶対の期限）。
func (id *Identity) cookieOf(is Issued) *string {
	v := SessionCookie(is.Cookie, is.Result.Session.ExpiresAt, id.d.now())
	return &v
}

// SetupApi はタグ setup の操作。
type SetupApi struct {
	id *Identity
}

var _ gen.SetupApi = (*SetupApi)(nil)

// GetSetupAdmin は GET /api/v1/setup/admin/{token}。SetupLink.Check。
func (a *SetupApi) GetSetupAdmin(ctx context.Context, request gen.GetSetupAdminRequestObject) (gen.GetSetupAdminResponseObject, error) {
	r, err := a.id.Setup.Check(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	return gen.GetSetupAdmin200JSONResponse(r), nil
}

// CreateSetupAdmin は POST /api/v1/setup/admin/{token}。SetupLink.Consume。応答の Set-Cookie は SessionCookie。
func (a *SetupApi) CreateSetupAdmin(ctx context.Context, request gen.CreateSetupAdminRequestObject) (gen.CreateSetupAdminResponseObject, error) {
	b := request.Body
	cid, err := parseId(b.ChallengeId, "body/challenge_id")
	if err != nil {
		return nil, err
	}
	var res AcceptResult
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		res, err = a.id.Setup.Consume(ctx, tx, request.Token, cid, b.FamilyName, b.DisplayName, b.Passkey,
			NewDevice{Name: b.DeviceName, Platform: Platform(b.Platform)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateSetupAdmin200JSONResponse{Body: res.Issued.Result, Headers: gen.CreateSetupAdmin200ResponseHeaders{SetCookie: a.id.cookieOf(res.Issued)}}, nil
}

// GetSetupStatus は GET /api/v1/setup/status。SetupLink.Status。
func (a *SetupApi) GetSetupStatus(ctx context.Context, request gen.GetSetupStatusRequestObject) (gen.GetSetupStatusResponseObject, error) {
	st, err := a.id.Setup.Status(ctx)
	if err != nil {
		return nil, err
	}
	return gen.GetSetupStatus200JSONResponse{State: st}, nil
}

// InvitesApi はタグ invites の操作。
type InvitesApi struct {
	id *Identity
}

var _ gen.InvitesApi = (*InvitesApi)(nil)

// GetInviteToken は GET /api/v1/invite-tokens/{token}。Invites.Peek。
func (a *InvitesApi) GetInviteToken(ctx context.Context, request gen.GetInviteTokenRequestObject) (gen.GetInviteTokenResponseObject, error) {
	r, err := a.id.Invites.Peek(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	return gen.GetInviteToken200JSONResponse(r), nil
}

// AcceptInviteToken は POST /api/v1/invite-tokens/{token}/accept。Invites.Accept。応答の Set-Cookie は SessionCookie。
func (a *InvitesApi) AcceptInviteToken(ctx context.Context, request gen.AcceptInviteTokenRequestObject) (gen.AcceptInviteTokenResponseObject, error) {
	b := request.Body
	cid, err := parseId(b.ChallengeId, "body/challenge_id")
	if err != nil {
		return nil, err
	}
	var res AcceptResult
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		res, err = a.id.Invites.Accept(ctx, tx, request.Token, cid, b.DisplayName, b.Passkey, NewDevice{Name: b.DeviceName, Platform: Platform(b.Platform)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.AcceptInviteToken200JSONResponse{Body: res.Issued.Result, Headers: gen.AcceptInviteToken200ResponseHeaders{SetCookie: a.id.cookieOf(res.Issued)}}, nil
}

// inviteView は Invite を応答の形にする。
func inviteView(inv Invite) gen.Invite {
	out := gen.Invite{Id: inv.Id.String(), Issuer: inv.Issuer.String(), Role: gen.InviteRole(inv.Role), DisplayHint: inv.DisplayHint,
		CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt, UsesLeft: inv.UsesLeft, UsedAt: inv.UsedAt, RevokedAt: inv.RevokedAt}
	if inv.UsedBy != nil {
		s := inv.UsedBy.String()
		out.UsedBy = &s
	}
	if inv.Child != nil {
		gs := make([]string, len(inv.Child.Guardians))
		for i, g := range inv.Child.Guardians {
			gs[i] = g.String()
		}
		out.Child = &struct {
			Guardians *[]gen.CommonUuidv71 `json:"guardians,omitempty"`
			IsChild   bool                 `json:"is_child"`
		}{Guardians: &gs, IsChild: inv.Child.IsChild}
	}
	return out
}

// ListInvites は GET /api/v1/invites。規則 invite_issue（MEM-007）。一覧は自分の発行分、管理者は全部（第2章 5.1）。
func (a *InvitesApi) ListInvites(ctx context.Context, request gen.ListInvitesRequestObject) (gen.ListInvitesResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, err
	}
	usr, err := a.id.Users.Get(ctx, a.id.d.Pool, me)
	if err != nil {
		return nil, err
	}
	pg := page(request.Params.Limit, request.Params.Before, request.Params.After)
	var list []Invite
	var more bool
	if usr.Role == boundary.RoleAdmin {
		list, more, err = a.id.Invites.ListAll(ctx, pg)
	} else {
		if err := a.id.Invites.mayIssue(ctx, a.id.d.Pool, usr, boundary.RoleMember); err != nil {
			return nil, err
		}
		list, more, err = a.id.Invites.ListMine(ctx, me, pg)
	}
	if err != nil {
		return nil, err
	}
	items := make([]gen.Invite, len(list))
	for i, inv := range list {
		items[i] = inviteView(inv)
	}
	return gen.ListInvites200JSONResponse{Items: items, HasMore: more}, nil
}

// CreateInvite は POST /api/v1/invites。Invites.Issue。応答の url は /i/<token>（第1章 10.10）。
func (a *InvitesApi) CreateInvite(ctx context.Context, request gen.CreateInviteRequestObject) (gen.CreateInviteResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	b := request.Body
	spec := InviteSpec{Role: boundary.Role(b.Role), DisplayHint: b.DisplayHint}
	if b.Child != nil && b.Child.IsChild {
		spec.Child = &ChildSpec{IsChild: true}
		if b.Child.Guardians != nil {
			for _, g := range *b.Child.Guardians {
				gid, err := parseId(g, "body/child/guardians")
				if err != nil {
					return nil, err
				}
				spec.Child.Guardians = append(spec.Child.Guardians, gid)
			}
		}
	}
	var inv Invite
	var token string
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		inv, token, err = a.id.Invites.Issue(ctx, tx, me, spec)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateInvite200JSONResponse{Invite: inviteView(inv), Url: a.id.d.origin() + "/i/" + token}, nil
}

// DeleteInvite は DELETE /api/v1/invites/{id}。Invites.Revoke（規則 invite_issuer_or_admin）。
func (a *InvitesApi) DeleteInvite(ctx context.Context, request gen.DeleteInviteRequestObject) (gen.DeleteInviteResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	iid, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Invites.Revoke(ctx, tx, iid, me) }); err != nil {
		return nil, err
	}
	return gen.DeleteInvite200JSONResponse{}, nil
}

// AuthApi はタグ auth の操作。
type AuthApi struct {
	id *Identity
}

var _ gen.AuthApi = (*AuthApi)(nil)

// Logout は POST /api/v1/auth/logout。Sessions.Revoke(logout)。応答の Set-Cookie は ClearSessionCookie。
func (a *AuthApi) Logout(ctx context.Context, request gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	if p.SessionId == nil {
		return nil, ev(cgen.AUTH001)
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Sessions.Revoke(ctx, tx, *p.SessionId, RevokeLogout) }); err != nil {
		return nil, err
	}
	clear := ClearSessionCookie()
	return gen.Logout200JSONResponse{Body: gen.LogoutResponse{}, Headers: gen.Logout200ResponseHeaders{SetCookie: &clear}}, nil
}

// CreatePasskeyLoginOptions は POST /api/v1/auth/passkey/login/options。Passkeys.LoginOptions。
func (a *AuthApi) CreatePasskeyLoginOptions(ctx context.Context, request gen.CreatePasskeyLoginOptionsRequestObject) (gen.CreatePasskeyLoginOptionsResponseObject, error) {
	r, err := a.id.Passkeys.LoginOptions(ctx)
	if err != nil {
		return nil, err
	}
	return gen.CreatePasskeyLoginOptions200JSONResponse(r), nil
}

// VerifyPasskeyLogin は POST /api/v1/auth/passkey/login/verify。Passkeys.LoginVerify。応答の Set-Cookie は SessionCookie。
// device_hint と device_sig は両方か無し（要求の検証の dependentRequired）。
func (a *AuthApi) VerifyPasskeyLogin(ctx context.Context, request gen.VerifyPasskeyLoginRequestObject) (gen.VerifyPasskeyLoginResponseObject, error) {
	b := request.Body
	cid, err := parseId(b.ChallengeId, "body/challenge_id")
	if err != nil {
		return nil, err
	}
	var proof *DeviceProof
	if b.DeviceHint != nil && b.DeviceSig != nil {
		did, err := parseId(*b.DeviceHint, "body/device_hint")
		if err != nil {
			return nil, err
		}
		sig, err := base64.RawURLEncoding.DecodeString(*b.DeviceSig)
		if err != nil {
			return nil, fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": "body/device_sig", "message": "base64url でない"}}})
		}
		proof = &DeviceProof{DeviceId: did, Sig: sig}
	}
	var is Issued
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		is, err = a.id.Passkeys.LoginVerify(ctx, tx, cid, b.Response, proof, NewDevice{Name: b.DeviceName, Platform: Platform(b.Platform)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.VerifyPasskeyLogin200JSONResponse{Body: is.Result, Headers: gen.VerifyPasskeyLogin200ResponseHeaders{SetCookie: a.id.cookieOf(is)}}, nil
}

// CreatePasskeyRegisterOptions は POST /api/v1/auth/passkey/register/options。Passkeys.RegisterOptions。
// トークンは invite、setup、reset のどれか 1 つ（2 つ以上は NET-001）。無ければログインの後の追加（主体が要る）。
func (a *AuthApi) CreatePasskeyRegisterOptions(ctx context.Context, request gen.CreatePasskeyRegisterOptionsRequestObject) (gen.CreatePasskeyRegisterOptionsResponseObject, error) {
	b := request.Body
	var tok *RegToken
	n := 0
	for _, t := range []struct {
		v    *string
		kind ChallengeKind
	}{{b.InviteToken, ChallengeInvite}, {b.SetupToken, ChallengeSetup}, {b.ResetToken, ChallengeReset}} {
		if t.v != nil {
			n++
			tok = &RegToken{Kind: t.kind, Token: *t.v}
		}
	}
	if n > 1 {
		return nil, fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": "body", "message": "トークンは 1 つだけ"}}})
	}
	var p *boundary.Principal
	if tok == nil {
		pr := boundary.PrincipalFrom(ctx)
		p = &pr
	}
	r, err := a.id.Passkeys.RegisterOptions(ctx, p, tok)
	if err != nil {
		return nil, err
	}
	return gen.CreatePasskeyRegisterOptions200JSONResponse(r), nil
}

// passkeyView は Credential を応答の形にする。
func passkeyView(c Credential, backedUp bool) gen.Passkey {
	return gen.Passkey{Id: base64.RawURLEncoding.EncodeToString(c.Id), Name: c.Name, CreatedAt: c.CreatedAt, LastUsedAt: c.LastUsedAt,
		BackedUp: backedUp, Prf: c.Prf}
}

// VerifyPasskeyRegister は POST /api/v1/auth/passkey/register/verify。Passkeys.RegisterVerify（ログインの後の追加）。
// 既にパスキーがあれば再認証（x-reauth conditional。AUTH-006）。主体が無ければ AUTH-003（challenge は本人の物だけ）。
// 端末が未承認なら KEY-004（認可 approved。srv-boundary はトークンの値がある経路を通すので本ブロックが確かめる）。
func (a *AuthApi) VerifyPasskeyRegister(ctx context.Context, request gen.VerifyPasskeyRegisterRequestObject) (gen.VerifyPasskeyRegisterResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, ev(cgen.AUTH003)
	}
	if !p.DeviceApproved {
		return nil, ev(cgen.KEY004)
	}
	creds, err := credsOf(ctx, a.id.d.Pool, me)
	if err != nil {
		return nil, err
	}
	if len(creds) > 0 {
		if err := a.id.d.requireReauth(p); err != nil {
			return nil, err
		}
	}
	b := request.Body
	cid, err := parseId(b.ChallengeId, "body/challenge_id")
	if err != nil {
		return nil, err
	}
	var c Credential
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		c, err = a.id.Passkeys.RegisterVerify(ctx, tx, me, cid, b.Response, b.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.VerifyPasskeyRegister200JSONResponse{Passkey: passkeyView(c, backedUpOf(c.Record))}, nil
}

// ApproveQrLogin は POST /api/v1/auth/qr/approve。QrFlow.Approve。
func (a *AuthApi) ApproveQrLogin(ctx context.Context, request gen.ApproveQrLoginRequestObject) (gen.ApproveQrLoginResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Qr.Approve(ctx, tx, p, request.Body.UserCode) }); err != nil {
		return nil, err
	}
	return gen.ApproveQrLogin200JSONResponse{}, nil
}

// PollQrLogin は POST /api/v1/auth/qr/poll。QrFlow.Poll。応答の Set-Cookie は SessionCookie。
func (a *AuthApi) PollQrLogin(ctx context.Context, request gen.PollQrLoginRequestObject) (gen.PollQrLoginResponseObject, error) {
	var is Issued
	err := a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		is, err = a.id.Qr.Poll(ctx, tx, request.Body.DeviceCode)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.PollQrLogin200JSONResponse{Body: is.Result, Headers: gen.PollQrLogin200ResponseHeaders{SetCookie: a.id.cookieOf(is)}}, nil
}

// StartQrLogin は POST /api/v1/auth/qr/start。QrFlow.Start（送り元の IP は ClientIpFrom）。
func (a *AuthApi) StartQrLogin(ctx context.Context, request gen.StartQrLoginRequestObject) (gen.StartQrLoginResponseObject, error) {
	b := request.Body
	s := QrStart{Name: b.DeviceName, Platform: Platform(b.Platform), SigPub: b.SigPub, KemPub: b.KemPub, Ip: boundary.ClientIpFrom(ctx)}
	var r gen.StartQrLoginResponse
	err := a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		r, err = a.id.Qr.Start(ctx, tx, s)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.StartQrLogin200JSONResponse(r), nil
}

// GetQrLogin は GET /api/v1/auth/qr/{user_code}。QrFlow.Info。
func (a *AuthApi) GetQrLogin(ctx context.Context, request gen.GetQrLoginRequestObject) (gen.GetQrLoginResponseObject, error) {
	r, err := a.id.Qr.Info(ctx, boundary.PrincipalFrom(ctx), request.UserCode)
	if err != nil {
		return nil, err
	}
	return gen.GetQrLogin200JSONResponse(r), nil
}

// GetPasskeyReset は GET /api/v1/auth/reset/{token}。ResetLink.Peek。
func (a *AuthApi) GetPasskeyReset(ctx context.Context, request gen.GetPasskeyResetRequestObject) (gen.GetPasskeyResetResponseObject, error) {
	r, err := a.id.Reset.Peek(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	return gen.GetPasskeyReset200JSONResponse(r), nil
}

// ResetPasskey は POST /api/v1/auth/reset/{token}。ResetLink.Consume。応答の Set-Cookie は SessionCookie。
func (a *AuthApi) ResetPasskey(ctx context.Context, request gen.ResetPasskeyRequestObject) (gen.ResetPasskeyResponseObject, error) {
	b := request.Body
	cid, err := parseId(b.ChallengeId, "body/challenge_id")
	if err != nil {
		return nil, err
	}
	var is Issued
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		is, err = a.id.Reset.Consume(ctx, tx, request.Token, cid, b.Passkey, NewDevice{Name: b.DeviceName, Platform: Platform(b.Platform)})
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.ResetPasskey200JSONResponse{Body: is.Result, Headers: gen.ResetPasskey200ResponseHeaders{SetCookie: a.id.cookieOf(is)}}, nil
}

// CreateShellToken は POST /api/v1/auth/shell-token。Sessions.IssueShellToken。
func (a *AuthApi) CreateShellToken(ctx context.Context, request gen.CreateShellTokenRequestObject) (gen.CreateShellTokenResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	var g gen.TokenGrant
	err := a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		g, err = a.id.Sessions.IssueShellToken(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateShellToken200JSONResponse(g), nil
}

// CreateWsToken は POST /api/v1/auth/ws-token。Sessions.IssueWsToken。
func (a *AuthApi) CreateWsToken(ctx context.Context, request gen.CreateWsTokenRequestObject) (gen.CreateWsTokenResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	var g gen.TokenGrant
	err := a.id.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		g, err = a.id.Sessions.IssueWsToken(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateWsToken200JSONResponse(g), nil
}

// ListMyPasskeys は GET /api/v1/me/passkeys。Passkeys.List。
func (a *AuthApi) ListMyPasskeys(ctx context.Context, request gen.ListMyPasskeysRequestObject) (gen.ListMyPasskeysResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	items, more, err := a.id.Passkeys.List(ctx, me, request.Params.Limit, request.Params.Before, request.Params.After)
	if err != nil {
		return nil, err
	}
	return gen.ListMyPasskeys200JSONResponse{Items: items, HasMore: more}, nil
}

// DeleteMyPasskey は DELETE /api/v1/me/passkeys/{id}（id は資格情報の識別子の base64url）。再認証（AUTH-006）。Passkeys.Delete。
func (a *AuthApi) DeleteMyPasskey(ctx context.Context, request gen.DeleteMyPasskeyRequestObject) (gen.DeleteMyPasskeyResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, err
	}
	if err := a.id.d.requireReauth(p); err != nil {
		return nil, err
	}
	cred, err := base64.RawURLEncoding.DecodeString(request.Id)
	if err != nil {
		return nil, fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": "path.id", "message": "base64url でない"}}})
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Passkeys.Delete(ctx, tx, me, cred) }); err != nil {
		return nil, err
	}
	return gen.DeleteMyPasskey200JSONResponse{}, nil
}

// MeApi はタグ me の操作。
type MeApi struct {
	id *Identity
}

var _ gen.MeApi = (*MeApi)(nil)

// GetMe は GET /api/v1/me。Users.Me。
func (a *MeApi) GetMe(ctx context.Context, request gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	r, err := a.id.Users.Me(ctx, me)
	if err != nil {
		return nil, err
	}
	return gen.GetMe200JSONResponse(r), nil
}

// UpdateMe は PATCH /api/v1/me。Users.UpdateProfile。応答は本人の User。
func (a *MeApi) UpdateMe(ctx context.Context, request gen.UpdateMeRequestObject) (gen.UpdateMeResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Users.UpdateProfile(ctx, tx, me, *request.Body) }); err != nil {
		return nil, err
	}
	r, err := a.id.Users.Me(ctx, me)
	if err != nil {
		return nil, err
	}
	return gen.UpdateMe200JSONResponse(r.User), nil
}

// DeleteMyAlias は DELETE /api/v1/me/aliases/{user_id}。Users.SetAlias(nil)。
func (a *MeApi) DeleteMyAlias(ctx context.Context, request gen.DeleteMyAliasRequestObject) (gen.DeleteMyAliasResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	target, err := parseId(request.UserId, "path.user_id")
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Users.SetAlias(ctx, tx, me, target, nil) }); err != nil {
		return nil, err
	}
	return gen.DeleteMyAlias200JSONResponse{}, nil
}

// PutMyAlias は PUT /api/v1/me/aliases/{user_id}。Users.SetAlias。
func (a *MeApi) PutMyAlias(ctx context.Context, request gen.PutMyAliasRequestObject) (gen.PutMyAliasResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	target, err := parseId(request.UserId, "path.user_id")
	if err != nil {
		return nil, err
	}
	alias := request.Body.Alias
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Users.SetAlias(ctx, tx, me, target, &alias) }); err != nil {
		return nil, err
	}
	return gen.PutMyAlias200JSONResponse{}, nil
}

// AckDisclosure は POST /api/v1/me/disclosure-ack。Users.AckDisclosure。
func (a *MeApi) AckDisclosure(ctx context.Context, request gen.AckDisclosureRequestObject) (gen.AckDisclosureResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Users.AckDisclosure(ctx, tx, me, request.Body.Version) }); err != nil {
		return nil, err
	}
	return gen.AckDisclosure200JSONResponse{}, nil
}

// CancelEraseMe は DELETE /api/v1/me/erase。Users.CancelErase。
func (a *MeApi) CancelEraseMe(ctx context.Context, request gen.CancelEraseMeRequestObject) (gen.CancelEraseMeResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Users.CancelErase(ctx, tx, me) }); err != nil {
		return nil, err
	}
	return gen.CancelEraseMe200JSONResponse{}, nil
}

// EraseMe は POST /api/v1/me/erase。再認証（AUTH-006）。Users.RequestErase。
func (a *MeApi) EraseMe(ctx context.Context, request gen.EraseMeRequestObject) (gen.EraseMeResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, err
	}
	if err := a.id.d.requireReauth(p); err != nil {
		return nil, err
	}
	var r gen.EraseMeResponse
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		at, err := a.id.Users.RequestErase(ctx, tx, me)
		r.EraseAt = at
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.EraseMe200JSONResponse(r), nil
}

// ApproveAsGuardian は POST /api/v1/me/guardian/approvals/{id}。approve が真なら Guardians.Approve、偽なら Guardians.Deny。
func (a *MeApi) ApproveAsGuardian(ctx context.Context, request gen.ApproveAsGuardianRequestObject) (gen.ApproveAsGuardianResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	aid, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		if request.Body.Approve {
			return a.id.Guardians.Approve(ctx, tx, me, aid)
		}
		return a.id.Guardians.Deny(ctx, tx, me, aid)
	})
	if err != nil {
		return nil, err
	}
	return gen.ApproveAsGuardian200JSONResponse{}, nil
}

// GetChildControls は GET /api/v1/me/guardian/children/{id}/controls。規則 guardian。Guardians.Settings。
func (a *MeApi) GetChildControls(ctx context.Context, request gen.GetChildControlsRequestObject) (gen.GetChildControlsResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	child, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if err := a.id.Guardians.authorize(ctx, me, child); err != nil {
		return nil, err
	}
	s, err := a.id.Guardians.Settings(ctx, a.id.d.Pool, child)
	if err != nil {
		return nil, err
	}
	return gen.GetChildControls200JSONResponse(s), nil
}

// PutChildBirthday は PUT /api/v1/me/guardian/children/{id}/birthday。Guardians.SetBirthday（第3章 9.1。再認証、規則 guardian）。
func (a *MeApi) PutChildBirthday(ctx context.Context, request gen.PutChildBirthdayRequestObject) (gen.PutChildBirthdayResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, err
	}
	if err := a.id.d.requireReauth(p); err != nil {
		return nil, err
	}
	child, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if err := a.id.Guardians.authorize(ctx, me, child); err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Guardians.SetBirthday(ctx, tx, child, request.Body.Birthday) }); err != nil {
		return nil, err
	}
	return gen.PutChildBirthday200JSONResponse{}, nil
}

// PutChildControls は PUT /api/v1/me/guardian/children/{id}/controls。再認証（AUTH-006）、規則 guardian。Guardians.PutSettings。
func (a *MeApi) PutChildControls(ctx context.Context, request gen.PutChildControlsRequestObject) (gen.PutChildControlsResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, err
	}
	if err := a.id.d.requireReauth(p); err != nil {
		return nil, err
	}
	child, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if err := a.id.Guardians.authorize(ctx, me, child); err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Guardians.PutSettings(ctx, tx, child, me, *request.Body) }); err != nil {
		return nil, err
	}
	s, err := a.id.Guardians.Settings(ctx, a.id.d.Pool, child)
	if err != nil {
		return nil, err
	}
	return gen.PutChildControls200JSONResponse(s), nil
}

// MarkRecoveryCardHeld は POST /api/v1/me/recovery-cards/{n}。Families.MarkCardHeld。
func (a *MeApi) MarkRecoveryCardHeld(ctx context.Context, request gen.MarkRecoveryCardHeldRequestObject) (gen.MarkRecoveryCardHeldResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Families.MarkCardHeld(ctx, tx, me, request.N) }); err != nil {
		return nil, err
	}
	return gen.MarkRecoveryCardHeld200JSONResponse{}, nil
}

// CreateReport は POST /api/v1/me/report（シェルのトークンも可）。Reports.Create。
func (a *MeApi) CreateReport(ctx context.Context, request gen.CreateReportRequestObject) (gen.CreateReportResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Reports.Create(ctx, tx, p, *request.Body) }); err != nil {
		return nil, err
	}
	return gen.CreateReport200JSONResponse{}, nil
}

// GetMySession は GET /api/v1/me/session。Sessions.Describe。
func (a *MeApi) GetMySession(ctx context.Context, request gen.GetMySessionRequestObject) (gen.GetMySessionResponseObject, error) {
	s, err := a.id.Sessions.Describe(ctx, boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	return gen.GetMySession200JSONResponse(s), nil
}

// GetMySettings は GET /api/v1/me/settings。Users.Settings。
func (a *MeApi) GetMySettings(ctx context.Context, request gen.GetMySettingsRequestObject) (gen.GetMySettingsResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	s, err := a.id.Users.Settings(ctx, me)
	if err != nil {
		return nil, err
	}
	return gen.GetMySettings200JSONResponse(s), nil
}

// PutMySettings は PUT /api/v1/me/settings。Users.PutSettings。応答は置き換えた後の全部の項目。
func (a *MeApi) PutMySettings(ctx context.Context, request gen.PutMySettingsRequestObject) (gen.PutMySettingsResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Users.PutSettings(ctx, tx, me, *request.Body) }); err != nil {
		return nil, err
	}
	s, err := a.id.Users.Settings(ctx, me)
	if err != nil {
		return nil, err
	}
	return gen.PutMySettings200JSONResponse(s), nil
}

// AcceptAdminTransfer は POST /api/v1/me/transfer/{id}/accept。規則 transfer_target。Users.AcceptTransfer。
func (a *MeApi) AcceptAdminTransfer(ctx context.Context, request gen.AcceptAdminTransferRequestObject) (gen.AcceptAdminTransferResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	tid, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Users.AcceptTransfer(ctx, tx, p, tid) }); err != nil {
		return nil, err
	}
	return gen.AcceptAdminTransfer200JSONResponse{}, nil
}

// FamilyApi はタグ family の操作。
type FamilyApi struct {
	id *Identity
}

var _ gen.FamilyApi = (*FamilyApi)(nil)

// GetFamily は GET /api/v1/family。規則 family_fields：未認証は名前とアイコンだけ、本人（セッション）は全部（第2章 5.3）。
func (a *FamilyApi) GetFamily(ctx context.Context, request gen.GetFamilyRequestObject) (gen.GetFamilyResponseObject, error) {
	fam, err := a.id.Families.Get(ctx)
	if err != nil {
		return nil, err
	}
	out := gen.Family{Name: fam.Name, IconUrl: familyIconUrl}
	if p := boundary.PrincipalFrom(ctx); p.Kind == boundary.KindUser && p.UserId != nil {
		names := fam.AdminNames()
		if names == nil {
			names = []string{}
		}
		members, err := familyMembers(ctx, a.id.d.Pool)
		if err != nil {
			return nil, err
		}
		n := len(members)
		created := fam.InstalledAt
		out.AdminNames, out.MemberCount, out.CreatedAt = &names, &n, &created
	}
	return gen.GetFamily200JSONResponse(out), nil
}

// GetFamilyDisclosure は GET /api/v1/family/disclosure。Families.Disclosure。
func (a *FamilyApi) GetFamilyDisclosure(ctx context.Context, request gen.GetFamilyDisclosureRequestObject) (gen.GetFamilyDisclosureResponseObject, error) {
	d, err := a.id.Families.Disclosure(ctx)
	if err != nil {
		return nil, err
	}
	return gen.GetFamilyDisclosure200JSONResponse(d), nil
}

// GetFamilyIcon は GET /api/v1/family/icon。192 の any（無ければ作る）。
func (a *FamilyApi) GetFamilyIcon(ctx context.Context, request gen.GetFamilyIconRequestObject) (gen.GetFamilyIconResponseObject, error) {
	fam, err := a.id.Families.Get(ctx)
	if err != nil {
		return nil, err
	}
	b, err := a.id.Families.icon.read(fam.Color, 192, IconAny)
	if err != nil {
		return nil, err
	}
	return gen.GetFamilyIcon200ImagepngResponse{Body: bytes.NewReader(b), ContentLength: int64(len(b))}, nil
}

// GetFamilyRecoveryCards は GET /api/v1/family/recovery-cards。Families.Cards。
func (a *FamilyApi) GetFamilyRecoveryCards(ctx context.Context, request gen.GetFamilyRecoveryCardsRequestObject) (gen.GetFamilyRecoveryCardsResponseObject, error) {
	cards, err := a.id.Families.Cards(ctx)
	if err != nil {
		return nil, err
	}
	var out gen.GetFamilyRecoveryCardsResponse
	for _, c := range cards {
		var holder *string
		if c.HolderId != nil {
			s := c.HolderId.String()
			holder = &s
		}
		out.Cards = append(out.Cards, struct {
			HeldAt   *gen.CommonRfc33391 `json:"held_at,omitempty"`
			HolderId *gen.CommonUuidv71  `json:"holder_id,omitempty"`
			No       int                 `json:"no"`
		}{HeldAt: c.HeldAt, HolderId: holder, No: c.No})
	}
	return gen.GetFamilyRecoveryCards200JSONResponse(out), nil
}

// GetManifest は GET /manifest.webmanifest。Families.Manifest。
func (a *FamilyApi) GetManifest(ctx context.Context, request gen.GetManifestRequestObject) (gen.GetManifestResponseObject, error) {
	m, err := a.id.Families.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	return gen.GetManifest200ApplicationManifestPlusJSONResponse(m), nil
}

// UsersApi はタグ users の操作。
type UsersApi struct {
	id *Identity
}

var _ gen.UsersApi = (*UsersApi)(nil)

// ListUsers は GET /api/v1/users。Users.List。
func (a *UsersApi) ListUsers(ctx context.Context, request gen.ListUsersRequestObject) (gen.ListUsersResponseObject, error) {
	items, more, err := a.id.Users.List(ctx, a.id.d.Pool, boundary.PrincipalFrom(ctx), page(request.Params.Limit, request.Params.Before, request.Params.After))
	if err != nil {
		return nil, err
	}
	return gen.ListUsers200JSONResponse{Items: items, HasMore: more}, nil
}

// GetUser は GET /api/v1/users/{id}。Users.Detail。
func (a *UsersApi) GetUser(ctx context.Context, request gen.GetUserRequestObject) (gen.GetUserResponseObject, error) {
	uid, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	r, err := a.id.Users.Detail(ctx, boundary.PrincipalFrom(ctx), uid)
	if err != nil {
		return nil, err
	}
	return gen.GetUser200JSONResponse(r), nil
}

// DevicesApi はタグ devices の操作。
type DevicesApi struct {
	id *Identity
}

var _ gen.DevicesApi = (*DevicesApi)(nil)

// ListMyDevices は GET /api/v1/me/devices。Devices.List。
func (a *DevicesApi) ListMyDevices(ctx context.Context, request gen.ListMyDevicesRequestObject) (gen.ListMyDevicesResponseObject, error) {
	me, dev, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	items, more, err := a.id.Devices.List(ctx, a.id.d.Pool, me, dev, page(request.Params.Limit, request.Params.Before, request.Params.After))
	if err != nil {
		return nil, err
	}
	return gen.ListMyDevices200JSONResponse{Items: items, HasMore: more}, nil
}

// DeleteMyDevice は DELETE /api/v1/me/devices/{id}。Devices.Revoke。自分の端末なら self（再認証なし、応答の Set-Cookie は
// ClearSessionCookie。第3章 4.5）、他の端末は再認証（x-reauth conditional。AUTH-006）。本人の物でなければ KEY-001。
func (a *DevicesApi) DeleteMyDevice(ctx context.Context, request gen.DeleteMyDeviceRequestObject) (gen.DeleteMyDeviceResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, dev, err := self(p)
	if err != nil {
		return nil, err
	}
	did, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	own := did == dev
	if !own {
		if err := a.id.d.requireReauth(p); err != nil {
			return nil, err
		}
	}
	err = a.id.d.inTx(ctx, func(tx *store.Tx) error {
		if _, err := a.id.Devices.mine(ctx, tx, me, did, true); err != nil {
			return err
		}
		return a.id.Devices.Revoke(ctx, tx, did, me, own)
	})
	if err != nil {
		return nil, err
	}
	out := gen.DeleteMyDevice200JSONResponse{Body: gen.DeleteMyDeviceResponse{}}
	if own {
		clear := ClearSessionCookie()
		out.Headers.SetCookie = &clear
	}
	return out, nil
}

// UpdateMyDevice は PATCH /api/v1/me/devices/{id}。Devices.Rename。応答はその端末。
func (a *DevicesApi) UpdateMyDevice(ctx context.Context, request gen.UpdateMyDeviceRequestObject) (gen.UpdateMyDeviceResponseObject, error) {
	me, dev, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	did, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	if err := a.id.d.inTx(ctx, func(tx *store.Tx) error { return a.id.Devices.Rename(ctx, tx, me, did, request.Body.Name) }); err != nil {
		return nil, err
	}
	v, err := a.id.Devices.view(ctx, me, did, dev)
	if err != nil {
		return nil, err
	}
	return gen.UpdateMyDevice200JSONResponse(v), nil
}
