// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// Stored は入れた応答。
type Stored struct {
	RequestHash [32]byte
	Status      int
	Body        []byte
}

// Idempotency は冪等の記録（B-026。第2章 11。Idempotency-Key = transaction id。30 日）。
type Idempotency struct {
	pool *Pool
	clk  clock.Clock
}

// NewIdempotency は Idempotency を作る。
func NewIdempotency(pool *Pool, clk clock.Clock) *Idempotency {
	return &Idempotency{pool: pool, clk: clk}
}

// errNoResponse は応答の無いまま残った行（Finish を呼ばずにコミットした。プログラムの誤り）。
var errNoResponse = errors.New("store: 冪等の記録に応答が無い")

// Begin は処理と同じトランザクションの頭で行を先に入れる。入れられたら nil を返し、処理を続ける。
// 同じ鍵が既にあれば入れた応答を返す（同時の 2 つ目は一意索引で 1 つ目のコミットを待つ）。本文のハッシュが違えば NET-008。
func (i *Idempotency) Begin(ctx context.Context, tx *Tx, userId uuid.UUID, key string, requestHash [32]byte) (*Stored, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO idempotency (user_id, key, request_sha256, created_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, key) DO NOTHING`, userId, key, requestHash[:], i.clk.Now())
	if err != nil {
		return nil, fault.Wrap(err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var hash []byte
	var status *int
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT request_sha256, status, body FROM idempotency WHERE user_id = $1 AND key = $2`,
		userId, key).Scan(&hash, &status, &body); err != nil {
		return nil, fault.Wrap(err)
	}
	var st Stored
	copy(st.RequestHash[:], hash)
	if st.RequestHash != requestHash {
		return nil, fault.New(gen.NET008, nil)
	}
	if status == nil {
		return nil, fault.Wrap(errNoResponse)
	}
	st.Status = *status
	st.Body = body
	return &st, nil
}

// Finish は処理の応答を同じトランザクションで入れる。
func (i *Idempotency) Finish(ctx context.Context, tx *Tx, userId uuid.UUID, key string, status int, body []byte) error {
	tag, err := tx.Exec(ctx, `UPDATE idempotency SET status = $3, body = $4 WHERE user_id = $1 AND key = $2`, userId, key, status, body)
	if err != nil {
		return fault.Wrap(err)
	}
	if tag.RowsAffected() != 1 {
		return fault.Wrap(errors.New("store: Begin の無い Finish"))
	}
	return nil
}

// Prune は olderThan より古い記録を 10,000 行ずつ消す。
func (i *Idempotency) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	return deleteInBatches(ctx, i.pool, `DELETE FROM idempotency WHERE ctid IN (SELECT ctid FROM idempotency WHERE created_at < $1 LIMIT 10000)`, olderThan.UTC())
}
