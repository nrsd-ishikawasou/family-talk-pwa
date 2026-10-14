// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// RoomLock はトークの行の錠（B-029。第1章 10.5。同じトークへの同時の送信を直列に）。
type RoomLock struct{}

// Lock は rooms の行を FOR UPDATE する。行が無ければ ROOM-001。rooms の表は srv-rooms の移行が作る。
func (RoomLock) Lock(ctx context.Context, tx *Tx, roomId uuid.UUID) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM rooms WHERE id = $1 FOR UPDATE`, roomId).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fault.New(gen.ROOM001, nil)
	}
	if err != nil {
		return fault.Wrap(err)
	}
	return nil
}
