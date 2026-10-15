// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package keys は MLS の配送サービスと認証サービス、鍵の運搬を持つ（srv-keys。基本設計書 第12章 5.2〜5.6、9、12、13、
// 第2章 5.4、5.5）。本人の署名鍵の公開部、端末の鍵の登録と承認、KeyPackage、鍵バンドルのコピー、回復コードの照合の値、
// 秘密分散の断片と回復の求め、家族回復鍵の公開部、鍵のバックアップ、端末宛メールボックス、Commit の直列化と読み取り（leaf の記録）。
// 暗号は Go の標準の部品だけで、MLS の暗号は持たない（MLS の物は RFC 9420 の TLS 符号化を CommitReader で読むだけ）。
// 予期した失敗はエラーコード表の *fault.Event、予期しない失敗は *fault.Fault を error で返す（全体実装設計書 2.7）。
// 書き込みは呼ぶ側が渡す *store.Tx の中で行う（I-8）。feed への書き込みは store.Feed.Append だけ（全体実装設計書 2.4）。
package keys

import (
	"context"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 固定の値（第12章と第2章の本文の値。設定の項目が無い物）。
const (
	keyPackageMax       = 20                  // 使い切りの KeyPackage の上限（第12章 5.2、第2章 5.4）。最後の手段は別に 1
	keyPackageTtl       = 90 * 24 * time.Hour // 置いてから（第12章 5.2）
	inboxItemMax        = 64 << 10            // メールボックスの 1 件の暗号文（base64url を解いた後。第2章 5.4）
	inboxDeviceMax      = 1000                // 1 端末の件数（KEY-010。第2章 5.4）
	inboxTtl            = 30 * 24 * time.Hour // 第12章 5.2
	recoveryRequestTtl  = 24 * time.Hour      // 第12章 5.4 (c)、第2章 6.2
	shareThreshold      = 2                   // 秘密分散のしきい値（第12章 5.4 (c)）
	commitListMax       = 100                 // Commit の一覧の上限（第2章 5.5）
	pendingAddNoticeAge = 7 * 24 * time.Hour  // 端末の追加待ちが残った時の知らせ（全体実装設計書 4.2）
	mlsSuite            = 0x0002              // 暗号スイート（DD-12-2）
)

// RoomReader はトークの一覧と構成員を読む（依存の逆転。本ブロックが定め、srv-rooms が実装して Keys.SetRooms で渡す。B-088）。
// tx は呼ぶ側のトランザクション（同じトランザクションの中の構成員の変化を読む）。
type RoomReader interface {
	// RoomsOf は本人が active か pending_add の構成員のトーク（端末の追加待ちを立てる先）。
	RoomsOf(ctx context.Context, tx *store.Tx, userId uuid.UUID) ([]uuid.UUID, error)
	// MembersOf は room.mls と room.member の宛先（active、pending_add、pending_remove の構成員）。
	MembersOf(ctx context.Context, tx *store.Tx, roomId uuid.UUID) ([]uuid.UUID, error)
	// IsMemberOrPending は本人がそのトークの active か pending_add の構成員か（Commit の Add の確かめ。第12章 5.2）。
	IsMemberOrPending(ctx context.Context, tx *store.Tx, roomId, userId uuid.UUID) (bool, error)
}

// Deps は本ブロックが使う下のブロック。
type Deps struct {
	Pool     *store.Pool
	Feed     *store.Feed
	RoomLock store.RoomLock
	Ids      *ids.Ids
	Clock    clock.Clock
	// Identity は srv-identity（端末の状態、構成員、家族、保護者、パスキー、hook と知らせ）。
	Identity *identity.Identity
}

// Keys は本ブロックの公開操作（B-080〜B-088）を束ねる。
type Keys struct {
	d                 *Deps
	rooms             RoomReader
	reader            *CommitReader
	UserSigningKeys   *UserSigningKeys
	DeviceKeys        *DeviceKeys
	KeyPackages       *KeyPackages
	KeyBundles        *KeyBundles
	RecoveryCode      *RecoveryCode
	Shares            *Shares
	FamilyRecoveryKey *FamilyRecoveryKey
	KeyBackup         *KeyBackup
	Inbox             *Inbox
	Groups            *Groups
	Recovery          *Recovery
	Hooks             *Hooks
}

// New は本ブロックを組む。RoomReader は srv-rooms が組まれた後に SetRooms で渡し、hook の登録は Register、最後に Ready を呼ぶ。
// 必須の欄が無いのはプログラムの誤り。
func New(d Deps) *Keys {
	if d.Pool == nil || d.Feed == nil || d.Ids == nil || d.Clock == nil || d.Identity == nil {
		panic("keys: Deps の必須の欄が無い")
	}
	dp := &d
	k := &Keys{d: dp, reader: &CommitReader{}}
	k.Recovery = &Recovery{d: dp}
	k.UserSigningKeys = &UserSigningKeys{d: dp}
	k.DeviceKeys = &DeviceKeys{d: dp, signing: k.UserSigningKeys}
	k.UserSigningKeys.devices = k.DeviceKeys
	k.KeyBundles = &KeyBundles{d: dp, devices: k.DeviceKeys, signing: k.UserSigningKeys}
	k.UserSigningKeys.bundles = k.KeyBundles
	k.KeyPackages = &KeyPackages{d: dp, reader: k.reader, devices: k.DeviceKeys}
	k.RecoveryCode = &RecoveryCode{d: dp, bundles: k.KeyBundles, recovery: k.Recovery}
	k.Inbox = &Inbox{d: dp, devices: k.DeviceKeys}
	k.Shares = &Shares{d: dp, inbox: k.Inbox, bundles: k.KeyBundles, recovery: k.Recovery}
	k.FamilyRecoveryKey = &FamilyRecoveryKey{d: dp, bundles: k.KeyBundles, recovery: k.Recovery}
	k.KeyBackup = &KeyBackup{d: dp, signing: k.UserSigningKeys}
	k.Groups = &Groups{d: dp, keys: k, reader: k.reader, inbox: k.Inbox, bundles: k.KeyBundles, devices: k.DeviceKeys}
	k.DeviceKeys.groups = k.Groups
	k.Hooks = &Hooks{d: dp, devices: k.DeviceKeys, packages: k.KeyPackages, bundles: k.KeyBundles, groups: k.Groups,
		inbox: k.Inbox, codes: k.RecoveryCode, shares: k.Shares, backup: k.KeyBackup, signing: k.UserSigningKeys,
		recovery: k.Recovery}
	return k
}

// SetRooms は srv-rooms の RoomReader を渡す（srv-rooms は本ブロックの上なので組み立ての後）。
func (k *Keys) SetRooms(r RoomReader) {
	k.rooms = r
}

// Register は srv-identity の hook の 6 つに本ブロックの処理を登録する（組み立ての中で 1 回。B-087）。
func (k *Keys) Register(id *identity.Identity) {
	h := id.Hooks
	h.RegisterOnDeviceRevoked(k.Hooks.OnDeviceRevoked)
	h.RegisterOnUserSuspended(k.Hooks.OnUserSuspended)
	h.RegisterOnUserErased(k.Hooks.OnUserErased)
	h.RegisterOnUserDeleted(k.Hooks.OnUserDeleted)
	h.RegisterOnPasskeyDeleted(k.Hooks.OnPasskeyDeleted)
	h.RegisterOnPasskeyReset(k.Hooks.OnPasskeyReset)
}

// errNoRooms は RoomReader が渡されていない（起動の組み立ての誤り）。
var errNoRooms = errors.New("keys: RoomReader が渡されていない")

// Ready は起動の最後に呼ぶ。RoomReader の無い組み立ては Fault（起動しない）。
func (k *Keys) Ready() error {
	if k.rooms == nil {
		return fault.Wrap(errNoRooms)
	}
	return nil
}

// Apis は生成のタグ keys の 22 操作の実装を返す（gen.Implementations の Keys の欄）。
func (k *Keys) Apis() *Apis {
	return &Apis{k: k}
}

// Reader は srv-identity の DeviceKeyReader の実装を返す（Identity.SetKeys に渡す）。
func (k *Keys) Reader() identity.DeviceKeyReader {
	return reader{devices: k.DeviceKeys, signing: k.UserSigningKeys}
}

// now は記録の時刻（Deps.Clock）。
func (d *Deps) now() time.Time {
	return d.Clock.Now()
}
