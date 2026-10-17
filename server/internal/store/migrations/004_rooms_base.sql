-- SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
-- SPDX-License-Identifier: Apache-2.0
-- srv-rooms の最初の移行：トーク、構成員、自分の設定、イベント、関係のコピー、既読、予約送信
-- （ブロックのページ srv-rooms のデータ設計。基本設計書 第4章 4.1、4.2、5.1、5.4、5.8、第2章 5.5、12、第12章 9）。
-- 外部キーは同じブロックの表の間だけに張る（全体実装設計書 4.2）。構成員と端末の識別子は他のブロックの表で、
-- 確かめるのは書く操作（全体実装設計書 4.3）。本文の平文を置く列は無い（I-1）。

-- トーク。kind は dm（1 対 1）、group、self（自分用メモ）、family（家族全員）。creator_id はグループだけで、作成者の退出・外し・
-- 削除で外れる。state は normal、empty（構成員が残らない。empty_since から 30 日で削除）、deleted（deleted_at から 7 日で行ごと消す）。
-- dm_key は 1 対 1 の 2 人の識別子を並べた一意の鍵（ROOM-004）。家族全員のトークは 1 つ。
CREATE TABLE rooms (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('dm', 'group', 'self', 'family')),
    creator_id uuid CHECK (creator_id IS NULL OR kind = 'group'),
    state text NOT NULL CHECK (state IN ('normal', 'empty', 'deleted')),
    empty_since timestamptz CHECK (state <> 'empty' OR empty_since IS NOT NULL),
    last_event_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    deleted_at timestamptz,
    dm_key text UNIQUE,
    CHECK ((state = 'deleted') = (deleted_at IS NOT NULL)),
    CHECK ((kind = 'dm') = (dm_key IS NOT NULL))
);
CREATE UNIQUE INDEX rooms_family ON rooms (kind) WHERE kind = 'family';
CREATE INDEX rooms_empty ON rooms (empty_since) WHERE state = 'empty';
CREATE INDEX rooms_deleted ON rooms (deleted_at) WHERE state = 'deleted';

-- 構成員。state は pending_add（追加待ち。Commit で active）、active、pending_remove（削除待ち。その人の leaf が無くなれば
-- removed か left）、removed、left（本人の退出）。by は最後に状態を変えた人（家族全員の参加とサーバーの処理は NULL）。
-- api の Member.state の member は active に当たる（第4章 4.1）。
CREATE TABLE room_members (
    room_id uuid NOT NULL REFERENCES rooms (id),
    user_id uuid NOT NULL,
    state text NOT NULL CHECK (state IN ('pending_add', 'active', 'pending_remove', 'removed', 'left')),
    since timestamptz NOT NULL,
    by uuid,
    PRIMARY KEY (room_id, user_id)
);
CREATE INDEX room_members_user ON room_members (user_id, state);
CREATE INDEX room_members_pending ON room_members (state, since) WHERE state IN ('pending_add', 'pending_remove');

-- 自分の設定（ft.settings.room_me/1。行が無ければ config.yaml の room_me.* の既定値）。端末から消した位置は端末が持つ（第4章 4.2）。
CREATE TABLE room_me (
    user_id uuid NOT NULL,
    room_id uuid NOT NULL REFERENCES rooms (id),
    mute_until timestamptz,
    pinned boolean NOT NULL,
    favorite boolean NOT NULL,
    hidden boolean NOT NULL,
    background text,
    PRIMARY KEY (user_id, room_id)
);
CREATE INDEX room_me_room ON room_me (room_id);

-- イベント（不変。取消は削除済みの印を書き、行は消さない。I-2）。ft.event.mls/1 は暗号文と平文の欄（mentions、relates_to、
-- attachments、schema_inner、expires_at）を plain に持ち、削除済みの印でなければ暗号文がある。他の種類（system、call、
-- redaction）は平文だけ。redacted_by は管理者・作成者・送信者（空のグループの削除の gc は NULL）。silent は通知なし送信。
-- scheduled_at は予約送信から来たイベントだけが持つ予約の時刻（ts は送った時刻。第4章 5.8）。
CREATE TABLE events (
    id uuid PRIMARY KEY,
    room_id uuid NOT NULL REFERENCES rooms (id),
    ts timestamptz NOT NULL,
    schema text NOT NULL CHECK (schema IN ('ft.event.mls/1', 'ft.event.system/1', 'ft.event.call/1', 'ft.event.redaction/1')),
    sender_id uuid,
    sender_device_id uuid,
    epoch bigint CHECK (epoch >= 0),
    ciphertext bytea,
    plain jsonb,
    redacted_at timestamptz,
    redacted_by uuid CHECK (redacted_by IS NULL OR redacted_at IS NOT NULL),
    redact_reason text CHECK (redact_reason IS NULL OR redacted_at IS NOT NULL),
    transaction_id uuid,
    silent boolean NOT NULL,
    scheduled_at timestamptz CHECK (scheduled_at IS NULL OR schema = 'ft.event.mls/1'),
    CHECK (schema <> 'ft.event.mls/1' OR (sender_id IS NOT NULL AND sender_device_id IS NOT NULL AND epoch IS NOT NULL
        AND plain IS NOT NULL AND (ciphertext IS NOT NULL OR redacted_at IS NOT NULL))),
    CHECK (schema = 'ft.event.mls/1' OR (ciphertext IS NULL AND epoch IS NULL AND plain IS NOT NULL))
);
CREATE INDEX events_room_ts ON events (room_id, ts);
CREATE INDEX events_room_id ON events (room_id, id);
CREATE INDEX events_sender ON events (sender_id) WHERE sender_id IS NOT NULL;
CREATE INDEX events_redacted ON events (redacted_at) WHERE redacted_at IS NOT NULL;

-- relates_to のコピー（/relations の読み出し用。第4章 3.2）。対象は同じトークの識別子で、まだ無くてもよい（相手が先に届く順序を
-- 許す。全体実装設計書 4.2）ので対象には外部キーを張らない。
CREATE TABLE event_relations (
    event_id uuid PRIMARY KEY REFERENCES events (id),
    room_id uuid NOT NULL REFERENCES rooms (id),
    rel_type text NOT NULL CHECK (rel_type IN ('replace', 'annotation', 'reply', 'thread')),
    target_event_id uuid NOT NULL,
    key text
);
CREATE INDEX event_relations_target ON event_relations (target_event_id, rel_type);

-- 既読の 3 種（公開の既読、私的な既読、読み終えた位置。第4章 5.4）。単調に進める。
CREATE TABLE receipts (
    user_id uuid NOT NULL,
    room_id uuid NOT NULL REFERENCES rooms (id),
    read_event_id uuid REFERENCES events (id),
    read_private_event_id uuid REFERENCES events (id),
    fully_read_event_id uuid REFERENCES events (id),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, room_id)
);
CREATE INDEX receipts_room ON receipts (room_id);

-- 予約送信（第4章 5.8。body は ft.event.mls/1 の暗号文を含む要求の本文）。state は pending、sent、cancelled。
-- job_id は srv-store の jobs の scheduled_send の仕事。
CREATE TABLE scheduled_events (
    id uuid PRIMARY KEY,
    room_id uuid NOT NULL REFERENCES rooms (id),
    user_id uuid NOT NULL,
    device_id uuid NOT NULL,
    send_at timestamptz NOT NULL,
    body jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('pending', 'sent', 'cancelled')),
    job_id uuid,
    created_at timestamptz NOT NULL
);
CREATE INDEX scheduled_events_mine ON scheduled_events (room_id, user_id, state);
CREATE INDEX scheduled_events_due ON scheduled_events (send_at) WHERE state = 'pending';

-- 一括の取消の送り残し（第12章 9、全体実装設計書 I-16）。削除で消す・抹消の hook が、削除済みの印にした本人のイベントごとに
-- 1 行入れる。srv-worker の仕事 redaction_fanout が取消のイベント（ft.event.redaction/1）を 1,000 件ずつ出して行を消す。
-- 仕事が終われば空。トークの行の物理削除（Gc.PurgeDeletedRooms）で残りも消す。
CREATE TABLE redaction_backlog (
    event_id uuid PRIMARY KEY REFERENCES events (id),
    room_id uuid NOT NULL REFERENCES rooms (id),
    user_id uuid NOT NULL,
    queued_at timestamptz NOT NULL
);
CREATE INDEX redaction_backlog_user ON redaction_backlog (user_id, room_id, event_id);
