// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"github.com/gofrs/uuid/v5"
)

// feed のイベントの payload（asyncapi の各 message の payload。additionalProperties は false。第2章 6.2）。

// deviceInbox は device.inbox（宛先の端末の件数）。
type deviceInbox struct {
	Count int `json:"count"`
}

// roomMls は room.mls（新しい Commit がある）。
type roomMls struct {
	Epoch           int64  `json:"epoch"`
	CommitRef       string `json:"commit_ref"`
	HasWelcomeForMe bool   `json:"has_welcome_for_me"`
}

// roomMember は room.member（端末の追加待ち・削除待ちと、Commit の added・removed）。
type roomMember struct {
	UserId   uuid.UUID  `json:"user_id"`
	DeviceId *uuid.UUID `json:"device_id,omitempty"`
	Change   string     `json:"change"`
	By       *uuid.UUID `json:"by,omitempty"`
}

// room.member の change のうち本ブロックが書く 4 つ（left は srv-rooms）。
const (
	memberAdded         = "added"
	memberRemoved       = "removed"
	memberPendingAdd    = "pending_add"
	memberPendingRemove = "pending_remove"
)

// keybundleChanged は keybundle.changed（変わった系統）。
type keybundleChanged struct {
	Kinds []BundleKind `json:"kinds"`
}

// userChanged は user.changed（S の交代で fields に signing_key。家族の端末の TOFU の警告）。
type userChanged struct {
	UserId uuid.UUID `json:"user_id"`
	Fields []string  `json:"fields"`
}

// fieldSigningKey は user.changed の fields の値（本人の署名鍵の公開部が変わった）。
const fieldSigningKey = "signing_key"

// recoveryRequestEvent は recovery.request（片を持つ構成員へ）。
type recoveryRequestEvent struct {
	RequestId  uuid.UUID `json:"request_id"`
	UserId     uuid.UUID `json:"user_id"`
	DeviceId   uuid.UUID `json:"device_id"`
	DeviceName string    `json:"device_name"`
	ExpiresAt  string    `json:"expires_at"`
}

// recoveryUsed は recovery.used。
type recoveryUsed struct {
	UserId   uuid.UUID      `json:"user_id"`
	Method   RecoveryMethod `json:"method"`
	DeviceId *uuid.UUID     `json:"device_id,omitempty"`
	Result   RecoveryResult `json:"result"`
}

// 知らせの種類（第7章 9）。本ブロックは srv-identity Hooks.Notify で出す。
const (
	noticeRecoveryUsed      = "recovery_used"
	noticeFamilyKeyUsed     = "family_key_used"
	noticeRecoveryRequested = "recovery_requested"
)
