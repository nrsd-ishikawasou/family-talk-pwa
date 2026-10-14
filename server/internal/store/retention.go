// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// identName は表と列の名に許す形（これ以外は SQL に置かない）。
var identName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Retention は期限つきの削除の器（B-027。第1章 8.4）。表と列の名は持ち主が渡す。
type Retention struct {
	pool *Pool
}

// NewRetention は Retention を作る。
func NewRetention(pool *Pool) *Retention {
	return &Retention{pool: pool}
}

// DeleteOlder は table の column が olderThan より古い行を 10,000 行ずつ消し、消した数を返す。
// 名は ^[a-z][a-z0-9_]*$ だけを受け、引用して SQL に置く（合わない名はプログラムの誤りなので panic）。
func (r *Retention) DeleteOlder(ctx context.Context, table, column string, olderThan time.Time) (int64, error) {
	if !identName.MatchString(table) || !identName.MatchString(column) {
		panic("store: 表か列の名の形が違う: " + table + "." + column)
	}
	t := pgx.Identifier{table}.Sanitize()
	c := pgx.Identifier{column}.Sanitize()
	return deleteInBatches(ctx, r.pool, "DELETE FROM "+t+" WHERE ctid IN (SELECT ctid FROM "+t+" WHERE "+c+" < $1 LIMIT 10000)", olderThan.UTC())
}
