// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Apis は生成のタグ rooms の 25 操作の実装（gen.Implementations の Rooms の欄。ブロックのページの受ける節、第2章 5.5）。
// 各操作は主体を boundary.PrincipalFrom(ctx) で取り、書き込みは store.TxFrom(ctx) のトランザクション（無ければ開く）で行う。
// *fault.Event はそのまま error で返す（srv-boundary の strict の口が problem にする）。x-authz の room_member は境界が
// RoomMembership で確かめ、x-authz-rule は本ブロックが資源を読んでから判定する（第2章 10）。/admin/rooms と /admin/rooms/{id} は
// タグ admin で srv-admin が実装し、Rooms.AdminList と Rooms.Delete を呼ぶ。
type Apis struct {
	b *Block
}

var _ gen.RoomsApi = (*Apis)(nil)

// ListRooms は GET /rooms。x-authz approved。Rooms.List（本人の active と pending_add のトーク。識別子の順）
func (a *Apis) ListRooms(ctx context.Context, request gen.ListRoomsRequestObject) (gen.ListRoomsResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	c, err := newCursor(request.Params.Limit, request.Params.Before, request.Params.After)
	if err != nil {
		return nil, err
	}
	items, more, err := a.b.Rooms.List(ctx, a.b.d.Pool, me, c)
	if err != nil {
		return nil, err
	}
	return gen.ListRooms200JSONResponse{Items: items, HasMore: more}, nil
}

// CreateRoom は POST /rooms。x-authz approved、冪等 key。Rooms.Create（第4章 DD-4-1 の規則。dm の重なりは ROOM-004）
func (a *Apis) CreateRoom(ctx context.Context, request gen.CreateRoomRequestObject) (gen.CreateRoomResponseObject, error) {
	members, err := parseIds(request.Body.Members, "body/members")
	if err != nil {
		return nil, err
	}
	var out RoomView
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		out, err = a.b.Rooms.Create(ctx, tx, boundary.PrincipalFrom(ctx), RoomKind(request.Body.Kind), members)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateRoom200JSONResponse(out), nil
}

// DeleteRoom は DELETE /rooms/{id}。x-authz room_member、規則 room_delete（グループだけ。作成者か構成員の管理者。他の構成員と 1 対
// 1・自分用メモは ROOM-003、家族全員は ROOM-011。資源を読んでから行う確かめなので本ブロックが判定する。第2章 10）。Rooms.Delete
func (a *Apis) DeleteRoom(ctx context.Context, request gen.DeleteRoomRequestObject) (gen.DeleteRoomResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		return a.b.Rooms.Delete(ctx, tx, room, RuleRoomDelete, me, p.Role == boundary.RoleAdmin)
	})
	if err != nil {
		return nil, err
	}
	return gen.DeleteRoom200JSONResponse{}, nil
}

// GetRoom は GET /rooms/{id}。x-authz room_member。Rooms.View
func (a *Apis) GetRoom(ctx context.Context, request gen.GetRoomRequestObject) (gen.GetRoomResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	v, err := a.b.Rooms.View(ctx, a.b.d.Pool, me, room)
	if err != nil {
		return nil, err
	}
	return gen.GetRoom200JSONResponse(v), nil
}

// ListRoomEvents は GET /rooms/{id}/events。x-authz room_member。Timeline.Page（types の絞り、第2章 3 の cursor）
func (a *Apis) ListRoomEvents(ctx context.Context, request gen.ListRoomEventsRequestObject) (gen.ListRoomEventsResponseObject, error) {
	room, c, err := roomAndCursor(request.Id, request.Params.Limit, request.Params.Before, request.Params.After)
	if err != nil {
		return nil, err
	}
	var types []string
	if request.Params.Types != nil {
		for _, t := range *request.Params.Types {
			types = append(types, string(t))
		}
	}
	items, more, err := a.b.Timeline.Page(ctx, boundary.PrincipalFrom(ctx), room, c, types)
	if err != nil {
		return nil, err
	}
	return gen.ListRoomEvents200JSONResponse{Items: items, HasMore: more}, nil
}

// roomAndCursor はトークの識別子と一覧の位置。
func roomAndCursor(id string, limit *int, before, after *string) (uuid.UUID, Cursor, error) {
	room, err := parseId(id, "path.id")
	if err != nil {
		return uuid.Nil, Cursor{}, err
	}
	c, err := newCursor(limit, before, after)
	return room, c, err
}

// CreateRoomEvent は POST /rooms/{id}/events。x-authz room_member、冪等 key。Timeline.Post（transaction_id は
// Idempotency-Key、silent は要求の欄）
func (a *Apis) CreateRoomEvent(ctx context.Context, request gen.CreateRoomEventRequestObject) (gen.CreateRoomEventResponseObject, error) {
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	key, err := parseId(request.Params.IdempotencyKey, "header.Idempotency-Key")
	if err != nil {
		return nil, err
	}
	silent := request.Body.Silent != nil && *request.Body.Silent
	var out Posted
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		out, err = a.b.Timeline.Post(ctx, tx, boundary.PrincipalFrom(ctx), room, request.Body.Body, silent, key)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateRoomEvent200JSONResponse{EventId: out.EventId.String(), Pos: int(out.Pos), Ts: out.Ts.UTC(), TransactionId: optId(out.TransactionId)}, nil
}

// GetRoomEvent は GET /rooms/{id}/events/{eid}。x-authz room_member。Timeline.Get（削除済みの印は骨を 200）
func (a *Apis) GetRoomEvent(ctx context.Context, request gen.GetRoomEventRequestObject) (gen.GetRoomEventResponseObject, error) {
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	eid, err := parseId(request.Eid, "path.eid")
	if err != nil {
		return nil, err
	}
	sk, err := a.b.Timeline.Get(ctx, boundary.PrincipalFrom(ctx), room, eid)
	if err != nil {
		return nil, err
	}
	return gen.GetRoomEvent200JSONResponse(sk), nil
}

// RedactRoomEvent は POST /rooms/{id}/events/{eid}/redact。x-authz room_member、規則 redact、冪等
// natural（eid）。Timeline.Redact
func (a *Apis) RedactRoomEvent(ctx context.Context, request gen.RedactRoomEventRequestObject) (gen.RedactRoomEventResponseObject, error) {
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	eid, err := parseId(request.Eid, "path.eid")
	if err != nil {
		return nil, err
	}
	var reason *string
	if request.Body != nil {
		reason = request.Body.Reason
	}
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		return a.b.Timeline.Redact(ctx, tx, boundary.PrincipalFrom(ctx), room, eid, reason)
	})
	if err != nil {
		return nil, err
	}
	return gen.RedactRoomEvent200JSONResponse{}, nil
}

// ListRoomEventRelations は GET /rooms/{id}/events/{eid}/relations。x-authz
// room_member。Timeline.Relations（rel_type の絞り）
func (a *Apis) ListRoomEventRelations(ctx context.Context, request gen.ListRoomEventRelationsRequestObject) (gen.ListRoomEventRelationsResponseObject, error) {
	room, c, err := roomAndCursor(request.Id, request.Params.Limit, request.Params.Before, request.Params.After)
	if err != nil {
		return nil, err
	}
	eid, err := parseId(request.Eid, "path.eid")
	if err != nil {
		return nil, err
	}
	var rel *string
	if request.Params.RelType != nil {
		r := string(*request.Params.RelType)
		rel = &r
	}
	items, more, err := a.b.Timeline.Relations(ctx, boundary.PrincipalFrom(ctx), room, eid, rel, c)
	if err != nil {
		return nil, err
	}
	return gen.ListRoomEventRelations200JSONResponse{Items: items, HasMore: more}, nil
}

// ExportRoom は GET /rooms/{id}/export。x-authz room_member。Timeline.Export（骨の一括）
func (a *Apis) ExportRoom(ctx context.Context, request gen.ExportRoomRequestObject) (gen.ExportRoomResponseObject, error) {
	room, c, err := roomAndCursor(request.Id, request.Params.Limit, request.Params.Before, request.Params.After)
	if err != nil {
		return nil, err
	}
	items, more, err := a.b.Timeline.Export(ctx, boundary.PrincipalFrom(ctx), room, c)
	if err != nil {
		return nil, err
	}
	return gen.ExportRoom200JSONResponse{Items: items, HasMore: more}, nil
}

// PutMyRoomSettings は PUT /rooms/{id}/me。x-authz room_member。RoomMeStore.Put
func (a *Apis) PutMyRoomSettings(ctx context.Context, request gen.PutMyRoomSettingsRequestObject) (gen.PutMyRoomSettingsResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	set, err := a.b.RoomMe.fromGen(*request.Body)
	if err != nil {
		return nil, err
	}
	var out RoomMe
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		out, err = a.b.RoomMe.Put(ctx, tx, me, room, set)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.PutMyRoomSettings200JSONResponse(out.toGen()), nil
}

// ListRoomMembers は GET /rooms/{id}/members。x-authz room_member。Members.List（api の state の member は
// active。joined_at は active の人だけ）
func (a *Apis) ListRoomMembers(ctx context.Context, request gen.ListRoomMembersRequestObject) (gen.ListRoomMembersResponseObject, error) {
	room, c, err := roomAndCursor(request.Id, request.Params.Limit, request.Params.Before, request.Params.After)
	if err != nil {
		return nil, err
	}
	rows, more, err := a.b.Members.List(ctx, room, c)
	if err != nil {
		return nil, err
	}
	items := make([]gen.Member, len(rows))
	for i, m := range rows {
		items[i] = memberToGen(m)
	}
	return gen.ListRoomMembers200JSONResponse{Items: items, HasMore: more}, nil
}

// memberToGen は構成員の行を api の Member にする（active は member。joined_at は active の人だけ since）。
func memberToGen(m Member) gen.Member {
	out := gen.Member{UserId: m.UserId.String(), State: gen.MemberState(m.State)}
	if m.State == MemberActive {
		out.State = gen.MemberStateMember
		t := m.Since.UTC()
		out.JoinedAt = &t
	}
	return out
}

// AddRoomMember は POST /rooms/{id}/members。x-authz room_member、規則 not_dm、冪等
// natural（user_id）。Members.Add
func (a *Apis) AddRoomMember(ctx context.Context, request gen.AddRoomMemberRequestObject) (gen.AddRoomMemberResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	user, err := parseId(request.Body.UserId, "body/user_id")
	if err != nil {
		return nil, err
	}
	var out Member
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		out, err = a.b.Members.Add(ctx, tx, room, &me, user)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.AddRoomMember200JSONResponse(memberToGen(out)), nil
}

// RemoveRoomMember は DELETE /rooms/{id}/members/{uid}。x-authz room_member、規則
// room_member_remove（本人の退出、作成者、管理者）。Members.Remove
func (a *Apis) RemoveRoomMember(ctx context.Context, request gen.RemoveRoomMemberRequestObject) (gen.RemoveRoomMemberResponseObject, error) {
	p := boundary.PrincipalFrom(ctx)
	me, _, err := self(p)
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	user, err := parseId(request.Uid, "path.uid")
	if err != nil {
		return nil, err
	}
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		return a.b.Members.Remove(ctx, tx, room, me, p.Role == boundary.RoleAdmin, user)
	})
	if err != nil {
		return nil, err
	}
	return gen.RemoveRoomMember200JSONResponse{}, nil
}

// CreateRoomMlsCommit は POST /rooms/{id}/mls/commit。x-authz room_member（承認済みの端末）、冪等
// natural（epoch）。同じトランザクションで srv-keys Groups.Commit、Members.Resolve（Commit が返した端末）の順。RoomLock は
// Groups.Commit が取る（前に取らない）
func (a *Apis) CreateRoomMlsCommit(ctx context.Context, request gen.CreateRoomMlsCommitRequestObject) (gen.CreateRoomMlsCommitResponseObject, error) {
	_, device, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	commit, err := base64.RawURLEncoding.DecodeString(request.Body.Commit)
	if err != nil {
		return nil, netError("body/commit", "base64url でない")
	}
	var welcome []byte
	if request.Body.Welcome != nil {
		if welcome, err = base64.RawURLEncoding.DecodeString(*request.Body.Welcome); err != nil {
			return nil, netError("body/welcome", "base64url でない")
		}
	}
	var res gen.CreateRoomMlsCommitResponse
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		r, err := a.b.d.Keys.Groups.Commit(ctx, tx, room, device, int64(request.Body.Epoch), commit, welcome)
		if err != nil {
			return err
		}
		res = gen.CreateRoomMlsCommitResponse{Epoch: int(r.Epoch), CommitRef: r.CommitRef}
		return a.b.Members.Resolve(ctx, tx, room, r.Added, r.Removed)
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateRoomMlsCommit200JSONResponse(res), nil
}

// ListRoomMlsCommits は GET /rooms/{id}/mls/commits。x-authz room_member。srv-keys Groups.CommitsSince
func (a *Apis) ListRoomMlsCommits(ctx context.Context, request gen.ListRoomMlsCommitsRequestObject) (gen.ListRoomMlsCommitsResponseObject, error) {
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	since, limit := int64(0), pageDefault
	if request.Params.SinceEpoch != nil {
		since = int64(*request.Params.SinceEpoch)
	}
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	pg, err := a.b.d.Keys.Groups.CommitsSince(ctx, room, since, limit)
	if err != nil {
		return nil, err
	}
	out := gen.ListRoomMlsCommits200JSONResponse{Generation: pg.Generation, HasMore: pg.HasMore}
	out.Items = make([]struct {
		Commit    gen.CommonBase64url1 `json:"commit"`
		CommitRef string               `json:"commit_ref"`
		Epoch     int                  `json:"epoch"`
	}, len(pg.Items))
	for i, it := range pg.Items {
		out.Items[i].Commit = base64.RawURLEncoding.EncodeToString(it.Commit)
		out.Items[i].CommitRef, out.Items[i].Epoch = it.CommitRef, int(it.Epoch)
	}
	return out, nil
}

// GetRoomMlsEpoch は GET /rooms/{id}/mls/epoch。x-authz room_member。srv-keys
// Groups.Epoch（commit_possible を含む）に Members.PendingAddUsers を足す（2 つの読みは別の見え方。端末は room.member で取り直す）
func (a *Apis) GetRoomMlsEpoch(ctx context.Context, request gen.GetRoomMlsEpochRequestObject) (gen.GetRoomMlsEpochResponseObject, error) {
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	info, err := a.b.d.Keys.Groups.Epoch(ctx, room)
	if err != nil {
		return nil, err
	}
	users, err := a.b.Members.PendingAddUsers(ctx, a.b.d.Pool, room)
	if err != nil {
		return nil, err
	}
	return gen.GetRoomMlsEpoch200JSONResponse{Generation: info.Generation, GroupId: base64.RawURLEncoding.EncodeToString(info.GroupId),
		Epoch: int(info.Epoch), CommitPossible: info.CommitPossible, PendingAdd: idStr(users), PendingAddDevices: idStr(info.PendingAddDevices),
		PendingRemove: idStr(info.PendingRemove), PendingReadd: idStr(info.PendingReadd)}, nil
}

// ReinitRoomMls は POST /rooms/{id}/mls/reinit。x-authz room_member（承認済みの端末）、冪等 key。同じトランザクションで srv-keys
// Groups.Reinit、Members.Resolve（空の引数）の順
func (a *Apis) ReinitRoomMls(ctx context.Context, request gen.ReinitRoomMlsRequestObject) (gen.ReinitRoomMlsResponseObject, error) {
	_, device, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	var out gen.ReinitRoomMlsResponse
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		r, err := a.b.d.Keys.Groups.Reinit(ctx, tx, room, device)
		if err != nil {
			return err
		}
		out = gen.ReinitRoomMlsResponse{Generation: r.Generation, GroupId: base64.RawURLEncoding.EncodeToString(r.GroupId), Epoch: int(r.Epoch)}
		return a.b.Members.Resolve(ctx, tx, room, nil, nil)
	})
	if err != nil {
		return nil, err
	}
	return gen.ReinitRoomMls200JSONResponse(out), nil
}

// RejectRoomMlsCommit は POST /rooms/{id}/mls/reject。x-authz room_member（承認済みの端末）、冪等 natural。srv-keys
// Groups.Reject
func (a *Apis) RejectRoomMlsCommit(ctx context.Context, request gen.RejectRoomMlsCommitRequestObject) (gen.RejectRoomMlsCommitResponseObject, error) {
	_, device, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	var epoch *int64
	if request.Body.Epoch != nil {
		e := int64(*request.Body.Epoch)
		epoch = &e
	}
	var at time.Time
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		r, err := a.b.d.Keys.Groups.Reject(ctx, tx, room, device, request.Body.Generation, epoch, request.Body.CommitRef, string(request.Body.Reason))
		at = r.ReinitAllowedAt
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.RejectRoomMlsCommit200JSONResponse{ReinitAllowedAt: at.UTC()}, nil
}

// ListRoomReceipts は GET /rooms/{id}/receipts。x-authz room_member。Receipts.Of（私的な既読は本人の分だけ）
func (a *Apis) ListRoomReceipts(ctx context.Context, request gen.ListRoomReceiptsRequestObject) (gen.ListRoomReceiptsResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	rs, err := a.b.Receipts.Of(ctx, me, room)
	if err != nil {
		return nil, err
	}
	var out gen.ListRoomReceipts200JSONResponse
	out.Items = make([]struct {
		FullyRead   *gen.CommonUuidv71 `json:"fully_read,omitempty"`
		Read        *gen.CommonUuidv71 `json:"read,omitempty"`
		ReadPrivate *gen.CommonUuidv71 `json:"read_private,omitempty"`
		UserId      gen.CommonUuidv71  `json:"user_id"`
	}, len(rs))
	for i, r := range rs {
		out.Items[i].UserId, out.Items[i].Read, out.Items[i].ReadPrivate, out.Items[i].FullyRead =
			r.UserId.String(), optId(r.Read), optId(r.ReadPrivate), optId(r.FullyRead)
	}
	return out, nil
}

// CreateRoomReceipts は POST /rooms/{id}/receipts。x-authz room_member、冪等 natural（単調）。Receipts.Put
func (a *Apis) CreateRoomReceipts(ctx context.Context, request gen.CreateRoomReceiptsRequestObject) (gen.CreateRoomReceiptsResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	var ids [3]*uuid.UUID
	for i, pair := range []struct {
		s    *string
		path string
	}{{request.Body.Read, "body/read"}, {request.Body.ReadPrivate, "body/read_private"}, {request.Body.FullyRead, "body/fully_read"}} {
		if pair.s == nil {
			continue
		}
		id, err := parseId(*pair.s, pair.path)
		if err != nil {
			return nil, err
		}
		ids[i] = &id
	}
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		return a.b.Receipts.Put(ctx, tx, me, room, ids[0], ids[1], ids[2])
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateRoomReceipts200JSONResponse{}, nil
}

// ListRoomScheduled は GET /rooms/{id}/scheduled。x-authz room_member。Scheduled.List（本人の分だけ）
func (a *Apis) ListRoomScheduled(ctx context.Context, request gen.ListRoomScheduledRequestObject) (gen.ListRoomScheduledResponseObject, error) {
	room, c, err := roomAndCursor(request.Id, request.Params.Limit, request.Params.Before, request.Params.After)
	if err != nil {
		return nil, err
	}
	items, more, err := a.b.Scheduled.List(ctx, boundary.PrincipalFrom(ctx), room, c)
	if err != nil {
		return nil, err
	}
	return gen.ListRoomScheduled200JSONResponse{Items: items, HasMore: more}, nil
}

// CreateRoomScheduled は POST /rooms/{id}/scheduled。x-authz room_member、冪等 key。Scheduled.Create
func (a *Apis) CreateRoomScheduled(ctx context.Context, request gen.CreateRoomScheduledRequestObject) (gen.CreateRoomScheduledResponseObject, error) {
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	var out ScheduledView
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		out, err = a.b.Scheduled.Create(ctx, tx, boundary.PrincipalFrom(ctx), room, request.Body.SendAt, request.Body.Body)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gen.CreateRoomScheduled200JSONResponse(out), nil
}

// DeleteRoomScheduled は DELETE /rooms/{id}/scheduled/{sid}。x-authz room_member、規則 own_scheduled（他人の物は
// ROOM-007）。Scheduled.Cancel
func (a *Apis) DeleteRoomScheduled(ctx context.Context, request gen.DeleteRoomScheduledRequestObject) (gen.DeleteRoomScheduledResponseObject, error) {
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	sid, err := parseId(request.Sid, "path.sid")
	if err != nil {
		return nil, err
	}
	err = a.b.d.inTx(ctx, func(tx *store.Tx) error {
		return a.b.Scheduled.Cancel(ctx, tx, boundary.PrincipalFrom(ctx), room, sid)
	})
	if err != nil {
		return nil, err
	}
	return gen.DeleteRoomScheduled200JSONResponse{}, nil
}

// PutRoomTyping は PUT /rooms/{id}/typing。x-authz room_member、冪等 natural。Typing.Set（timeout が無ければ既定）
func (a *Apis) PutRoomTyping(ctx context.Context, request gen.PutRoomTypingRequestObject) (gen.PutRoomTypingResponseObject, error) {
	me, _, err := self(boundary.PrincipalFrom(ctx))
	if err != nil {
		return nil, err
	}
	room, err := parseId(request.Id, "path.id")
	if err != nil {
		return nil, err
	}
	var timeout time.Duration
	if request.Body.Timeout != nil {
		timeout = time.Duration(*request.Body.Timeout) * time.Second
	}
	a.b.Typing.Set(me, room, request.Body.Typing, timeout)
	return gen.PutRoomTyping200JSONResponse{}, nil
}
