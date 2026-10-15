// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Group は MLS のグループの 1 行（mls_groups。トークごとに 1 つ。第12章 5.2）。
type Group struct {
	RoomId       uuid.UUID
	Epoch        int64
	Suite        int
	LastCommitAt time.Time
}

// Leaf は leaf の番号とその端末（mls_leaves）。
type Leaf struct {
	Index    int
	DeviceId uuid.UUID
}

// EpochInfo は GET /rooms/{id}/mls/epoch の本ブロックの分（応答の pending_add は srv-rooms が足す）。
type EpochInfo struct {
	Epoch             int64
	PendingAddDevices []uuid.UUID
	PendingRemove     []uuid.UUID
}

// CommitItem は GET /rooms/{id}/mls/commits の 1 件（epoch は Commit で進んだ後の epoch）。
type CommitItem struct {
	Epoch  int64
	Commit []byte
}

// CommitResult は受け付けた Commit の結果（srv-rooms Members.Resolve が構成員の待ちを解く元）。
type CommitResult struct {
	Epoch   int64
	Added   []uuid.UUID
	Removed []uuid.UUID
}

// RoomDevice はトークと端末の組（休眠の leaf）。
type RoomDevice struct {
	RoomId   uuid.UUID
	DeviceId uuid.UUID
}

// PendingKind は端末の追加待ち・削除待ちの種類（mls_pending.kind）。
type PendingKind string

// 待ちの 2 つ。
const (
	PendingAdd    PendingKind = "add"
	PendingRemove PendingKind = "remove"
)

// PendingDevice は残っている端末の待ち（srv-worker の監視。add が 7 日残れば知らせ）。
type PendingDevice struct {
	RoomId   uuid.UUID
	DeviceId uuid.UUID
	Kind     PendingKind
	Since    time.Time
}

// Groups は MLS のグループの epoch、leaf の記録、端末の追加待ち・削除待ち、Commit を持つ（B-086。第12章 5.2、5.5）。
type Groups struct {
	d       *Deps
	keys    *Keys
	reader  *CommitReader
	inbox   *Inbox
	bundles *KeyBundles
	devices *DeviceKeys
}

// Create は epoch 0 のグループを作り、leaf 0 に creatorDevice を置く（トークの作成と同じトランザクション。srv-rooms から）。
func (g *Groups) Create(ctx context.Context, tx *store.Tx, roomId, creatorDevice uuid.UUID) error {
	panic("未実装")
}

// Epoch は今の epoch と端末の待ち。グループが無ければ ROOM-001。
func (g *Groups) Epoch(ctx context.Context, roomId uuid.UUID) (EpochInfo, error) {
	panic("未実装")
}

// CommitsSince は sinceEpoch より後の Commit を epoch の順に最大 limit 件（上限 100）と has_more。
func (g *Groups) CommitsSince(ctx context.Context, roomId uuid.UUID, sinceEpoch int64, limit int) ([]CommitItem, bool, error) {
	panic("未実装")
}

// Commit は POST /rooms/{id}/mls/commit の本体（srv-rooms が受け、同じトランザクションで本操作、srv-rooms Members.Resolve の順に呼ぶ）。
// RoomLock の中で epoch を expectedEpoch から 1 進め（違えば KEY-007。I-4）、CommitReader.ParseCommit で読んで第12章 5.2 の確かめを
// 行い（通らなければ KEY-009 で epoch は進まない）、leaf の記録と待ちを更新し、Commit を置き、Welcome を Add の各端末へ、
// room.mls と room.member を構成員へ流す。休眠の端末が加われば srv-identity Devices.MarkActive。
func (g *Groups) Commit(ctx context.Context, tx *store.Tx, roomId, byDevice uuid.UUID, expectedEpoch int64, commit, welcome []byte) (CommitResult, error) {
	panic("未実装")
}

// CheckSender は送信の時の外形の検査（srv-rooms Timeline.Post の 4。第12章 5.2）。CommitReader.ParsePrivateHeader で本文を読み、
// wire format と content_type（ROOM-005）、外側と本文の group_id（ROOM-005）、外側と本文の epoch が今の epoch か（ROOM-006）、
// 送信した端末が leaf の記録にあるか（ROOM-006）を確かめる。RoomLock の中で呼ばれる。
func (g *Groups) CheckSender(ctx context.Context, roomId, device uuid.UUID, body gen.FtEventMls1) error {
	panic("未実装")
}

// Leaves は leaf の番号と端末（番号の順）。
func (g *Groups) Leaves(ctx context.Context, q store.Querier, roomId uuid.UUID) ([]Leaf, error) {
	panic("未実装")
}

// DormantLeafs は srv-identity Devices.ApprovedSeenBefore(olderThan) の端末が持つ leaf（srv-worker DormantLeafHandler）。
func (g *Groups) DormantLeafs(ctx context.Context, olderThan time.Time) ([]RoomDevice, error) {
	panic("未実装")
}

// RetireDormant は休眠（第12章 5.2）：RemoveLeafsOfDevice、KeyBundles.DeleteOfDevice、srv-identity Devices.MarkDormant の順
// （srv-worker から端末ごとに 1 トランザクション）。
func (g *Groups) RetireDormant(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	panic("未実装")
}

// RemoveLeafsOfDevice は取り消し・削除・休眠：その端末が leaf を持つ各グループに削除待ちを置き、room.member（pending_remove）を
// 構成員へ流す。その端末の追加待ちは消す。leaf の記録は Remove の Commit で消える。
func (g *Groups) RemoveLeafsOfDevice(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	panic("未実装")
}

// RequestAdd は端末の追加待ち（第12章 5.5 の 4）：本人が構成員のトーク（RoomReader.RoomsOf）で、その端末の leaf が無いグループの
// 全部に追加待ちを置き、room.member（pending_add）を流す。既にあれば置かない。戻りは置いた数（POST /me/mls/rejoin の応答）。
func (g *Groups) RequestAdd(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) (int, error) {
	panic("未実装")
}

// PendingDevices は残っている端末の待ち（srv-worker の監視）。
func (g *Groups) PendingDevices(ctx context.Context) ([]PendingDevice, error) {
	panic("未実装")
}

// dropPendingAdds は停止で本人の端末の追加待ちを消す（hook OnUserSuspended。leaf と削除待ちは触らない）。
func (g *Groups) dropPendingAdds(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}
