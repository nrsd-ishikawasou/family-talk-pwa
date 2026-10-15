// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// InboxKind は端末宛メールボックスの種類（第12章 5.2 の表）。
type InboxKind string

// 種類の 8 つ。welcome は Commit の受け付けでサーバーだけが置く（PutWelcome。端末からの要求の kind の列挙に無く、境界の形の検査で
// NET-001）。
const (
	InboxWelcome              InboxKind = "welcome"
	InboxKeybundle            InboxKind = "keybundle"
	InboxKeyBackup            InboxKind = "key_backup"
	InboxKeyRequest           InboxKind = "key_request"
	InboxShare                InboxKind = "share"
	InboxCallKeyRequest       InboxKind = "call_key_request"
	InboxCallKey              InboxKind = "call_key"
	InboxStickerRemoveRequest InboxKind = "sticker_remove_request"
)

// InboxItem は端末宛メールボックスの 1 件（GET /me/inbox の要素）。
type InboxItem struct {
	Id         uuid.UUID
	Kind       InboxKind
	FromDevice uuid.UUID
	// Ciphertext は HPKE の暗号文（welcome は Welcome の MLSMessage そのもの）。
	Ciphertext []byte
	// Sig は送り手の端末の署名（r と s の 64 バイト）。welcome だけ nil。
	Sig       []byte
	Pos       int64
	CreatedAt time.Time
}

// Inbox は端末宛メールボックス（to-device）を持つ（B-085。第12章 5.2、第2章 5.4）。
type Inbox struct {
	d       *Deps
	devices *DeviceKeys
}

// Put は POST /devices/{did}/inbox と本ブロック・srv-rooms の中から。kind welcome を渡すのはプログラムの誤り（Fault。welcome は
// PutWelcome）。sig を fromDevice の登録した署名鍵で第12章 5.2 の対象（ft.inbox/1、0、kind、0、from、to、暗号文）に対して確かめ、
// 通らなければ KEY-009。暗号文が 64 KiB を超えれば KEY-009、1 端末 1,000 件で KEY-010。device.inbox{count} を toDevice の持ち主の
// feed に宛先の端末 = toDevice で書く。期限 30 日。
func (i *Inbox) Put(ctx context.Context, tx *store.Tx, fromDevice, toDevice uuid.UUID, kind InboxKind, ciphertext, sig []byte) (uuid.UUID, error) {
	panic("未実装")
}

// PutWelcome は Groups.Commit から。kind welcome、署名は無し（Welcome の GroupInfo の署名で守られる）。上限と device.inbox は Put と同じ。
func (i *Inbox) PutWelcome(ctx context.Context, tx *store.Tx, fromDevice, toDevice uuid.UUID, welcome []byte) (uuid.UUID, error) {
	panic("未実装")
}

// ListSince は GET /me/inbox?since=&limit=。pos の順に limit 件と has_more（limit + 1 件を読む）。
func (i *Inbox) ListSince(ctx context.Context, deviceId uuid.UUID, pos int64, limit int) ([]InboxItem, bool, error) {
	panic("未実装")
}

// Delete は DELETE /me/inbox/{id}（受け取ったら消す）。その端末宛ての物だけ。無ければ何もしない（DELETE の冪等）。
func (i *Inbox) Delete(ctx context.Context, tx *store.Tx, deviceId, id uuid.UUID) error {
	panic("未実装")
}

// Prune は期限を過ぎた項目を消す（srv-worker PruneHandler）。戻りは消した数。
func (i *Inbox) Prune(ctx context.Context) (int64, error) {
	panic("未実装")
}

// DeleteAllOfDevice は端末宛ての項目を全部消す（hook OnDeviceRevoked）。
func (i *Inbox) DeleteAllOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	panic("未実装")
}
