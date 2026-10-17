// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"time"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// Gc は本ブロックの記録の掃除（B-110。srv-worker の gc。第12章 9、第4章 4.1）。
type Gc struct {
	d     *Deps
	rooms *Rooms
}

// DeleteEmptyGroups は空（empty_since）になって room.empty_group_delete_after（30 日。olderThan は srv-worker が求める）のグループを、
// 1 つずつのトランザクションで RoomLock の後に空のままかを確かめ、Rooms.Delete と同じ処理（by 無し）で削除済みにする。戻りは消した数。
func (g *Gc) DeleteEmptyGroups(ctx context.Context, olderThan time.Time) (int, error) {
	ids, err := scanUuids(ctx, g.d.Pool, `SELECT id FROM rooms WHERE state = 'empty' AND empty_since <= $1 ORDER BY id`, olderThan)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := g.d.Pool.InTx(ctx, func(tx *store.Tx) error {
			room, ok, err := g.d.lockRoomAny(ctx, tx, id)
			if err != nil || !ok || room.State != RoomEmpty || room.EmptySince == nil || room.EmptySince.After(olderThan) {
				return err
			}
			n++
			return g.rooms.drop(ctx, tx, room, nil)
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// PurgeDeletedRooms は deleted_at から 7 日（config retention.deleted_room_grace。olderThan は srv-worker が求める）の後に、1 つの
// トークを 1 つのトランザクションで、RoomLock の後に（FanoutRedactions と錠の順を揃える）そのトークの本ブロックの表の行を参照する側
// から消す（redaction_backlog、event_relations、receipts、events、room_me、scheduled_events、room_members、rooms）。srv-keys の記録は
// Rooms.Delete の Groups.Drop で既に無い。戻りは消したトークの数。
func (g *Gc) PurgeDeletedRooms(ctx context.Context, olderThan time.Time) (int, error) {
	ids, err := scanUuids(ctx, g.d.Pool, `SELECT id FROM rooms WHERE state = 'deleted' AND deleted_at <= $1 ORDER BY id`, olderThan)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := g.d.Pool.InTx(ctx, func(tx *store.Tx) error {
			room, ok, err := g.d.lockRoomAny(ctx, tx, id)
			if err != nil || !ok || room.State != RoomDeleted {
				return err
			}
			for _, t := range []string{"redaction_backlog", "event_relations", "receipts", "events", "room_me", "scheduled_events", "room_members"} {
				if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE room_id = $1`, id); err != nil {
					return wrap(err)
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM rooms WHERE id = $1`, id); err != nil {
				return wrap(err)
			}
			n++
			return nil
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
