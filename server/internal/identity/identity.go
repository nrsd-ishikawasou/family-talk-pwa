// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package identity は家族、構成員、保護者、招待、パスキー、端末、セッションを持つ（srv-identity。
// 基本設計書 第3章、第2章 4.1、4.4、5.1〜5.4、第12章 7〜9、11）。
// 予期した失敗はエラーコード表の *fault.Event、予期しない失敗は *fault.Fault を error で返す（全体実装設計書 2.7）。
// 書き込みは呼ぶ側が渡す *store.Tx の中で行い、hook（Hooks）も同じトランザクションで走る（I-8）。
package identity

import (
	"errors"
	"path/filepath"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/envelope"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// UserState は構成員の状態（全体実装設計書 3.1 の UserState、users.state）。
type UserState string

// 構成員の状態の 5 つ。
const (
	UserNormal       UserState = "normal"
	UserSuspended    UserState = "suspended"
	UserErasePending UserState = "erase_pending"
	UserDeleted      UserState = "deleted"
	UserErased       UserState = "erased"
)

// DeviceState は端末の状態（全体実装設計書 3.1 の DeviceState、devices.state。承認の正）。
type DeviceState string

// 端末の状態の 5 つ（未登録は行が無い）。
const (
	DeviceUnapproved DeviceState = "unapproved"
	DeviceApproved   DeviceState = "approved"
	DeviceDormant    DeviceState = "dormant"
	DeviceRevoked    DeviceState = "revoked"
	DeviceExpired    DeviceState = "expired"
)

// Platform は端末の種類（devices.kind）。
type Platform string

// 端末の種類の 3 つ。
const (
	PlatformIphone  Platform = "iphone"
	PlatformAndroid Platform = "android"
	PlatformPc      Platform = "pc"
)

// RevokeReason はセッションの失効の理由（エラーコード表の AUTH-002 の params.reason、sessions.revoked_reason）。
type RevokeReason string

// 失効の理由の 7 つ。
const (
	RevokeLogout        RevokeReason = "logout"
	RevokeDeviceRevoked RevokeReason = "device_revoked"
	RevokeSuspended     RevokeReason = "suspended"
	RevokeDeleted       RevokeReason = "deleted"
	RevokeErased        RevokeReason = "erased"
	RevokeKeyRotation   RevokeReason = "key_rotation"
	RevokeExpired       RevokeReason = "expired"
)

// Deps は本ブロックが使う下のブロックと、上のブロックが実装して渡す interface。
type Deps struct {
	Pool *store.Pool
	Feed *store.Feed
	Jobs *store.Jobs
	// Retention は期限つきの削除（招待の記録、IP の記録）。
	Retention *store.Retention
	Ids       *ids.Ids
	Envelope  *envelope.Envelope
	Secrets   *secret.Store
	Clock     clock.Clock
	Config    *config.Config
	// Keys は srv-keys が実装する（端末の署名の確かめ、/users の鍵）。
	Keys DeviceKeyReader
	// Push は srv-push が実装する（端末の一覧の購読の有無）。
	Push PushReader
	// Audit は srv-admin が実装する（管理者の操作の記録）。
	Audit Audit
	// State は srv-admin が実装する家族のサーバーの状態（/setup/status の read_only。srv-boundary と同じ interface）。
	State boundary.ServerStateReader
}

// Identity は本ブロックの公開操作（B-060〜B-072）を束ねる。
type Identity struct {
	d         *Deps
	Families  *Families
	Users     *Users
	Guardians *Guardians
	Invites   *Invites
	Setup     *SetupLink
	Passkeys  *Passkeys
	Sessions  *Sessions
	Devices   *Devices
	Qr        *QrFlow
	Reports   *Reports
	Reset     *ResetLink
	IpLog     *IpLog
	Hooks     *Hooks
}

// New は本ブロックを組む。Keys、Push、Audit は上のブロックが組まれた後に SetKeys、SetPush、SetAudit で渡し、
// hook の登録も上のブロックの組み立ての中で行う。最後に Ready を呼ぶ。必須の欄が無いのはプログラムの誤り。
func New(d Deps) *Identity {
	if d.Pool == nil || d.Feed == nil || d.Jobs == nil || d.Retention == nil || d.Ids == nil ||
		d.Envelope == nil || d.Secrets == nil || d.Clock == nil || d.Config == nil {
		panic("identity: Deps の必須の欄が無い")
	}
	dp := &d
	keys := userKeys{d: dp}
	hooks := &Hooks{}
	id := &Identity{d: dp, Hooks: hooks}
	id.IpLog = &IpLog{d: dp, keys: keys}
	id.Families = &Families{d: dp, icon: &Icon{dir: filepath.Join(d.Config.DataDir, "www", "family")}, hooks: hooks}
	id.Sessions = &Sessions{d: dp, expiry: newExpiryCache()}
	id.Devices = &Devices{d: dp, hooks: hooks, sessions: id.Sessions, ipLog: id.IpLog, keys: keys}
	id.Sessions.devices = id.Devices
	id.Guardians = &Guardians{d: dp, hooks: hooks, devices: id.Devices, keys: keys}
	id.Users = &Users{d: dp, hooks: hooks, families: id.Families, sessions: id.Sessions, devices: id.Devices,
		guardians: id.Guardians, keys: keys}
	id.Guardians.users = id.Users
	id.Passkeys = &Passkeys{d: dp, hooks: hooks, users: id.Users, devices: id.Devices, sessions: id.Sessions}
	id.Invites = &Invites{d: dp, users: id.Users, passkeys: id.Passkeys, devices: id.Devices, sessions: id.Sessions,
		guardians: id.Guardians, families: id.Families, hooks: hooks, keys: keys}
	id.Setup = &SetupLink{d: dp, users: id.Users, families: id.Families, passkeys: id.Passkeys, devices: id.Devices,
		sessions: id.Sessions, hooks: hooks}
	id.Qr = &QrFlow{d: dp, users: id.Users, devices: id.Devices, sessions: id.Sessions, guardians: id.Guardians}
	id.Guardians.qr = id.Qr
	id.Reports = &Reports{d: dp, hooks: hooks}
	id.Reset = &ResetLink{d: dp, passkeys: id.Passkeys, devices: id.Devices, sessions: id.Sessions, hooks: hooks}
	return id
}

// SetKeys は srv-keys の DeviceKeyReader を渡す（srv-keys は本ブロックの上なので組み立ての後）。
func (id *Identity) SetKeys(k DeviceKeyReader) {
	id.d.Keys = k
}

// SetPush は srv-push の PushReader を渡す。
func (id *Identity) SetPush(p PushReader) {
	id.d.Push = p
}

// SetAudit は srv-admin の Audit を渡す。
func (id *Identity) SetAudit(a Audit) {
	id.d.Audit = a
}

// SetState は srv-admin のサーバーの状態の読みを渡す。
func (id *Identity) SetState(s boundary.ServerStateReader) {
	id.d.State = s
}

// Ready は起動の最後に呼ぶ。上のブロックの interface が揃っていない、または OnNotice・OnBeforeUserChange・
// OnBeforeDeviceRevoke の登録が無ければ Fault（知らせを黙って捨てない。トークの錠の順が守れない組み立てで動かさない。I-18）。
// 以後の hook の登録はプログラムの誤り。
func (id *Identity) Ready() error {
	if id.d.Keys == nil || id.d.Push == nil || id.d.Audit == nil || id.d.State == nil {
		return fault.Wrap(errors.New("identity: Keys、Push、Audit、State のどれかが渡されていない"))
	}
	if len(id.Hooks.notice) == 0 {
		return fault.Wrap(errNoNoticeHook)
	}
	if len(id.Hooks.beforeUserChange) == 0 || len(id.Hooks.beforeDeviceRevoke) == 0 {
		return fault.Wrap(errNoBeforeHook)
	}
	id.Hooks.sealed = true
	return nil
}

// Apis は生成のタグの interface（SetupApi、InvitesApi、AuthApi、MeApi、FamilyApi、UsersApi、DevicesApi）の実装を返す。
func (id *Identity) Apis() *Apis {
	return &Apis{Setup: &SetupApi{id: id}, Invites: &InvitesApi{id: id}, Auth: &AuthApi{id: id}, Me: &MeApi{id: id},
		Family: &FamilyApi{id: id}, Users: &UsersApi{id: id}, Devices: &DevicesApi{id: id}}
}

// Verifiers は srv-boundary の逆転の interface（SessionVerifier、WsTokenVerifier、ShellTokenVerifier）の実装を返す。
func (id *Identity) Verifiers() *Verifiers {
	return &Verifiers{Session: sessionVerifier{s: id.Sessions}, Ws: wsVerifier{s: id.Sessions}, Shell: shellVerifier{s: id.Sessions}}
}
