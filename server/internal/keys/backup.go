// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"context"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// KeyBackupEntry は鍵のバックアップの一覧の 1 行（{pos, backup}。pos は端末の次の since）。
type KeyBackupEntry = gen.KeyBackupEntry

// KeyBackup は鍵のバックアップ（fkb-history/1）を持つ（B-084。第12章 5.6）。
type KeyBackup struct {
	d       *Deps
	signing *UserSigningKeys
}

// groupIdLen はグループの識別子の長さ（トークの識別子の 16 バイトと世代の 4 バイト。第12章 5.2）。
const groupIdLen = 20

// check は経路と本文の照らし（group_id が 20 バイト、本文の user_id と group_id と epoch が経路と同じ。違えば KEY-009）と、
// key_version が今の版か（違えば KEY-011）。
func (b *KeyBackup) check(ctx context.Context, tx *store.Tx, userId uuid.UUID, groupId []byte, epoch int64, h gen.FkbHistory1) error {
	if len(groupId) != groupIdLen {
		return ev(cgen.KEY009)
	}
	u, err := uuid.FromString(h.UserId)
	body, ok := decodeB64(h.GroupId)
	if err != nil || u != userId || !ok || !bytes.Equal(body, groupId) || int64(h.Epoch) != epoch {
		return ev(cgen.KEY009)
	}
	v, err := b.signing.Version(ctx, tx, userId)
	if err != nil {
		return err
	}
	if h.KeyVersion != v {
		return fault.New(cgen.KEY011, map[string]any{"version": v})
	}
	return nil
}

// Put は PUT /me/keybackup/{group_id}/{epoch}。group_id は 20 バイトでなければ KEY-009。key_version が今の版と違えば KEY-011。
// 既にあれば上書きしない（戻りは偽で 204）。created_at はサーバーの時刻。
func (b *KeyBackup) Put(ctx context.Context, tx *store.Tx, userId uuid.UUID, groupId []byte, epoch int64, h gen.FkbHistory1) (bool, error) {
	if err := b.check(ctx, tx, userId, groupId, epoch, h); err != nil {
		return false, err
	}
	h.CreatedAt = b.d.now().UTC()
	tag, err := tx.Exec(ctx, `INSERT INTO key_backup (user_id, group_id, epoch, key_version, body, created_at) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, group_id, epoch) DO NOTHING`, userId, groupId, epoch, h.KeyVersion, h, h.CreatedAt)
	if err != nil {
		return false, wrap(err)
	}
	return tag.RowsAffected() == 1, nil
}

// Replace は PUT ...?rewrap=true。S の交代の後に本人が再認証して行を置き換える（再認証は呼ぶ側）。key_version が今の版と違えば
// KEY-011。行が無くても置く。pos を新しく振る（本人の他の端末が since で取り直す）。
func (b *KeyBackup) Replace(ctx context.Context, tx *store.Tx, userId uuid.UUID, groupId []byte, epoch int64, h gen.FkbHistory1) error {
	if err := b.check(ctx, tx, userId, groupId, epoch, h); err != nil {
		return err
	}
	h.CreatedAt = b.d.now().UTC()
	_, err := tx.Exec(ctx, `INSERT INTO key_backup (user_id, group_id, epoch, key_version, body, created_at) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, group_id, epoch) DO UPDATE SET key_version = EXCLUDED.key_version, body = EXCLUDED.body, created_at = EXCLUDED.created_at,
  pos = nextval(pg_get_serial_sequence('key_backup', 'pos'))`, userId, groupId, epoch, h.KeyVersion, h, h.CreatedAt)
	return wrap(err)
}

// ListSince は GET /me/keybackup?since=&limit=。pos の順に limit 件と has_more（limit + 1 件を読む）。
func (b *KeyBackup) ListSince(ctx context.Context, userId uuid.UUID, pos int64, limit int) ([]KeyBackupEntry, bool, error) {
	rows, err := b.d.Pool.Query(ctx, `SELECT pos, body FROM key_backup WHERE user_id = $1 AND pos > $2 ORDER BY pos LIMIT $3`, userId, pos, limit+1)
	if err != nil {
		return nil, false, wrap(err)
	}
	defer rows.Close()
	out := []KeyBackupEntry{}
	for rows.Next() {
		var e KeyBackupEntry
		var p int64
		if err := rows.Scan(&p, &e.Backup); err != nil {
			return nil, false, wrap(err)
		}
		e.Pos = gen.CommonPos1(p)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, wrap(err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// DeleteAllOfUser は抹消で本人の鍵のバックアップを消す（hook OnUserErased）。
func (b *KeyBackup) DeleteAllOfUser(ctx context.Context, tx *store.Tx, userId uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM key_backup WHERE user_id = $1`, userId)
	return wrap(err)
}
