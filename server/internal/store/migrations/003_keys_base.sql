-- SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
-- SPDX-License-Identifier: Apache-2.0
-- srv-keys の最初の移行：本人の署名鍵、端末の鍵、KeyPackage、鍵バンドルのコピー、回復コードの照合の値、回復の求め、
-- 鍵のバックアップ、端末宛メールボックス、MLS のグループと leaf の記録と追加待ち・削除待ちと Commit
-- （ブロックのページ srv-keys のデータ設計。基本設計書 第12章 5.2〜5.6、12、第2章 5.4、5.5）。
-- 外部キーは同じブロックの表の間だけに張る（全体実装設計書 4.2）。端末と構成員とトークの識別子は他のブロックの表で、
-- 確かめるのは書く操作（全体実装設計書 4.3）。

-- 本人の署名鍵の公開部（fusr/1）。version は鍵バンドルの秘密 S の版（初回 1。交代で 1 増える。第12章 5.4）。
CREATE TABLE user_signing_keys (
    user_id uuid PRIMARY KEY,
    pub jsonb NOT NULL,
    version integer NOT NULL CHECK (version >= 1),
    created_at timestamptz NOT NULL,
    rotated_at timestamptz,
    revoked_at timestamptz
);

-- 端末の鍵の登録（fdev/1）。credential は MLS の basic の credential（35 バイト。第12章 5.3）。
-- user_sig は本人の署名鍵による端末の証（r と s の 64 バイト）。承認の時に置く。
CREATE TABLE device_keys (
    device_id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    sig_pub jsonb NOT NULL,
    kem_pub jsonb NOT NULL,
    credential bytea NOT NULL CHECK (length(credential) = 35),
    user_sig bytea CHECK (length(user_sig) = 64),
    created_at timestamptz NOT NULL,
    approved_at timestamptz,
    CHECK ((user_sig IS NULL) = (approved_at IS NULL))
);
CREATE INDEX device_keys_user ON device_keys (user_id);

-- KeyPackage（MLSMessage の符号化）。ref は KeyPackageRef（RFC 9420 5.2。SHA-256 の 32 バイト）。
-- 端末ごとに使い切り 20 と最後の手段 1。期限は置いた時から 90 日（第12章 5.2）。
CREATE TABLE keypackages (
    ref bytea PRIMARY KEY CHECK (length(ref) = 32),
    device_id uuid NOT NULL,
    package bytea NOT NULL,
    last_resort boolean NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX keypackages_device ON keypackages (device_id, last_resort, created_at);
CREATE UNIQUE INDEX keypackages_one_last_resort ON keypackages (device_id) WHERE last_resort;
CREATE INDEX keypackages_expires ON keypackages (expires_at);

-- 鍵バンドルのコピー（fkb/1）。target_id は kind device なら端末の識別子、share なら片を持つ構成員の端末の識別子、
-- recovery-code と family なら self、prf なら資格情報の識別子の base64url（第12章 12）。
-- key_version は作った時の S の版（第12章 5.4）。
CREATE TABLE keybundles (
    user_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('device', 'recovery-code', 'share', 'family', 'prf')),
    target_id text NOT NULL,
    key_version integer NOT NULL CHECK (key_version >= 1),
    body jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, kind, target_id),
    CHECK ((kind IN ('recovery-code', 'family')) = (target_id = 'self'))
);
CREATE INDEX keybundles_target ON keybundles (target_id);

-- 回復コードの照合の値（第12章 5.4 (b)。Argon2id の出力 A から HKDF で導いた 32 バイト。包む鍵は導けない）。
CREATE TABLE recovery_codes (
    user_id uuid PRIMARY KEY,
    verifier bytea NOT NULL CHECK (length(verifier) = 32),
    params jsonb NOT NULL,
    issued_at timestamptz NOT NULL
);

-- 回復の求め（家族に頼む。第12章 5.4 (c)。24 時間）。本人の open の求めは 1 つ。
CREATE TABLE recovery_requests (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    device_id uuid NOT NULL,
    state text NOT NULL CHECK (state IN ('open', 'approved', 'expired')),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX recovery_requests_one_open ON recovery_requests (user_id) WHERE state = 'open';
CREATE INDEX recovery_requests_expires ON recovery_requests (expires_at) WHERE state = 'open';

-- 回復の求めに応じた構成員（片の数は構成員で数える。同じ構成員の 2 台目は数えない）。
CREATE TABLE recovery_approvals (
    request_id uuid NOT NULL REFERENCES recovery_requests (id),
    holder_user_id uuid NOT NULL,
    holder_device_id uuid NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (request_id, holder_user_id)
);

-- 鍵のバックアップ（fkb-history/1。本人ごと、グループと epoch ごとに 1 件。第12章 5.6）。
-- group_id はトークの識別子の 16 バイト（第12章 5.2）。pos は取得の位置（端末の次の since）。
CREATE TABLE key_backup (
    user_id uuid NOT NULL,
    group_id bytea NOT NULL CHECK (length(group_id) = 16),
    epoch bigint NOT NULL CHECK (epoch >= 0),
    key_version integer NOT NULL CHECK (key_version >= 1),
    body jsonb NOT NULL,
    pos bigserial NOT NULL UNIQUE,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, group_id, epoch)
);
CREATE INDEX key_backup_user_pos ON key_backup (user_id, pos);

-- 端末宛メールボックス（第12章 5.2 の種類の表。30 日。1 端末 1,000 件）。sig は welcome だけ空。
CREATE TABLE device_inbox (
    id uuid PRIMARY KEY,
    to_device uuid NOT NULL,
    from_device uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('welcome', 'keybundle', 'key_backup', 'key_request', 'share',
        'call_key_request', 'call_key', 'sticker_remove_request')),
    ciphertext bytea NOT NULL,
    sig bytea CHECK (length(sig) = 64),
    pos bigserial NOT NULL UNIQUE,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK ((kind = 'welcome') = (sig IS NULL))
);
CREATE INDEX device_inbox_to_pos ON device_inbox (to_device, pos);
CREATE INDEX device_inbox_from ON device_inbox (from_device);
CREATE INDEX device_inbox_expires ON device_inbox (expires_at);

-- MLS のグループ（トークごとに 1 つ。group_id はトークの識別子の 16 バイト。第12章 5.2）。
CREATE TABLE mls_groups (
    room_id uuid PRIMARY KEY,
    epoch bigint NOT NULL CHECK (epoch >= 0),
    suite integer NOT NULL CHECK (suite = 2),
    last_commit_at timestamptz NOT NULL
);

-- leaf の記録（サーバーが Commit の Add と Remove から求める。RFC 9420 12.1.1、12.1.3、12.3）。
CREATE TABLE mls_leaves (
    room_id uuid NOT NULL REFERENCES mls_groups (room_id),
    leaf_index integer NOT NULL CHECK (leaf_index >= 0),
    device_id uuid NOT NULL,
    since_epoch bigint NOT NULL CHECK (since_epoch >= 0),
    PRIMARY KEY (room_id, leaf_index),
    UNIQUE (room_id, device_id)
);
CREATE INDEX mls_leaves_device ON mls_leaves (device_id);

-- 端末の追加待ち・削除待ち（第12章 5.5 の 4、5.2 の休眠と取り消し）。Commit の Add と Remove で行を消す。
CREATE TABLE mls_pending (
    room_id uuid NOT NULL REFERENCES mls_groups (room_id),
    device_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('add', 'remove')),
    since timestamptz NOT NULL,
    by uuid,
    PRIMARY KEY (room_id, device_id)
);
CREATE INDEX mls_pending_device ON mls_pending (device_id);
CREATE INDEX mls_pending_since ON mls_pending (kind, since);

-- 受け付けた Commit（MLSMessage の符号化。PublicMessage）。保持はイベントと同じ（永久）。
CREATE TABLE mls_commits (
    room_id uuid NOT NULL REFERENCES mls_groups (room_id),
    epoch bigint NOT NULL CHECK (epoch >= 1),
    commit bytea NOT NULL,
    by_device uuid NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (room_id, epoch)
);
