// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
)

// 仕事の状態。
const (
	JobQueued  = "queued"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// maxAttempts は失敗を数える上限（全体実装設計書 2.3：1・4・16 分で 3 回）。
const maxAttempts = 3

// ErrJobLost は取り出した仕事が取り直されたこと（claim_id が一致しない）。srv-worker は処理を止める。
var ErrJobLost = errors.New("store: 仕事が取り直された")

// Job は jobs の 1 行。
type Job struct {
	Id          uuid.UUID
	Kind        string
	Payload     json.RawMessage
	RunAt       time.Time
	Attempts    int
	LockedUntil *time.Time
	// ClaimId は取り出しの印。Complete、Fail、Heartbeat は行と一致する時だけ効く。
	ClaimId uuid.UUID
	State   string
}

// JobStats は /health のための仕事の数と最後の時刻。
type JobStats struct {
	Queued     int
	Running    int
	Failed     int
	LastDoneAt *time.Time
}

// JobHandler は仕事の種類ごとの処理（定義は本ブロック、実装は srv-worker。冪等。I-10）。
type JobHandler interface {
	Kind() string
	Run(ctx context.Context, job Job) error
}

// Jobs は仕事の列（B-024。FOR UPDATE SKIP LOCKED）。
type Jobs struct {
	pool *Pool
	ids  *ids.Ids
	clk  clock.Clock
}

// NewJobs は Jobs を作る。
func NewJobs(pool *Pool, ids *ids.Ids, clk clock.Clock) *Jobs {
	return &Jobs{pool: pool, ids: ids, clk: clk}
}

// Enqueue は仕事を積む。dedupeKey があり queued の同じ鍵があれば積まずに既にある id を返す。
func (j *Jobs) Enqueue(ctx context.Context, tx *Tx, kind string, payload any, runAt time.Time, dedupeKey string) (uuid.UUID, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fault.Wrap(err)
	}
	var dk any
	if dedupeKey != "" {
		dk = dedupeKey
	}
	id := j.ids.NewV7()
	var got uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO jobs (id, kind, payload, run_at, attempts, state, dedupe_key, created_at)
VALUES ($1, $2, $3, $4, 0, 'queued', $5, $6)
ON CONFLICT (kind, dedupe_key) WHERE state = 'queued' AND dedupe_key IS NOT NULL DO NOTHING
RETURNING id`, id, kind, body, runAt.UTC(), dk, j.clk.Now()).Scan(&got)
	if err == nil {
		return got, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fault.Wrap(err)
	}
	err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE kind = $1 AND dedupe_key = $2 AND state = 'queued'`, kind, dedupeKey).Scan(&got)
	if err != nil {
		return uuid.Nil, fault.Wrap(err)
	}
	return got, nil
}

// scanJob は 1 行を Job に読む。
func scanJob(row pgx.Row) (*Job, error) {
	var jb Job
	var payload []byte
	var claim *uuid.UUID
	if err := row.Scan(&jb.Id, &jb.Kind, &payload, &jb.RunAt, &jb.Attempts, &jb.LockedUntil, &claim, &jb.State); err != nil {
		return nil, err
	}
	jb.Payload = payload
	jb.RunAt = jb.RunAt.UTC()
	if jb.LockedUntil != nil {
		t := jb.LockedUntil.UTC()
		jb.LockedUntil = &t
	}
	if claim != nil {
		jb.ClaimId = *claim
	}
	return &jb, nil
}

// Claim は kinds の中で run_at が now 以前の queued を 1 つ取り、running にして locked_until = now + lockFor と
// 新しい claim_id を入れる。無ければ nil。
func (j *Jobs) Claim(ctx context.Context, kinds []string, now time.Time, lockFor time.Duration) (*Job, error) {
	row := j.pool.QueryRow(ctx, `UPDATE jobs SET state = 'running', locked_until = $3, claim_id = $4
WHERE id = (SELECT id FROM jobs WHERE state = 'queued' AND kind = ANY($1) AND run_at <= $2
            ORDER BY run_at LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING id, kind, payload, run_at, attempts, locked_until, claim_id, state`,
		kinds, now.UTC(), now.Add(lockFor).UTC(), j.ids.NewV7())
	jb, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return jb, nil
}

// Complete は done にする（job の ClaimId が行と一致する時だけ）。周期の仕事は同じトランザクションで次を Enqueue する。
func (j *Jobs) Complete(ctx context.Context, tx *Tx, job Job) error {
	tag, err := tx.Exec(ctx, `UPDATE jobs SET state = 'done', locked_until = NULL, finished_at = $3
WHERE id = $1 AND claim_id = $2 AND state = 'running'`, job.Id, job.ClaimId, j.clk.Now())
	if err != nil {
		return fault.Wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobLost
	}
	return nil
}

// Fail は attempts を増やし、3 回で failed、それまでは nextRunAt に queued へ戻す（ClaimId が一致する時だけ）。
// reason はエラーの番号か短い理由（秘密を入れない）。
func (j *Jobs) Fail(ctx context.Context, job Job, reason string, nextRunAt time.Time) error {
	tag, err := j.pool.Exec(ctx, `UPDATE jobs SET attempts = attempts + 1, last_error = $3,
  state = CASE WHEN attempts + 1 >= $5 THEN 'failed' ELSE 'queued' END,
  finished_at = CASE WHEN attempts + 1 >= $5 THEN $6::timestamptz ELSE NULL END,
  run_at = $4, locked_until = NULL, claim_id = NULL
WHERE id = $1 AND claim_id = $2 AND state = 'running'`, job.Id, job.ClaimId, reason, nextRunAt.UTC(), maxAttempts, j.clk.Now())
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobLost
	}
	return nil
}

// Heartbeat は走っている仕事の locked_until を now + lockFor に延ばす。ClaimId が一致しなければ ErrJobLost（取り直された）。
func (j *Jobs) Heartbeat(ctx context.Context, job Job, now time.Time, lockFor time.Duration) error {
	tag, err := j.pool.Exec(ctx, `UPDATE jobs SET locked_until = $3 WHERE id = $1 AND claim_id = $2 AND state = 'running'`,
		job.Id, job.ClaimId, now.Add(lockFor).UTC())
	if err != nil {
		return unavailable(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobLost
	}
	return nil
}

// Recover は locked_until が now を過ぎた running を queued に戻す（異常終了の後）。戻した数を返す。
func (j *Jobs) Recover(ctx context.Context, now time.Time) (int64, error) {
	tag, err := j.pool.Exec(ctx, `UPDATE jobs SET state = 'queued', locked_until = NULL, claim_id = NULL
WHERE state = 'running' AND locked_until < $1`, now.UTC())
	if err != nil {
		return 0, unavailable(err)
	}
	return tag.RowsAffected(), nil
}

// Prune は done と failed で finished_at が olderThan より古い行を 10,000 行ずつ消す（30 日。srv-worker が呼ぶ）。
func (j *Jobs) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	return deleteInBatches(ctx, j.pool, `DELETE FROM jobs WHERE ctid IN (SELECT ctid FROM jobs
WHERE state IN ('done', 'failed') AND finished_at < $1 LIMIT 10000)`, olderThan.UTC())
}

// Stats は仕事の数と最後の完了の時刻。
func (j *Jobs) Stats(ctx context.Context) (JobStats, error) {
	var s JobStats
	err := j.pool.QueryRow(ctx, `SELECT
  count(*) FILTER (WHERE state = 'queued'),
  count(*) FILTER (WHERE state = 'running'),
  count(*) FILTER (WHERE state = 'failed'),
  max(finished_at) FILTER (WHERE state = 'done')
FROM jobs`).Scan(&s.Queued, &s.Running, &s.Failed, &s.LastDoneAt)
	if err != nil {
		return JobStats{}, unavailable(err)
	}
	if s.LastDoneAt != nil {
		t := s.LastDoneAt.UTC()
		s.LastDoneAt = &t
	}
	return s, nil
}
