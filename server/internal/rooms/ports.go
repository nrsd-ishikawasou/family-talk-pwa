// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Notifier はイベントの後の通知の判定と送信（依存の逆転。本ブロックが定め、srv-push が実装して Block.SetNotifier で渡す。B-109）。
// recipients は送信者を除く active の構成員。判定（ミュート、メンション、通知なし送信）は srv-push の規則。
type Notifier interface {
	Notify(ctx context.Context, tx *store.Tx, event Event, recipients []uuid.UUID) error
}

// MediaBinder は添付の結び付き（srv-media Refs が実装して Block.SetMediaBinder で渡す。B-109。第2章 5.6、第5章 5.1、I-5）。
type MediaBinder interface {
	// Bind は media_refs に (media_id, ref_kind = event, ref_id = event_id, room_id) を置く。完了していない id は MEDIA-002、送信者が
	// 取得できない媒体（持ち主でも、結び付いたどのトークの構成員でもない）は MEDIA-005。
	Bind(ctx context.Context, tx *store.Tx, senderUserId, roomId, eventId uuid.UUID, mediaIds []uuid.UUID) error
	// BindScheduled は予約送信の添付を予約に結び付ける（確かめは Bind と同じ。取得の認可に数えない：送られるまで構成員から取れない）。
	BindScheduled(ctx context.Context, tx *store.Tx, senderUserId, roomId, scheduledId uuid.UUID, mediaIds []uuid.UUID) error
	// Rebind は予約の結び付きをイベントの結び付きに替える（予約送信を送った時。同じトーク）。
	Rebind(ctx context.Context, tx *store.Tx, scheduledId, eventId uuid.UUID) error
	// Unbind はイベントの参照を外す（削除済みの印。参照が 0 になれば srv-media が orphaned_at を書く）。
	Unbind(ctx context.Context, tx *store.Tx, eventId uuid.UUID) error
	// UnbindEvents は複数のイベントの参照を外す（一括の取消。1 回に 1,000 件まで）。
	UnbindEvents(ctx context.Context, tx *store.Tx, eventIds []uuid.UUID) error
	// UnbindScheduled は予約の結び付きを外す（予約送信の取り消しと、送れない予約）。
	UnbindScheduled(ctx context.Context, tx *store.Tx, scheduledId uuid.UUID) error
	// UnbindRoom はそのトークの結び付きを種類（イベント、予約）を問わず全部外す（トークの削除）。
	UnbindRoom(ctx context.Context, tx *store.Tx, roomId uuid.UUID) error
}

// CallEnder はトークの削除で進行中の通話を終える（srv-calls Calls.EndAllOfRoom が実装して Block.SetCallEnder で渡す。B-109）。
type CallEnder interface {
	// EndAll はそのトークの ringing と active の通話を ended にし、SFU のルームを閉じる。
	EndAll(ctx context.Context, tx *store.Tx, roomId uuid.UUID) error
}
