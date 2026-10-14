-- SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
-- SPDX-License-Identifier: Apache-2.0
-- srv-store の最初の移行：feed、feed_cursor、jobs、idempotency（全体実装設計書 2.3、2.4、4.1。第2章 6.1、11）。

-- 利用者ごとの配信の列。pos は利用者ごとに単調増加で欠番なし（I-3）。
-- ref_id はフレームの id。room_id は room.* の種類だけが持つ。
-- room.event は本体（events）から引くので payload を持たず、他の種類は書く時の payload を持つ。
CREATE TABLE feed (
    user_id uuid NOT NULL,
    pos bigint NOT NULL,
    kind text NOT NULL,
    ref_id uuid NOT NULL,
    room_id uuid,
    payload jsonb,
    ts timestamptz NOT NULL,
    PRIMARY KEY (user_id, pos),
    CONSTRAINT feed_room CHECK ((kind LIKE 'room.%') = (room_id IS NOT NULL)),
    CONSTRAINT feed_payload CHECK ((kind = 'room.event') = (payload IS NULL))
);
CREATE INDEX feed_ts ON feed (ts);

-- 利用者ごとの次の pos。FOR UPDATE で採る。
CREATE TABLE feed_cursor (
    user_id uuid PRIMARY KEY,
    next_pos bigint NOT NULL
);

-- 仕事の列（少なくとも 1 回。I-10）。
CREATE TABLE jobs (
    id uuid PRIMARY KEY,
    kind text NOT NULL,
    payload jsonb NOT NULL,
    run_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    locked_until timestamptz,
    claim_id uuid,
    state text NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed')),
    dedupe_key text,
    last_error text,
    created_at timestamptz NOT NULL,
    finished_at timestamptz
);
CREATE UNIQUE INDEX jobs_dedupe_queued ON jobs (kind, dedupe_key) WHERE state = 'queued' AND dedupe_key IS NOT NULL;
CREATE INDEX jobs_state_run_at ON jobs (state, run_at);
CREATE INDEX jobs_finished ON jobs (state, finished_at);

-- 冪等の記録（Idempotency-Key = transaction id。30 日）。
CREATE TABLE idempotency (
    user_id uuid NOT NULL,
    key text NOT NULL,
    request_sha256 bytea NOT NULL,
    status integer,
    body bytea,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, key)
);
CREATE INDEX idempotency_created ON idempotency (created_at);
