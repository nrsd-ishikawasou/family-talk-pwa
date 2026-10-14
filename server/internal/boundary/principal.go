// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package boundary はサーバーの境界（HTTP、WebSocket、middleware、認可の表）を持つ（srv-boundary。全体実装設計書 2.1）。
// 業務の判断は持たない。業務のブロックは生成したタグごとの interface を実装し、本パッケージが束ねる。
package boundary

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"
)

// PrincipalKind は要求の主体の種類。
type PrincipalKind string

// 主体の種類（ページの Principal.Kind）。
const (
	KindUser     PrincipalKind = "user"
	KindShell    PrincipalKind = "shell"
	KindSetup    PrincipalKind = "setup"
	KindInvite   PrincipalKind = "invite"
	KindInternal PrincipalKind = "internal"
	KindNone     PrincipalKind = "none"
)

// Role は家族の中の役割。
type Role string

// 役割（第2章 4.3）。
const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
)

// RoomRole はトークの中の役割。
type RoomRole string

// トークの中の役割（第2章 4.3）。
const (
	RoomRoleNone    RoomRole = ""
	RoomRoleMember  RoomRole = "member"
	RoomRoleCreator RoomRole = "creator"
)

// Principal は要求の主体（B-041）。業務の operation は PrincipalFrom(ctx) で取る。
type Principal struct {
	Kind           PrincipalKind
	UserId         *uuid.UUID
	DeviceId       *uuid.UUID
	SessionId      *uuid.UUID
	Role           Role
	DeviceApproved bool
	// ReauthAt は再認証の時刻。5 分の判定は業務が行う。
	ReauthAt *time.Time
}

type ctxKey int

const (
	keyPrincipal ctxKey = iota
	keyOperation
	keyRequestId
)

// WithPrincipal は主体を ctx に入れる（Session の middleware）。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	panic("未実装")
}

// PrincipalFrom は ctx の主体を返す。無ければ Kind が none の主体。
func PrincipalFrom(ctx context.Context) Principal {
	panic("未実装")
}

// WithOperation は経路の operationId を ctx に入れる（AuthzTable と RateLimit が引く）。
func WithOperation(ctx context.Context, operationId string) context.Context {
	panic("未実装")
}

// OperationFrom は ctx の operationId を返す。
func OperationFrom(ctx context.Context) string {
	panic("未実装")
}

// RequestIdFrom は ctx の要求の識別子（UUIDv7。ログの req_id、problem の instance）を返す。
func RequestIdFrom(ctx context.Context) uuid.UUID {
	panic("未実装")
}
