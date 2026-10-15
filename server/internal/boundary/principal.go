// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package boundary はサーバーの境界（HTTP、WebSocket、middleware、認可の表）を持つ（srv-boundary。全体実装設計書 2.1）。
// 業務の判断は持たない。業務のブロックは生成したタグごとの interface を実装し、本パッケージが束ねる。
package boundary

import (
	"context"
	"net/netip"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
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

// reqInfo は要求ごとの情報（RequestId の middleware が作る）。
type reqInfo struct {
	id     uuid.UUID
	op     string
	origin string
	log    *logx.Logger
	start  time.Time
	// ip は端末の IP（clientIp と同じ規則。形が読めなければ空の値）。
	ip netip.Addr
}

type ctxKey int

const (
	keyPrincipal ctxKey = iota
	keyOperation
	keyReq
)

// WithPrincipal は主体を ctx に入れる（Session の middleware）。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, keyPrincipal, p)
}

// PrincipalFrom は ctx の主体を返す。無ければ Kind が none の主体。
func PrincipalFrom(ctx context.Context) Principal {
	if p, ok := ctx.Value(keyPrincipal).(Principal); ok {
		return p
	}
	return Principal{Kind: KindNone}
}

// WithOperation は経路の operationId を ctx に入れる（AuthzTable と RateLimit が引く）。
func WithOperation(ctx context.Context, operationId string) context.Context {
	return context.WithValue(ctx, keyOperation, operationId)
}

// OperationFrom は ctx の operationId を返す。経路の外（SpaIndex など）は空。
func OperationFrom(ctx context.Context) string {
	op, _ := ctx.Value(keyOperation).(string)
	return op
}

// withReq は要求ごとの情報を ctx に入れる。
func withReq(ctx context.Context, ri *reqInfo) context.Context {
	return context.WithValue(ctx, keyReq, ri)
}

// reqFrom は要求ごとの情報を返す。RequestId を通っていなければ nil。
func reqFrom(ctx context.Context) *reqInfo {
	ri, _ := ctx.Value(keyReq).(*reqInfo)
	return ri
}

// RequestIdFrom は ctx の要求の識別子（UUIDv7。ログの req_id、problem の instance）を返す。無ければ uuid.Nil。
func RequestIdFrom(ctx context.Context) uuid.UUID {
	if ri := reqFrom(ctx); ri != nil {
		return ri.id
	}
	return uuid.Nil
}

// ClientIpFrom は ctx の端末の IP を返す（B-041。RequestId の middleware が置く。レート制限と同じ値）。
// RequestId を通っていない、または形が読めなければ空の値（IsValid が偽）。
func ClientIpFrom(ctx context.Context) netip.Addr {
	if ri := reqFrom(ctx); ri != nil {
		return ri.ip
	}
	return netip.Addr{}
}
