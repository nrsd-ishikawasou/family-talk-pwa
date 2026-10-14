// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
)

// Reauth は x-reauth の値（再認証が要るか）。
type Reauth string

// 再認証の要否。
const (
	ReauthNone        Reauth = ""
	ReauthAlways      Reauth = "true"
	ReauthConditional Reauth = "conditional"
)

// IdempotencyKind は x-idempotency の値（第2章 11）。
type IdempotencyKind string

// 冪等の種類。
const (
	IdempotencyNone    IdempotencyKind = "none"
	IdempotencyKey     IdempotencyKind = "key"
	IdempotencyNatural IdempotencyKind = "natural"
)

// Route は 1 つの operation の属性（openapi の x- の欄。全体実装設計書 5.2）。
type Route struct {
	OperationId string
	Method      string
	Path        string
	// Authz は x-authz の値のどれか 1 つで通す（第2章 10 の表）。
	Authz []string
	// Rule は x-authz-rule。ハンドラが資源を読んでから確かめる。
	Rule        string
	Reauth      Reauth
	Idempotency IdempotencyKind
	// Natural は x-idempotency-natural（自然な鍵の欄）。
	Natural   string
	RateLimit []string
	// WebSocket は x-websocket（ws か upWs。strict の生成から除いた経路）。
	WebSocket string
	Internal  bool
	// ReadOnlyAllow は x-readonly: allow（読み取り専用でも通す書き込み。第2章 10）。
	ReadOnlyAllow bool
	// Errors は x-errors（この経路が返しうるエラーコード）。
	Errors []string
	// DeprecatedAt と Sunset は x-deprecation（廃止する経路。RFC 9745、8594。第2章 10）。無ければ nil。
	DeprecatedAt *time.Time
	Sunset       *time.Time
}

// AuthzArgs は判定に使う経路の引数。
type AuthzArgs struct {
	RoomId   *uuid.UUID
	DeviceId *uuid.UUID
}

// AuthzTable は経路の属性の表（B-048）。x-idempotency と x-ratelimit も同じ表で引く。
type AuthzTable struct {
	routes     map[string]Route
	membership RoomMembership
}

// FromSpec は束ねた spec（gen.Spec）の全 operation の属性を読む。x-authz の欠けや表に無い値は Fault（I-12）。
func FromSpec(spec []byte, membership RoomMembership) (*AuthzTable, error) {
	panic("未実装")
}

// Route は operationId の属性を返す。
func (t *AuthzTable) Route(operationId string) (Route, bool) {
	panic("未実装")
}

// Routes は全 operation の属性（operationId の順）。
func (t *AuthzTable) Routes() []Route {
	panic("未実装")
}

// Check は x-authz の配列のどれか 1 つを満たすかを判定する。
// 未承認の端末は session か device_self を含む経路だけを通す（他は KEY-004）。
func (t *AuthzTable) Check(ctx context.Context, operationId string, p Principal, args AuthzArgs) *fault.Event {
	panic("未実装")
}
