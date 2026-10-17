// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/keys"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// membership は srv-boundary の RoomMembership の実装（Members へ委ねる）。
type membership struct {
	members *Members
}

var _ boundary.RoomMembership = membership{}

func (m membership) IsMember(ctx context.Context, userId, roomId uuid.UUID) (bool, error) {
	return m.members.IsMember(ctx, userId, roomId)
}

func (m membership) RoomRole(ctx context.Context, userId, roomId uuid.UUID) (boundary.RoomRole, error) {
	return m.members.RoomRole(ctx, userId, roomId)
}

// snapshotter は srv-boundary の Snapshotter の実装（Timeline.Snapshot へ委ねる）。
type snapshotter struct {
	timeline *Timeline
}

var _ boundary.Snapshotter = snapshotter{}

func (s snapshotter) Snapshot(ctx context.Context, p boundary.Principal) (gen.SyncSnapshot, int64, error) {
	in, err := s.timeline.Snapshot(ctx, p)
	if err != nil {
		return gen.SyncSnapshot{}, 0, err
	}
	return in.Snapshot, in.Pos, nil
}

// eventSource は srv-boundary の EventSource の実装（Timeline.ForFeed へ委ねる）。
type eventSource struct {
	timeline *Timeline
}

var _ boundary.EventSource = eventSource{}

func (e eventSource) ForFeed(ctx context.Context, userId, deviceId uuid.UUID, eventIds []uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	return e.timeline.ForFeed(ctx, userId, deviceId, eventIds)
}

// typingSink は srv-boundary の TypingSink の実装（Typing.Set へ委ねる）。
type typingSink struct {
	typing *Typing
}

var _ boundary.TypingSink = typingSink{}

func (t typingSink) Set(userId, roomId uuid.UUID, typing bool, timeout time.Duration) {
	t.typing.Set(userId, roomId, typing, timeout)
}

// roomReader は srv-keys の RoomReader の実装（tx を q として Members へ渡す。全体実装設計書 2.4）。
type roomReader struct {
	members *Members
}

var _ keys.RoomReader = roomReader{}

func (r roomReader) RoomsOf(ctx context.Context, tx *store.Tx, userId uuid.UUID) ([]uuid.UUID, error) {
	return r.members.RoomsOf(ctx, tx, userId)
}

func (r roomReader) MembersOf(ctx context.Context, tx *store.Tx, roomId uuid.UUID) ([]uuid.UUID, error) {
	return r.members.Audience(ctx, tx, roomId)
}

func (r roomReader) IsMemberOrPending(ctx context.Context, tx *store.Tx, roomId, userId uuid.UUID) (bool, error) {
	return r.members.IsMemberOrPending(ctx, tx, roomId, userId)
}
