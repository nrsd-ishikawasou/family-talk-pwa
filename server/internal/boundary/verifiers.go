// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
)

// 依存の逆転（B-042）：次の interface と値の型は本ブロックが定め、上のブロックが実装して渡す。

// Verifiers は認証と認可の判定の実装。
type Verifiers struct {
	Session    SessionVerifier
	Shell      ShellTokenVerifier
	Ws         WsTokenVerifier
	UpWs       UpWsVerifier
	Membership RoomMembership
	State      ServerStateReader
}

// SessionVerifier は Cookie __Host-session を確かめる（srv-identity）。
type SessionVerifier interface {
	// Verify は AUTH-001 か AUTH-002 の Event を返す。無操作の期限の延長もここで行う。
	Verify(ctx context.Context, cookie string) (Principal, *fault.Event)
	// ExpiresHeader は X-Session-Expires の時刻。
	ExpiresHeader(p Principal) time.Time
}

// ShellTokenVerifier はシェルのトークン（Authorization: Bearer。第2章 6.4）を確かめる（srv-identity）。
type ShellTokenVerifier interface {
	Verify(ctx context.Context, token string) (Principal, *fault.Event)
}

// UpWsVerifier は配布元の常時接続 /up/ws の秘密（Authorization: Bearer の ws_secret。第2章 4.4、6.4）を確かめ、端末の識別子を返す（srv-push）。
type UpWsVerifier interface {
	Verify(ctx context.Context, secret string) (uuid.UUID, *fault.Event)
}

// WsTokenVerifier は  WebSocket の auth のトークン（60 秒の使い切り）を確かめる（srv-identity）。
type WsTokenVerifier interface {
	Verify(ctx context.Context, token string) (Principal, *fault.Event)
}

// RoomMembership はトークの今の構成員の判定（srv-rooms。第2章 4.3）。
type RoomMembership interface {
	IsMember(ctx context.Context, userId, roomId uuid.UUID) (bool, error)
	RoomRole(ctx context.Context, userId, roomId uuid.UUID) (RoomRole, error)
}

// ServerState は家族のサーバーの状態（第1章 DD-1-15）。srv-admin の Startup が返す。
type ServerState string

// 状態の 5 つ。
const (
	StateBeforeSetup ServerState = "before_setup"
	StateInitial     ServerState = "initial"
	StateNormal      ServerState = "normal"
	StateReadOnly    ServerState = "read_only"
	StateStopped     ServerState = "stopped"
)

// ServerStateReader は今の状態を読む（srv-admin）。
type ServerStateReader interface {
	State() ServerState
	// ReadOnlyReason は ReadOnly の理由（SRV-001 の params.reason：migration、capacity、storage、restore）。
	ReadOnlyReason() string
}

// Links は本ブロックが呼ぶ上のブロックの処理。
type Links struct {
	Snapshot Snapshotter
	Events   EventSource
	Typing   TypingSink
	Up       UpMailbox
	Probe    HealthProbe
}

// Snapshotter は /sync の初回の一覧（srv-rooms Timeline.Snapshot）。snapshot と pos は 1 つの読みで取る。主体は構成員の一覧の
// 見え方（子ども区分）と端末の一覧の this_device に要る。予期した失敗は *fault.Event、他は *fault.Fault を error で返す。
type Snapshotter interface {
	Snapshot(ctx context.Context, p Principal) (gen.SyncSnapshot, int64, error)
}

// EventSource は feed の room.event の行の payload（ft.room_event/1）を引く（srv-rooms Timeline.ForFeed）。
// その利用者と端末に見える形で返し、見えない物は返さない（本ブロックはそのフレームを送らない）。
type EventSource interface {
	ForFeed(ctx context.Context, userId, deviceId uuid.UUID, eventIds []uuid.UUID) (map[uuid.UUID]json.RawMessage, error)
}

// TypingSink は入力中の通知（srv-rooms Typing.Set）。timeout が 0 なら既定（10 秒）。
type TypingSink interface {
	Set(userId, roomId uuid.UUID, typing bool, timeout time.Duration)
}

// UpMessage は配布元の常時接続で送る 1 件（asyncapi の upMsg）。
type UpMessage struct {
	Id uuid.UUID
	// Body は RFC 8291 の暗号文のまま。フレームでは base64url。
	Body      []byte
	ExpiresAt time.Time
}

// UpMailbox は配布元のメールボックス（srv-push UpBox）。
type UpMailbox interface {
	// Pending は未配信を古い順に返す。
	Pending(ctx context.Context, deviceId uuid.UUID) ([]UpMessage, error)
	// Ack は端末の ack で消す。その端末宛でない id は何もしない。
	Ack(ctx context.Context, deviceId, messageId uuid.UUID) error
}

// HealthFacts は srv-admin が /health に渡す値（第11章 11）。
type HealthFacts struct {
	CaddyCertDays int
	DiskFreePct   int
	DdnsLastOkAt  *time.Time
	Mode          string
}

// HealthProbe は /health の値の提供者（srv-admin）。
type HealthProbe interface {
	Probe(ctx context.Context) (HealthFacts, error)
}
