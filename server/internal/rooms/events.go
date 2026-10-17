// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
)

// 本ブロックが feed に書くイベントの種類（第2章 6.2。asyncapi の channels）。room.typing は feed に書かず入力中の送り先で流す。
const (
	kindRoomEvent     = "room.event" // payload は空（srv-boundary が Timeline.ForFeed で中身を引く）
	kindRoomMember    = "room.member"
	kindRoomSettings  = "room.settings"
	kindRoomReceipt   = "room.receipt"
	kindRoomMe        = "room.me"
	kindRoomScheduled = "room.scheduled"
	kindRoomTyping    = "room.typing"
)

// roomMember は room.member の payload（asyncapi の wsRoomMember）。device_id は端末の待ち（srv-keys が書く）だけ。
type roomMember struct {
	UserId   uuid.UUID  `json:"user_id"`
	DeviceId *uuid.UUID `json:"device_id,omitempty"`
	Change   string     `json:"change"`
	By       *uuid.UUID `json:"by,omitempty"`
}

// room.member の change のうち本ブロックが書く物（pending_readd は srv-keys だけ）。
const (
	changeAdded         = "added"
	changeRemoved       = "removed"
	changeLeft          = "left"
	changePendingAdd    = "pending_add"
	changePendingRemove = "pending_remove"
)

// roomSettings は room.settings の payload（kind と、グループの作成者）。
type roomSettings struct {
	Settings struct {
		Kind      RoomKind   `json:"kind"`
		CreatorId *uuid.UUID `json:"creator_id,omitempty"`
	} `json:"settings"`
}

// roomReceipt は room.receipt の payload（公開は構成員へ、私的と読み終えた位置は本人へ）。
type roomReceipt struct {
	UserId      uuid.UUID  `json:"user_id"`
	Read        *uuid.UUID `json:"read,omitempty"`
	ReadPrivate *uuid.UUID `json:"read_private,omitempty"`
	FullyRead   *uuid.UUID `json:"fully_read,omitempty"`
}

// roomMe は room.me の payload（本人の全端末）。
type roomMe struct {
	Settings gen.FtSettingsRoomMe1 `json:"settings"`
}

// roomScheduled は room.scheduled の payload（本人）。
type roomScheduled struct {
	ScheduledId uuid.UUID      `json:"scheduled_id"`
	State       ScheduledState `json:"state"`
}

// roomTyping は room.typing の payload（入力中の人と残りの秒）。
type roomTyping struct {
	UserIds []uuid.UUID `json:"user_ids"`
	Timeout int         `json:"timeout"`
}

// ft.event.system/1 の kind のうち本ブロックが書く物（第4章 6、第2章 12）。他の kind は呼ぶ側（srv-admin、srv-push、srv-worker、
// srv-keys）が FamilyRoom.Flow と SystemEvent に渡す。
const (
	systemRoomCreated       = "room_created"
	systemMemberAdded       = "member_added"
	systemMemberLeft        = "member_left"
	systemMemberRemoved     = "member_removed"
	systemMemberSuspended   = "member_suspended"
	systemMemberUnsuspended = "member_unsuspended"
	systemMemberDeleted     = "member_deleted"
	systemMemberErased      = "member_erased"
)
