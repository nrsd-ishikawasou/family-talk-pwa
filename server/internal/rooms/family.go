// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"

	"github.com/gofrs/uuid/v5"

	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// FamilyRoom は家族全員のトーク（B-107。第4章 6）。
type FamilyRoom struct {
	d        *Deps
	timeline *Timeline
}

// Id は家族全員のトークの識別子（設置の前は ROOM-001）。q は ctx のトランザクションか Pool（hook の中は呼ぶ側のトランザクション）。
func (f *FamilyRoom) Id(ctx context.Context, q store.Querier) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM rooms WHERE kind = 'family'`).Scan(&id)
	if noRows(err) {
		return uuid.Nil, ev(cgen.ROOM001)
	}
	return id, wrap(err)
}

// Flow は参加、退出、停止、再開、削除、誕生日、管理者の操作、容量・証明書・バックアップ・更新の知らせ（第4章 6 の表）を
// 家族全員のトークの SystemEvent にする（active の構成員へ）。
func (f *FamilyRoom) Flow(ctx context.Context, tx *store.Tx, kind string, params map[string]any) error {
	id, err := f.Id(ctx, tx)
	if err != nil {
		return err
	}
	_, err = f.timeline.SystemEvent(ctx, tx, id, kind, params)
	return err
}
