// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// ChallengeKind は WebAuthn の challenge の種類（login_challenges.kind）。
type ChallengeKind string

// challenge の種類の 5 つ。期限は 5 分。
const (
	ChallengeRegister ChallengeKind = "register"
	ChallengeLogin    ChallengeKind = "login"
	ChallengeSetup    ChallengeKind = "setup"
	ChallengeInvite   ChallengeKind = "invite"
	ChallengeReset    ChallengeKind = "reset"
)

// Challenge は login_challenges の 1 行。検証で消す（1 回）。
type Challenge struct {
	Id          uuid.UUID
	Bytes       []byte
	Kind        ChallengeKind
	UserId      *uuid.UUID
	TokenSha256 []byte
	// Session は WebAuthn の session の記録（user handle を含む）。
	Session   json.RawMessage
	ExpiresAt time.Time
}

// RegToken は登録の options の要求のトークン（invite、setup、reset のどれか 1 つ。2 つ以上は NET-001）。
type RegToken struct {
	Kind  ChallengeKind
	Token string
}

// DeviceProof は既知の端末の証（device_hint と device_sig。第3章 4.2、第12章 5.3）。
type DeviceProof struct {
	DeviceId uuid.UUID
	Sig      []byte
}

// Credential はパスキーの 1 行（passkeys）。Record は資格情報の記録の全体（PRF の結果は含まない）。
type Credential struct {
	Id         []byte
	UserId     uuid.UUID
	Name       *string
	Record     []byte
	Prf        bool
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// Passkeys はパスキーの登録とログインを持つ（B-065。第3章 4.1、4.2）。
type Passkeys struct {
	d        *Deps
	hooks    *Hooks
	users    *Users
	devices  *Devices
	sessions *Sessions
}

// webauthnUser は go-webauthn の User（handle は users.webauthn_handle、名前は表示名）。
type webauthnUser struct {
	handle []byte
	name   string
	creds  []webauthn.Credential
}

func (u webauthnUser) WebAuthnID() []byte                         { return u.handle }
func (u webauthnUser) WebAuthnName() string                       { return u.name }
func (u webauthnUser) WebAuthnDisplayName() string                { return u.name }
func (u webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// newMemberName は設置と招待の登録の options の時の仮の名前（要求に表示名がまだ無い。第3章 4.1）。
const newMemberName = "新しい構成員"

// origin は家族のサーバーのオリジン（https://名前、443 でなければ :ポート。srv-boundary と同じ規則）。
func (d *Deps) origin() string {
	host := d.Config.Domain
	if d.Config.HttpsPort != 0 && d.Config.HttpsPort != 443 {
		host += ":" + strconv.Itoa(d.Config.HttpsPort)
	}
	return "https://" + host
}

// webAuthn は今の家族の名前で設定を作る（RPDisplayName は家族の名前。無ければ「家族のサーバー」）。
// attestation none、residentKey required、userVerification required、期限 300 秒（Enforce は偽：時刻の比較は DB の期限と Clock）、
// 求めていない拡張の出力は拒む（既定の Reject）。
func (k *Passkeys) webAuthn(ctx context.Context) (*webauthn.WebAuthn, error) {
	name := "家族のサーバー"
	if err := k.d.Pool.QueryRow(ctx, `SELECT name FROM families`).Scan(&name); err != nil && !noRows(err) {
		return nil, wrap(err)
	}
	t := webauthn.TimeoutConfig{Enforce: false, Timeout: challengeTtl, TimeoutUVD: challengeTtl}
	w, err := webauthn.New(&webauthn.Config{
		RPID:                  k.d.Config.Domain,
		RPDisplayName:         name,
		RPOrigins:             []string{k.d.origin()},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(), UserVerification: protocol.VerificationRequired},
		Timeouts:                          webauthn.TimeoutsConfig{Login: t, Registration: t},
		ExtensionsUnsolicitedOutputPolicy: protocol.UnsolicitedOutputPolicyReject,
	})
	return w, wrap(err)
}

// credsOf は構成員の資格情報の記録を読む。
func credsOf(ctx context.Context, q querier, userId uuid.UUID) ([]webauthn.Credential, error) {
	rows, err := q.Query(ctx, `SELECT record FROM passkeys WHERE user_id = $1 ORDER BY created_at, cred_id`, userId)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, wrap(err)
		}
		var c webauthn.Credential
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, wrap(err)
		}
		out = append(out, c)
	}
	return out, wrap(rows.Err())
}

// optionsMap は options を応答の WebAuthnOptions（JSON の object）にする。
func optionsMap(v any) (gen.WebAuthnOptions, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, wrap(err)
	}
	m := gen.WebAuthnOptions{}
	return m, wrap(json.Unmarshal(b, &m))
}

// putChallenge は login_challenges に置き、id を返す（5 分）。
func (k *Passkeys) putChallenge(ctx context.Context, tx *store.Tx, kind ChallengeKind, userId *uuid.UUID, tokenHash, challenge []byte, session *webauthn.SessionData) (uuid.UUID, error) {
	raw, err := json.Marshal(session)
	if err != nil {
		return uuid.Nil, wrap(err)
	}
	id := k.d.Ids.NewV7()
	_, err = tx.Exec(ctx, `INSERT INTO login_challenges (id, challenge, kind, user_id, token_sha256, session, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, challenge, string(kind), userId, tokenHash, raw, k.d.now().Add(challengeTtl))
	return id, wrap(err)
}

// takeChallenge は challenge を消して取り出す（1 回）。無い・種類が違う・期限切れは AUTH-003。
func (k *Passkeys) takeChallenge(ctx context.Context, tx *store.Tx, id uuid.UUID, kind ChallengeKind) (Challenge, webauthn.SessionData, error) {
	c := Challenge{Id: id, Kind: kind}
	var raw []byte
	err := tx.QueryRow(ctx, `DELETE FROM login_challenges WHERE id = $1 AND kind = $2 RETURNING challenge, user_id, token_sha256, session, expires_at`,
		id, string(kind)).Scan(&c.Bytes, &c.UserId, &c.TokenSha256, &raw, &c.ExpiresAt)
	if noRows(err) {
		return Challenge{}, webauthn.SessionData{}, ev(cgen.AUTH003)
	}
	if err != nil {
		return Challenge{}, webauthn.SessionData{}, wrap(err)
	}
	if !c.ExpiresAt.After(k.d.now()) {
		return Challenge{}, webauthn.SessionData{}, ev(cgen.AUTH003)
	}
	c.Session = raw
	var s webauthn.SessionData
	if err := json.Unmarshal(raw, &s); err != nil {
		return Challenge{}, webauthn.SessionData{}, wrap(err)
	}
	return c, s, nil
}

// regTarget は登録の対象（構成員がまだ無ければ userId は nil）。
type regTarget struct {
	kind      ChallengeKind
	userId    *uuid.UUID
	handle    []byte
	name      string
	tokenHash []byte
	exclude   []webauthn.Credential
}

// RegisterOptions は第3章 4.1 の値で登録の options を作る（excludeCredentials）。p はログインの後の追加
// （既にパスキーがあれば再認証。AUTH-006）、token は設置・招待・作り直し（使えなければ AUTH-007。使い切りは消費の時）。
// challenge を login_challenges に置き id を返す。
func (k *Passkeys) RegisterOptions(ctx context.Context, p *boundary.Principal, token *RegToken) (gen.WebAuthnChallenge, error) {
	t, err := k.regTarget(ctx, p, token)
	if err != nil {
		return gen.WebAuthnChallenge{}, err
	}
	w, err := k.webAuthn(ctx)
	if err != nil {
		return gen.WebAuthnChallenge{}, err
	}
	excl := make([]protocol.CredentialDescriptor, 0, len(t.exclude))
	for _, c := range t.exclude {
		excl = append(excl, c.Descriptor())
	}
	challenge := k.d.Ids.NewToken()
	creation, session, err := w.BeginRegistration(webauthnUser{handle: t.handle, name: t.name},
		webauthn.WithExclusions(excl),
		webauthn.WithCredentialParameters([]protocol.CredentialParameter{
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256},
			{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgRS256}}),
		webauthn.WithPublicKeyCredentialHints([]protocol.PublicKeyCredentialHints{protocol.PublicKeyCredentialHintClientDevice}),
		webauthn.WithExtensions(webauthn.WithExtensionCredProps(), webauthn.WithExtensionPRFSupport()),
		func(o *protocol.PublicKeyCredentialCreationOptions) error { o.Challenge = challenge; return nil })
	if err != nil {
		return gen.WebAuthnChallenge{}, wrap(err)
	}
	// hints から埋められた authenticatorAttachment を空に戻す（第3章 4.1 は attachment を書かず、hybrid での登録を締め出さない）。
	creation.Response.AuthenticatorSelection.AuthenticatorAttachment = ""
	var id uuid.UUID
	if err := k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = k.putChallenge(ctx, tx, t.kind, t.userId, t.tokenHash, challenge, session)
		return err
	}); err != nil {
		return gen.WebAuthnChallenge{}, err
	}
	opts, err := optionsMap(creation.Response)
	if err != nil {
		return gen.WebAuthnChallenge{}, err
	}
	return gen.WebAuthnChallenge{ChallengeId: id.String(), Options: opts}, nil
}

// regTarget は登録の対象を決める。
func (k *Passkeys) regTarget(ctx context.Context, p *boundary.Principal, token *RegToken) (regTarget, error) {
	if token == nil {
		if p == nil {
			return regTarget{}, ev(cgen.AUTH001)
		}
		me, _, err := self(*p)
		if err != nil {
			return regTarget{}, err
		}
		// 認可 approved（srv-boundary はトークンの値がある経路を通すので、トークンの無い時は本ブロックが確かめる）。
		if !p.DeviceApproved {
			return regTarget{}, ev(cgen.KEY004)
		}
		usr, handle, err := k.userHandle(ctx, k.d.Pool, me)
		if err != nil {
			return regTarget{}, err
		}
		creds, err := credsOf(ctx, k.d.Pool, me)
		if err != nil {
			return regTarget{}, err
		}
		if len(creds) > 0 {
			if err := k.d.requireReauth(*p); err != nil {
				return regTarget{}, err
			}
		}
		return regTarget{kind: ChallengeRegister, userId: &me, handle: handle, name: usr.DisplayName, exclude: creds}, nil
	}
	hash, ok := tokenHash(token.Token)
	if !ok {
		return regTarget{}, ev(cgen.AUTH007)
	}
	now := k.d.now()
	switch token.Kind {
	case ChallengeSetup:
		if err := setupUsable(ctx, k.d.Pool, hash, now, false); err != nil {
			return regTarget{}, err
		}
		return regTarget{kind: ChallengeSetup, handle: k.d.Ids.NewToken(), name: newMemberName, tokenHash: hash}, nil
	case ChallengeInvite:
		if _, err := inviteUsable(ctx, k.d.Pool, hash, now, false); err != nil {
			return regTarget{}, err
		}
		return regTarget{kind: ChallengeInvite, handle: k.d.Ids.NewToken(), name: newMemberName, tokenHash: hash}, nil
	case ChallengeReset:
		userId, err := resetUsable(ctx, k.d.Pool, hash, now, false)
		if err != nil {
			return regTarget{}, err
		}
		usr, handle, err := k.userHandle(ctx, k.d.Pool, userId)
		if err != nil {
			return regTarget{}, err
		}
		creds, err := credsOf(ctx, k.d.Pool, userId)
		if err != nil {
			return regTarget{}, err
		}
		return regTarget{kind: ChallengeReset, userId: &userId, handle: handle, name: usr.DisplayName, tokenHash: hash, exclude: creds}, nil
	}
	return regTarget{}, fault.Wrap(errors.New("identity: 登録のトークンの種類が違う"))
}

// userHandle は構成員と WebAuthn の user handle を読む。
func (k *Passkeys) userHandle(ctx context.Context, q querier, userId uuid.UUID) (User, []byte, error) {
	usr, err := k.users.get(ctx, q, userId, false)
	if err != nil {
		return User{}, nil, err
	}
	var handle []byte
	err = q.QueryRow(ctx, `SELECT webauthn_handle FROM users WHERE id = $1`, userId).Scan(&handle)
	return usr, handle, wrap(err)
}

// hasPrfResults は拡張の出力に PRF の出力があるか（サーバーは受け取らない。第12章 5.4 (e)）。
func hasPrfResults(o protocol.AuthenticationExtensionsClientOutputs) bool {
	return o.PRF != nil && o.PRF.Results != nil
}

// finishRegister は登録の応答を確かめて passkeys に置く。PRF の出力があれば AUTH-003（値は記録しない）。
// 発見可能でない（credProps.rk が真でない）資格情報は AUTH-003（residentKey required。条件付き UI で使えないため）。
func (k *Passkeys) finishRegister(ctx context.Context, tx *store.Tx, user webauthnUser, session webauthn.SessionData, response gen.WebAuthnResponse, userId uuid.UUID, name *string) (Credential, error) {
	raw, err := json.Marshal(response)
	if err != nil {
		return Credential{}, ev(cgen.AUTH003)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil || hasPrfResults(parsed.ClientExtensionResults) {
		return Credential{}, ev(cgen.AUTH003)
	}
	w, err := k.webAuthn(ctx)
	if err != nil {
		return Credential{}, err
	}
	cred, err := w.CreateCredential(user, session, parsed)
	if err != nil || cred.Extensions.RK == nil || !*cred.Extensions.RK {
		return Credential{}, ev(cgen.AUTH003)
	}
	record, err := json.Marshal(cred)
	if err != nil {
		return Credential{}, wrap(err)
	}
	c := Credential{Id: cred.ID, UserId: userId, Name: name, Record: record,
		Prf: cred.Extensions.PRFEnabled != nil && *cred.Extensions.PRFEnabled, CreatedAt: k.d.now()}
	_, err = tx.Exec(ctx, `INSERT INTO passkeys (cred_id, user_id, name, record, prf, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		c.Id, userId, name, record, c.Prf, c.CreatedAt)
	if uniqueViolation(err) {
		return Credential{}, ev(cgen.AUTH003)
	}
	return c, wrap(err)
}

// RegisterVerify はログインの後のパスキーの追加を確かめて記録する（credProps.rk、prf.enabled）。challenge は 1 回で消す。
// 応答の拡張の出力に prf.results があれば AUTH-003（PRF の出力をサーバーが受け取らない。第12章 5.4 (e)）。
func (k *Passkeys) RegisterVerify(ctx context.Context, tx *store.Tx, userId, challengeId uuid.UUID, response gen.WebAuthnResponse, name *string) (Credential, error) {
	c, session, err := k.takeChallenge(ctx, tx, challengeId, ChallengeRegister)
	if err != nil {
		return Credential{}, err
	}
	if c.UserId == nil || *c.UserId != userId {
		return Credential{}, ev(cgen.AUTH003)
	}
	usr, handle, err := k.userHandle(ctx, tx, userId)
	if err != nil {
		return Credential{}, err
	}
	return k.finishRegister(ctx, tx, webauthnUser{handle: handle, name: usr.DisplayName}, session, response, userId, name)
}

// LoginOptions はログインの options（allowCredentials は空。条件付き UI）。拡張は求めない（PRF の評価は端末が自分で足す。
// 第3章 4.2、第12章 5.4 (e)）。既定の Reject を保つ。
func (k *Passkeys) LoginOptions(ctx context.Context) (gen.WebAuthnChallenge, error) {
	w, err := k.webAuthn(ctx)
	if err != nil {
		return gen.WebAuthnChallenge{}, err
	}
	challenge := k.d.Ids.NewToken()
	assertion, session, err := w.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired), webauthn.WithChallenge(challenge))
	if err != nil {
		return gen.WebAuthnChallenge{}, wrap(err)
	}
	var id uuid.UUID
	if err := k.d.inTx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = k.putChallenge(ctx, tx, ChallengeLogin, nil, nil, challenge, session)
		return err
	}); err != nil {
		return gen.WebAuthnChallenge{}, err
	}
	opts, err := optionsMap(assertion.Response)
	if err != nil {
		return gen.WebAuthnChallenge{}, err
	}
	return gen.WebAuthnChallenge{ChallengeId: id.String(), Options: opts}, nil
}

// deviceProofMessage は device_sig の署名の対象（ft.device_proof/1、0、challenge の 32 バイト。第12章 5.3）。
func deviceProofMessage(challenge []byte) []byte {
	m := append([]byte("ft.device_proof/1"), 0)
	return append(m, challenge...)
}

// LoginVerify はログインを確かめてセッションを出す。AUTH-003（構成員の有無を漏らさない）。停止・削除・抹消の猶予と抹消済みは MEM-002。
// AUTH-004 は srv-boundary のレート制限（ratelimits.yaml の login。DD-3-9）。応答の拡張の出力に prf があれば空でも AUTH-003
// （ログインの options は prf を求めない。端末は送る前に prf を丸ごと除く。第3章 4.2、第12章 5.4 (e)）。
// proof があり、端末が同じ構成員の物で取り消し・期限切れでなく、DeviceKeyReader.VerifyProof が通れば同じ端末
// （そのセッションを作り直す）。それ以外は device の名前と種類で新しい端末（未承認。第3章 4.2）。
func (k *Passkeys) LoginVerify(ctx context.Context, tx *store.Tx, challengeId uuid.UUID, response gen.WebAuthnResponse, proof *DeviceProof, device NewDevice) (Issued, error) {
	c, session, err := k.takeChallenge(ctx, tx, challengeId, ChallengeLogin)
	if err != nil {
		return Issued{}, err
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return Issued{}, ev(cgen.AUTH003)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil || parsed.ClientExtensionResults.PRF != nil {
		return Issued{}, ev(cgen.AUTH003)
	}
	w, err := k.webAuthn(ctx)
	if err != nil {
		return Issued{}, err
	}
	var userId uuid.UUID
	var lookupErr error
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		var name string
		if err := tx.QueryRow(ctx, `SELECT id, display_name FROM users WHERE webauthn_handle = $1`, userHandle).Scan(&userId, &name); err != nil {
			if !noRows(err) {
				lookupErr = wrap(err)
			}
			return nil, errors.New("identity: user handle に当たる構成員が無い")
		}
		creds, err := credsOf(ctx, tx, userId)
		if err != nil {
			lookupErr = err
			return nil, err
		}
		return webauthnUser{handle: userHandle, name: name, creds: creds}, nil
	}
	_, cred, err := w.ValidatePasskeyLogin(handler, session, parsed)
	if lookupErr != nil {
		return Issued{}, lookupErr
	}
	if err != nil {
		return Issued{}, ev(cgen.AUTH003)
	}
	usr, err := k.users.get(ctx, tx, userId, false)
	if err != nil {
		return Issued{}, err
	}
	if usr.State != UserNormal {
		return Issued{}, ev(cgen.MEM002)
	}
	record, err := json.Marshal(cred)
	if err != nil {
		return Issued{}, wrap(err)
	}
	now := k.d.now()
	if _, err := tx.Exec(ctx, `UPDATE passkeys SET record = $2, last_used_at = $3 WHERE cred_id = $1`, cred.ID, record, now); err != nil {
		return Issued{}, wrap(err)
	}
	deviceId, known, err := k.knownDevice(ctx, tx, userId, proof, c.Bytes)
	if err != nil {
		return Issued{}, err
	}
	if !known {
		dv, err := k.newDevice(ctx, tx, usr, device)
		if err != nil {
			return Issued{}, err
		}
		deviceId = dv.Id
	}
	// 錠の順は端末、セッション（端末の取り消しと同じ順）。最終の利用を先に書く。
	if err := k.devices.Touch(ctx, tx, deviceId, boundary.ClientIpFrom(ctx), now); err != nil {
		return Issued{}, err
	}
	var cookie string
	var sess gen.Session
	var sid uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM sessions WHERE device_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1`, deviceId).Scan(&sid)
	switch {
	case known && err == nil:
		cookie, sess, err = k.sessions.rotate(ctx, tx, sid)
	case err == nil || noRows(err):
		cookie, sess, err = k.sessions.Issue(ctx, tx, userId, deviceId, true)
	default:
		err = wrap(err)
	}
	if err != nil {
		return Issued{}, err
	}
	return Issued{Cookie: cookie, Result: gen.LoginResult{NewDevice: !known, Session: sess}}, nil
}

// knownDevice は device_sig が通る既知の端末か（端末の識別子だけでは同じ端末としない。H-3-3）。
func (k *Passkeys) knownDevice(ctx context.Context, tx *store.Tx, userId uuid.UUID, proof *DeviceProof, challenge []byte) (uuid.UUID, bool, error) {
	if proof == nil {
		return uuid.Nil, false, nil
	}
	dv, err := k.devices.get(ctx, tx, proof.DeviceId, false)
	if err != nil {
		var e *fault.Event
		if errors.As(err, &e) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	if dv.UserId != userId || !slices.Contains([]DeviceState{DeviceUnapproved, DeviceApproved, DeviceDormant}, dv.State) {
		return uuid.Nil, false, nil
	}
	ok, err := k.d.Keys.VerifyProof(ctx, dv.Id, deviceProofMessage(challenge), proof.Sig)
	if err != nil {
		return uuid.Nil, false, wrap(err)
	}
	return dv.Id, ok, nil
}

// newDevice は新しい端末（未承認）を登録し（new_device は Devices.Register が出す）、家族の全員に device.changed（added）。
func (k *Passkeys) newDevice(ctx context.Context, tx *store.Tx, usr User, device NewDevice) (Device, error) {
	dv, err := k.devices.Register(ctx, tx, usr.Id, device.Platform, device.Name)
	if err != nil {
		return Device{}, err
	}
	by := usr.Id
	if err := k.d.emitDeviceChanged(ctx, tx, usr.Id, dv.Id, changeAdded, &by); err != nil {
		return Device{}, err
	}
	return dv, nil
}

// List は GET /me/passkeys（第2章 5.2）。同期の有無は記録の flags の backupState。並びは作った順、cursor は資格情報の識別子
// （base64url）。cursor が本人のパスキーに無ければ NET-001。
func (k *Passkeys) List(ctx context.Context, userId uuid.UUID, limit *int, before, after *string) ([]gen.Passkey, bool, error) {
	rows, err := k.d.Pool.Query(ctx, `SELECT cred_id, name, record, prf, created_at, last_used_at FROM passkeys WHERE user_id = $1 ORDER BY created_at, cred_id`, userId)
	if err != nil {
		return nil, false, wrap(err)
	}
	var all []gen.Passkey
	for rows.Next() {
		var id, record []byte
		var name *string
		var prf bool
		var created time.Time
		var used *time.Time
		if err := rows.Scan(&id, &name, &record, &prf, &created, &used); err != nil {
			rows.Close()
			return nil, false, wrap(err)
		}
		var c webauthn.Credential
		if err := json.Unmarshal(record, &c); err != nil {
			rows.Close()
			return nil, false, wrap(err)
		}
		all = append(all, gen.Passkey{Id: base64.RawURLEncoding.EncodeToString(id), Name: name, CreatedAt: created.UTC(),
			LastUsedAt: utcPtr(used), BackedUp: c.Flags.BackupState, Prf: prf})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	return pageInMemory(all, func(p gen.Passkey) string { return p.Id }, limit, before, after)
}

// pageInMemory は古い順の全件に第2章 3 の cursor を当てる（数の少ない一覧。cursor が無い要素なら NET-001）。
func pageInMemory[T any](all []T, key func(T) string, limit *int, before, after *string) ([]T, bool, error) {
	c := newCursor(limit, before, after)
	lo, hi := 0, len(all)
	find := func(s *string, field string) (int, error) {
		for i, x := range all {
			if key(x) == *s {
				return i, nil
			}
		}
		return 0, fault.New(cgen.NET001, map[string]any{"errors": []map[string]any{{"path": "query." + field, "message": "一覧に無い"}}})
	}
	if after != nil {
		i, err := find(after, "after")
		if err != nil {
			return nil, false, err
		}
		lo = i + 1
	}
	if before != nil {
		i, err := find(before, "before")
		if err != nil {
			return nil, false, err
		}
		hi = i
	}
	if lo > hi {
		lo = hi
	}
	span := all[lo:hi]
	if c.ascending() {
		if len(span) > c.limit {
			return slices.Clone(span[:c.limit]), true, nil
		}
		return slices.Clone(span), false, nil
	}
	if len(span) > c.limit {
		return slices.Clone(span[len(span)-c.limit:]), true, nil
	}
	return slices.Clone(span), false, nil
}

// Delete は DELETE /me/passkeys/{id}。無い・本人の物でなければ何もしない（DELETE の冪等。RFC 9110。他人のパスキーの有無を漏らさない）。
// 最後の 1 つは AUTH-008（本人のパスキーを FOR UPDATE で数える）。消した後に同じトランザクションで hook OnPasskeyDeleted（コピー (e) の削除）。
func (k *Passkeys) Delete(ctx context.Context, tx *store.Tx, userId uuid.UUID, credId []byte) error {
	rows, err := tx.Query(ctx, `SELECT cred_id FROM passkeys WHERE user_id = $1 ORDER BY cred_id FOR UPDATE`, userId)
	if err != nil {
		return wrap(err)
	}
	var mine [][]byte
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return wrap(err)
		}
		mine = append(mine, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrap(err)
	}
	if !slices.ContainsFunc(mine, func(id []byte) bool { return bytes.Equal(id, credId) }) {
		return nil
	}
	if len(mine) == 1 {
		return ev(cgen.AUTH008)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM passkeys WHERE cred_id = $1`, credId); err != nil {
		return wrap(err)
	}
	return k.hooks.onPasskeyDeleted(ctx, tx, userId, credId)
}

// PrfEnabled はパスキーが PRF を持つか（srv-keys がコピー (e) の可否に使う）。無ければ偽。
func (k *Passkeys) PrfEnabled(ctx context.Context, credId []byte) (bool, error) {
	var prf bool
	err := k.d.Pool.QueryRow(ctx, `SELECT prf FROM passkeys WHERE cred_id = $1`, credId).Scan(&prf)
	if noRows(err) {
		return false, nil
	}
	return prf, wrap(err)
}

// CredOwner は資格情報の持ち主の構成員と PRF の有無（Passkeys.OwnerOf）。
type CredOwner struct {
	UserId uuid.UUID
	Prf    bool
}

// OwnerOf は資格情報の持ち主と PRF の有無。無ければ KEY-009（srv-keys がコピー (e) の対象を確かめる。
// PUT /me/keybundle/prf/{cred_id}。第12章 5.4 (e)）。
func (k *Passkeys) OwnerOf(ctx context.Context, credId []byte) (CredOwner, error) {
	var o CredOwner
	err := k.d.Pool.QueryRow(ctx, `SELECT user_id, prf FROM passkeys WHERE cred_id = $1`, credId).Scan(&o.UserId, &o.Prf)
	if noRows(err) {
		return CredOwner{}, ev(cgen.KEY009)
	}
	return o, wrap(err)
}

// backedUpOf は記録の flags の backupState（同期の有無。第3章 4.1）。読めなければ偽。
func backedUpOf(record []byte) bool {
	var c webauthn.Credential
	if err := json.Unmarshal(record, &c); err != nil {
		return false
	}
	return c.Flags.BackupState
}
