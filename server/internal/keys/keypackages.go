// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// KeyPackageCount は端末の KeyPackage の残り（GET /me/devices/{id}/keypackages）。
type KeyPackageCount struct {
	// Count は使い切りの KeyPackage の数（最後の手段を含まない）。
	Count int
	// LastResort は最後の手段があるか。
	LastResort bool
}

// Taken は取り出した KeyPackage（MLSMessage の符号化）。LastResort なら応答に KEY-006 の警告を添える（最後の手段は消さない）。
type Taken struct {
	Package    []byte
	LastResort bool
}

// KeyPackages は KeyPackage の保管と確かめを持つ（B-082。第12章 5.2）。
type KeyPackages struct {
	d       *Deps
	reader  *CommitReader
	devices *DeviceKeys
}

// Push は POST /me/devices/{id}/keypackages。各要素を CommitReader.ParseKeyPackage で読み、第12章 5.2 の 5 つを確かめる（1 つでも
// 通らなければ KEY-009 で全部を拒む）。使い切りは最大 20（超えたら古い物から消す）、最後の手段は 1 つ（新しい物で置き換える）。
// 期限は 90 日。主キーは KeyPackageRef。戻りは使い切りの数。
func (p *KeyPackages) Push(ctx context.Context, tx *store.Tx, deviceId uuid.UUID, packages [][]byte, lastResort []byte) (int, error) {
	panic("未実装")
}

// Count は端末の残り。
func (p *KeyPackages) Count(ctx context.Context, deviceId uuid.UUID) (KeyPackageCount, error) {
	panic("未実装")
}

// Take は POST /users/{uid}/devices/{did}/keypackage。取り出しと削除を同じトランザクションで（I-11。FOR UPDATE SKIP LOCKED）。
// userId の構成員が無ければ MEM-001。deviceId が userId の端末でないか、承認済みか休眠でなければ KEY-001。尽きたら最後の手段
// （消さない。RFC 9420 16.8）。
func (p *KeyPackages) Take(ctx context.Context, tx *store.Tx, userId, deviceId uuid.UUID) (Taken, error) {
	panic("未実装")
}

// PruneExpired は期限を過ぎた KeyPackage を消す（srv-worker）。戻りは消した数。
func (p *KeyPackages) PruneExpired(ctx context.Context) (int64, error) {
	panic("未実装")
}

// DeleteAll は端末の KeyPackage を全部消す（hook OnDeviceRevoked）。
func (p *KeyPackages) DeleteAll(ctx context.Context, tx *store.Tx, deviceId uuid.UUID) error {
	panic("未実装")
}
