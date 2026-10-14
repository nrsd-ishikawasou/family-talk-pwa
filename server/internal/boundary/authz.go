// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"go.yaml.in/yaml/v3"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
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

// x-authz の値（第2章 10 の表）。
const (
	authzNone        = "none"
	authzSession     = "session"
	authzApproved    = "approved"
	authzDeviceSelf  = "device_self"
	authzRoomMember  = "room_member"
	authzAdmin       = "admin"
	authzShell       = "shell"
	authzSetupToken  = "setup_token"
	authzInviteToken = "invite_token"
	authzResetToken  = "reset_token"
	authzDeviceCode  = "device_code"
	authzWsToken     = "ws_token"
	authzInternal    = "internal"
)

// knownAuthz は Check が扱う値。表に無い値は FromSpec が Fault にする（I-12）。
var knownAuthz = map[string]bool{
	authzNone: true, authzSession: true, authzApproved: true, authzDeviceSelf: true, authzRoomMember: true,
	authzAdmin: true, authzShell: true, authzSetupToken: true, authzInviteToken: true, authzResetToken: true,
	authzDeviceCode: true, authzWsToken: true, authzInternal: true,
}

// passThrough は本ブロックが通す値。トークンは使い切る操作（srv-identity）、ws_token は接続の後の auth、
// internal はネットワークの側が確かめる（全体実装設計書 5.2）。
var passThrough = map[string]bool{
	authzSetupToken: true, authzInviteToken: true, authzResetToken: true, authzDeviceCode: true,
	authzWsToken: true, authzInternal: true,
}

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

// AuthzArgs は判定に使う経路の引数（room_member と device_self の経路は path の {id}）。
type AuthzArgs struct {
	RoomId   *uuid.UUID
	DeviceId *uuid.UUID
}

// AuthzTable は経路の属性の表（B-048）。x-idempotency と x-ratelimit も同じ表で引く。
type AuthzTable struct {
	routes     map[string]Route
	byPattern  map[string]string
	membership RoomMembership
}

// specOperation は spec の 1 つの operation の x- の欄。
type specOperation struct {
	OperationId string   `yaml:"operationId"`
	Authz       []string `yaml:"x-authz"`
	Rule        string   `yaml:"x-authz-rule"`
	Reauth      any      `yaml:"x-reauth"`
	Idempotency string   `yaml:"x-idempotency"`
	Natural     string   `yaml:"x-idempotency-natural"`
	RateLimit   []string `yaml:"x-ratelimit"`
	WebSocket   string   `yaml:"x-websocket"`
	Internal    bool     `yaml:"x-internal"`
	ReadOnly    string   `yaml:"x-readonly"`
	Errors      []string `yaml:"x-errors"`
	ServedBy    string   `yaml:"x-served-by"`
	Deprecated  bool     `yaml:"deprecated"`
	Deprecation *struct {
		At     time.Time `yaml:"at"`
		Sunset time.Time `yaml:"sunset"`
	} `yaml:"x-deprecation"`
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// FromSpec は束ねた spec（gen.Spec）の全 operation の属性を読む。
// x-authz の欠けや表に無い値、ratelimits に無い policy、x-deprecation の順の誤りは Fault（I-12）。
// caddy が配る経路（x-served-by: caddy）は api に来ないので表に入れない。
func FromSpec(spec []byte, membership RoomMembership) (*AuthzTable, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fault.Wrap(err)
	}
	policies := map[string]bool{}
	for _, p := range gen.Policies {
		policies[p.Name] = true
	}
	t := &AuthzTable{routes: map[string]Route{}, byPattern: map[string]string{}, membership: membership}
	for path, item := range doc.Paths {
		for _, m := range httpMethods {
			node, ok := item[m]
			if !ok {
				continue
			}
			var op specOperation
			if err := node.Decode(&op); err != nil {
				return nil, fault.Wrap(err)
			}
			if op.ServedBy == "caddy" {
				continue
			}
			r, err := routeOf(path, m, op, policies)
			if err != nil {
				return nil, fault.Wrap(err)
			}
			if _, dup := t.routes[r.OperationId]; dup {
				return nil, fault.Wrap(fmt.Errorf("operationId が重なる: %s", r.OperationId))
			}
			t.routes[r.OperationId] = r
			t.byPattern[r.Method+" "+r.Path] = r.OperationId
		}
	}
	if len(t.routes) == 0 {
		return nil, fault.Wrap(fmt.Errorf("spec に operation が無い"))
	}
	return t, nil
}

// routeOf は 1 つの operation の属性を確かめて Route にする。
func routeOf(path, method string, op specOperation, policies map[string]bool) (Route, error) {
	r := Route{OperationId: op.OperationId, Method: strings.ToUpper(method), Path: path, Authz: op.Authz,
		Rule: op.Rule, Natural: op.Natural, RateLimit: op.RateLimit, WebSocket: op.WebSocket,
		Internal: op.Internal, Errors: op.Errors}
	if r.OperationId == "" {
		return r, fmt.Errorf("operationId が無い: %s %s", method, path)
	}
	if len(op.Authz) == 0 {
		return r, fmt.Errorf("x-authz が無い: %s", r.OperationId)
	}
	for _, a := range op.Authz {
		if !knownAuthz[a] {
			return r, fmt.Errorf("x-authz の値が表に無い: %s %s", r.OperationId, a)
		}
	}
	switch op.Reauth {
	case nil, false:
		r.Reauth = ReauthNone
	case true:
		r.Reauth = ReauthAlways
	case "conditional":
		r.Reauth = ReauthConditional
	default:
		return r, fmt.Errorf("x-reauth の値が表に無い: %s", r.OperationId)
	}
	switch IdempotencyKind(op.Idempotency) {
	case IdempotencyNone, IdempotencyKey, IdempotencyNatural:
		r.Idempotency = IdempotencyKind(op.Idempotency)
	default:
		return r, fmt.Errorf("x-idempotency の値が表に無い: %s", r.OperationId)
	}
	switch op.ReadOnly {
	case "":
	case "allow":
		r.ReadOnlyAllow = true
	default:
		return r, fmt.Errorf("x-readonly の値が表に無い: %s", r.OperationId)
	}
	for _, p := range op.RateLimit {
		if !policies[p] {
			return r, fmt.Errorf("x-ratelimit の policy が ratelimits.yaml に無い: %s %s", r.OperationId, p)
		}
	}
	if op.Deprecation != nil {
		if !op.Deprecated || op.Deprecation.Sunset.Before(op.Deprecation.At) {
			return r, fmt.Errorf("x-deprecation の形が違う: %s", r.OperationId)
		}
		at, sunset := op.Deprecation.At.UTC(), op.Deprecation.Sunset.UTC()
		r.DeprecatedAt, r.Sunset = &at, &sunset
	}
	return r, nil
}

// Route は operationId の属性を返す。
func (t *AuthzTable) Route(operationId string) (Route, bool) {
	r, ok := t.routes[operationId]
	return r, ok
}

// RouteByPattern は「動詞 path」（生成の HandleFunc の pattern）の属性を返す。
func (t *AuthzTable) RouteByPattern(pattern string) (Route, bool) {
	op, ok := t.byPattern[pattern]
	if !ok {
		return Route{}, false
	}
	return t.routes[op], true
}

// Routes は全 operation の属性（operationId の順）。
func (t *AuthzTable) Routes() []Route {
	out := make([]Route, 0, len(t.routes))
	for _, r := range t.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperationId < out[j].OperationId })
	return out
}

// Check は x-authz の配列のどれか 1 つを満たすかを判定する（第2章 10）。
// 未承認の端末は session か device_self を含む経路だけを通す（他は KEY-004）。
// どれも通らない時は、値ごとの理由のうち資源に結び付いた物（ROOM-002、ADM-001、KEY-001）を先に返す。
func (t *AuthzTable) Check(ctx context.Context, operationId string, p Principal, args AuthzArgs) *fault.Event {
	r, ok := t.routes[operationId]
	if !ok {
		return fault.New(gen.SRV003, nil)
	}
	var deny *fault.Event
	for _, a := range r.Authz {
		e, err := t.check(ctx, a, p, args)
		if err != nil {
			return fault.Wrap(err).AsEvent()
		}
		if e == nil {
			return nil
		}
		if deny == nil || rank(e.Code) > rank(deny.Code) {
			deny = e
		}
	}
	return deny
}

// rank は拒む番号の選び方。
func rank(code string) int {
	switch code {
	case gen.AUTH001:
		return 0
	case gen.KEY004:
		return 1
	default:
		return 2
	}
}

// check は 1 つの値の判定。nil は通す。
func (t *AuthzTable) check(ctx context.Context, a string, p Principal, args AuthzArgs) (*fault.Event, error) {
	if a == authzNone || passThrough[a] {
		return nil, nil
	}
	if a == authzShell {
		if p.Kind == KindShell {
			return nil, nil
		}
		return fault.New(gen.AUTH001, nil), nil
	}
	if p.Kind != KindUser || p.UserId == nil {
		return fault.New(gen.AUTH001, nil), nil
	}
	switch a {
	case authzSession:
		return nil, nil
	case authzDeviceSelf:
		if p.DeviceId != nil && args.DeviceId != nil && *p.DeviceId == *args.DeviceId {
			return nil, nil
		}
		return fault.New(gen.KEY001, nil), nil
	}
	if !p.DeviceApproved {
		return fault.New(gen.KEY004, nil), nil
	}
	switch a {
	case authzApproved:
		return nil, nil
	case authzAdmin:
		if p.Role == RoleAdmin {
			return nil, nil
		}
		return fault.New(gen.ADM001, nil), nil
	case authzRoomMember:
		if args.RoomId == nil {
			return fault.New(gen.ROOM002, nil), nil
		}
		ok, err := t.membership.IsMember(ctx, *p.UserId, *args.RoomId)
		if err != nil {
			return nil, err
		}
		if ok {
			return nil, nil
		}
		return fault.New(gen.ROOM002, nil), nil
	}
	return nil, fmt.Errorf("x-authz の値を判定できない: %s", a)
}
