// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// KeyBackupEntry は鍵のバックアップの一覧の 1 行（{pos, backup}。pos は端末の次の since）。
type KeyBackupEntry = gen.KeyBackupEntry

// KeyBackup は鍵のバックアップ（fkb-history/1）を持つ（B-084。第12章 5.6）。
type KeyBackup struct {
	d       *Deps
	signing *UserSigningKeys
}

// Put は PUT /me/keybackup/{group_id}/{epoch}。groupId は 16 バイトでなければ KEY-009。key_version が今の版と違えば KEY-011。
// 既にあれば上書きしない（戻りは偽で 204）。
func (b *KeyBackup) Put(ctx context.Context, tx *store.Tx, userId uuid.UUID, groupId []byte, epoch int64, h gen.FkbHistory1) (bool, error) {
	panic("未実装")
}

// Replace は PUT ...?rewrap=true。S の交代の後に本人が再認証して行を置き換える（再認証は呼ぶ側）。key_version が今の版と違えば
// KEY-011。pos を新しく振る（本人の他の端末が since で取り直す）。
func (b *KeyBackup) Replace(ctx context.Context, tx *store.Tx, userId uuid.UUID, groupId []byte, epoch int64, h gen.FkbHistory1) error {
	panic("未実装")
}

// ListSince は GET /me/keybackup?since=&limit=。pos の順に limit 件と has_more（limit + 1 件を読む）。
func (b *KeyBackup) ListSince(ctx context.Context, userId uuid.UUID, pos int64, limit int) ([]KeyBackupEntry, bool, error) {
	panic("未実装")
}

// DeleteAllOfUser は抹消で本人の鍵のバックアップを消す（hook OnUserErased）。
func (b *KeyBackup) DeleteAllOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	panic("未実装")
}
