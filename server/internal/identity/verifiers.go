// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"time"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
)

// Verifiers は srv-boundary の逆転の interface の実装（B-066）。boundary.Verifiers の 3 つの欄に渡す。
// Principal.DeviceApproved は devices.state で決める（承認済みか休眠。承認の正。全体実装設計書 3.1）。
type Verifiers struct {
	Session boundary.SessionVerifier
	Ws      boundary.WsTokenVerifier
	Shell   boundary.ShellTokenVerifier
}

type sessionVerifier struct{ s *Sessions }

var _ boundary.SessionVerifier = sessionVerifier{}

// Verify は Sessions.Verify。
func (v sessionVerifier) Verify(ctx context.Context, cookie string) (boundary.Principal, *fault.Event) {
	return v.s.Verify(ctx, cookie)
}

// ExpiresHeader は Sessions.ExpiresAt。
func (v sessionVerifier) ExpiresHeader(p boundary.Principal) time.Time {
	return v.s.ExpiresAt(p)
}

type wsVerifier struct{ s *Sessions }

var _ boundary.WsTokenVerifier = wsVerifier{}

// Verify は Sessions.VerifyWsToken。
func (v wsVerifier) Verify(ctx context.Context, token string) (boundary.Principal, *fault.Event) {
	return v.s.VerifyWsToken(ctx, token)
}

type shellVerifier struct{ s *Sessions }

var _ boundary.ShellTokenVerifier = shellVerifier{}

// Verify は Sessions.VerifyShellToken。
func (v shellVerifier) Verify(ctx context.Context, token string) (boundary.Principal, *fault.Event) {
	return v.s.VerifyShellToken(ctx, token)
}
