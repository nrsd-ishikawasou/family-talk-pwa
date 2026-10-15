-- SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
-- SPDX-License-Identifier: Apache-2.0
-- srv-identity の最初の移行：家族、カードの持ち主、構成員、保護者、招待、パスキー、端末、セッション（ブロックのページ srv-identity のデータ設計。
-- 基本設計書 第3章、第2章 4.1、4.4、第12章 6.2、11）。外部キーは同じブロックの表の間だけに張る（全体実装設計書 4.2）。
-- 秘密の値（トークン、セッション、端末のコード）はハッシュだけを置く（I-6）。

-- 家族（1 行）。
CREATE TABLE families (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    icon_media_id uuid,
    color text NOT NULL,
    installed_at timestamptz NOT NULL,
    recovery_pub jsonb,
    -- 家族回復鍵の公開部の版（置くたびに 1 増える。第12章 5.4 (d)）。
    recovery_key_version integer NOT NULL DEFAULT 0 CHECK (recovery_key_version >= 0),
    card_printed_at timestamptz,
    card_printed_by uuid,
    CHECK ((card_printed_at IS NULL) = (card_printed_by IS NULL))
);
CREATE UNIQUE INDEX families_one_row ON families ((true));

-- 構成員。webauthn_handle は WebAuthn の user handle（乱数 32 バイト。UUIDv7 は時刻を含むので使わない）。
CREATE TABLE users (
    id uuid PRIMARY KEY,
    webauthn_handle bytea NOT NULL UNIQUE CHECK (length(webauthn_handle) = 32),
    display_name text NOT NULL,
    role text NOT NULL CHECK (role IN ('member', 'admin')),
    state text NOT NULL CHECK (state IN ('normal', 'suspended', 'erase_pending', 'deleted', 'erased')),
    is_child boolean NOT NULL DEFAULT false,
    child_declared boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    dek_id uuid NOT NULL,
    deleted_at timestamptz,
    erase_at timestamptz,
    disclosure_ack_version text CHECK (disclosure_ack_version ~ '^[0-9a-f]{16}$')
);

-- card_printed_by はカード 1 の持ち主（印刷した管理者。第12章 5.4）。users より前に作るので外部キーは下で張る。
ALTER TABLE families ADD CONSTRAINT families_card_printed_by_fk FOREIGN KEY (card_printed_by) REFERENCES users (id);

-- 家族回復鍵のカード 2 と 3 の持ち主（第12章 5.4、第3章 3.1 の 6）。作り直しで空に。同じ人が 2 枚を持つことの扱いは第12章 5.4。
CREATE TABLE recovery_cards (
    no integer PRIMARY KEY CHECK (no IN (2, 3)),
    holder_user_id uuid NOT NULL REFERENCES users (id),
    held_at timestamptz NOT NULL
);

-- 構成員ごとの DEK（fdek/1。KEK で包む。第12章 6.2）。
CREATE TABLE user_dek (
    user_id uuid PRIMARY KEY REFERENCES users (id),
    wrapped bytea NOT NULL
);

-- 構成員のエンベロープの欄（fenv/1。本人の DEK）。
CREATE TABLE user_private (
    user_id uuid PRIMARY KEY REFERENCES users (id),
    birthday bytea,
    aliases bytea NOT NULL,
    muted_users bytea NOT NULL,
    settings bytea NOT NULL
);

-- 保護者と子の組（第3章 9）。
CREATE TABLE guardians (
    child_id uuid NOT NULL REFERENCES users (id),
    guardian_id uuid NOT NULL REFERENCES users (id),
    set_by uuid NOT NULL REFERENCES users (id),
    set_at timestamptz NOT NULL,
    PRIMARY KEY (child_id, guardian_id),
    CHECK (child_id <> guardian_id)
);
CREATE INDEX guardians_guardian ON guardians (guardian_id);

-- 子の制限（第3章 9.2 の 4 行。ft.settings.guardian/1）。子ごとに 1 行。
CREATE TABLE child_controls (
    child_id uuid PRIMARY KEY REFERENCES users (id),
    settings jsonb NOT NULL,
    updated_by uuid NOT NULL REFERENCES users (id),
    updated_at timestamptz NOT NULL,
    -- 申告だけの子に保護者へ年齢を問うた時刻（第3章 9.1 の 2 年ごと）。
    age_asked_at timestamptz
);

-- 招待（第3章 3.2）。display_hint は発行者の DEK のエンベロープ。
CREATE TABLE invites (
    id uuid PRIMARY KEY,
    token_sha256 bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    uses_left integer NOT NULL CHECK (uses_left >= 0),
    role text NOT NULL CHECK (role IN ('member', 'admin')),
    display_hint bytea,
    child jsonb,
    issuer uuid NOT NULL REFERENCES users (id),
    used_by uuid REFERENCES users (id),
    used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL
);
CREATE INDEX invites_issuer ON invites (issuer, created_at);
CREATE INDEX invites_created ON invites (created_at);

-- 管理者の引き継ぎの求め（第3章 8.4。7 日）。
CREATE TABLE admin_transfers (
    id uuid PRIMARY KEY,
    from_user_id uuid NOT NULL REFERENCES users (id),
    to_user_id uuid NOT NULL REFERENCES users (id),
    state text NOT NULL CHECK (state IN ('open', 'accepted', 'cancelled', 'expired')),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX admin_transfers_one_open ON admin_transfers ((true)) WHERE state = 'open';

-- 保護者の承認の求め（第3章 8.3、9.2）。device の対象は 2 つのどちらか 1 つ：QR の承認なら端末のコードのハッシュ
-- （target_sha256。device_codes の主キー）、鍵の承認なら端末の識別子（target_device_id。第3章 4.4）。
-- target_device_id の外部キーは devices の後で張る。
CREATE TABLE guardian_approvals (
    id uuid PRIMARY KEY,
    child_id uuid NOT NULL REFERENCES users (id),
    kind text NOT NULL CHECK (kind IN ('erase', 'device')),
    target_sha256 bytea,
    target_device_id uuid,
    state text NOT NULL CHECK (state IN ('open', 'approved', 'denied', 'expired')),
    approved_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK (CASE kind
        WHEN 'device' THEN (target_sha256 IS NULL) <> (target_device_id IS NULL)
        ELSE target_sha256 IS NULL AND target_device_id IS NULL END)
);
CREATE INDEX guardian_approvals_child ON guardian_approvals (child_id, state);

-- 初回の管理者のリンク（第3章 3.1。1 回限り・7 日）。
CREATE TABLE setup_links (
    token_sha256 bytea PRIMARY KEY,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at timestamptz
);

-- パスキー（第3章 4.1）。record は資格情報の記録の全体（go-webauthn の Credential の JSON。PRF の結果は含まない）。
CREATE TABLE passkeys (
    cred_id bytea PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id),
    name text,
    record jsonb NOT NULL,
    prf boolean NOT NULL,
    created_at timestamptz NOT NULL,
    last_used_at timestamptz
);
CREATE INDEX passkeys_user ON passkeys (user_id);

-- WebAuthn の challenge（5 分。1 回で消す）。session は go-webauthn の SessionData の JSON（user handle を含む）。
CREATE TABLE login_challenges (
    id uuid PRIMARY KEY,
    challenge bytea NOT NULL UNIQUE CHECK (length(challenge) = 32),
    kind text NOT NULL CHECK (kind IN ('register', 'login', 'setup', 'invite', 'reset')),
    user_id uuid REFERENCES users (id),
    token_sha256 bytea,
    session jsonb NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK ((kind IN ('setup', 'invite', 'reset')) = (token_sha256 IS NOT NULL))
);
CREATE INDEX login_challenges_expires ON login_challenges (expires_at);

-- 端末（第3章 5.1）。承認の正は state（全体実装設計書 3.1）。name と last_ip は本人の DEK のエンベロープ。
CREATE TABLE devices (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id),
    name bytea NOT NULL,
    kind text NOT NULL CHECK (kind IN ('iphone', 'android', 'pc')),
    state text NOT NULL CHECK (state IN ('unapproved', 'approved', 'dormant', 'revoked', 'expired')),
    last_seen_at timestamptz NOT NULL,
    last_ip bytea,
    created_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revoked_by uuid REFERENCES users (id),
    -- 保護者が承認した端末（QR の承認か鍵の承認で。子の端末の鍵の承認に 2 度は求めない。第3章 4.4）。
    guardian_approved_at timestamptz,
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE INDEX devices_user ON devices (user_id, state);
CREATE INDEX devices_unapproved ON devices (created_at) WHERE state = 'unapproved';
CREATE INDEX devices_last_seen ON devices (last_seen_at) WHERE state = 'approved';

ALTER TABLE guardian_approvals ADD CONSTRAINT guardian_approvals_target_device_fk FOREIGN KEY (target_device_id) REFERENCES devices (id);

-- セッション（第2章 4.1）。revoked_reason はエラーコード表の AUTH-002 の params.reason の 7 つ。
CREATE TABLE sessions (
    id uuid PRIMARY KEY,
    token_sha256 bytea NOT NULL UNIQUE,
    user_id uuid NOT NULL REFERENCES users (id),
    device_id uuid NOT NULL REFERENCES devices (id),
    created_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    idle_expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revoked_reason text CHECK (revoked_reason IN
        ('logout', 'device_revoked', 'suspended', 'deleted', 'erased', 'key_rotation', 'expired')),
    reauth_at timestamptz,
    CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL))
);
CREATE INDEX sessions_device ON sessions (device_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_user ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_revoked ON sessions (revoked_at);

-- 短いトークン（ws は 60 秒の使い切り、shell は 24 時間。第2章 4.4）。
CREATE TABLE short_tokens (
    token_sha256 bytea PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('ws', 'shell')),
    session_id uuid NOT NULL REFERENCES sessions (id),
    expires_at timestamptz NOT NULL,
    used_at timestamptz
);
CREATE INDEX short_tokens_expires ON short_tokens (expires_at);

-- QR による端末の追加（第3章 4.4。RFC 8628 に倣う。10 分）。ip は承認の画面に示す生の値（10 分で消える）。
CREATE TABLE device_codes (
    device_code_sha256 bytea PRIMARY KEY,
    user_code text NOT NULL CHECK (user_code ~ '^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$'),
    kind text NOT NULL CHECK (kind IN ('iphone', 'android', 'pc')),
    name text NOT NULL,
    sig_pub jsonb NOT NULL,
    kem_pub jsonb NOT NULL,
    ip inet,
    expires_at timestamptz NOT NULL,
    state text NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired')),
    -- 承認した構成員（承認の後の最初の poll がこの構成員の端末とセッションを作る）。
    approved_user_id uuid REFERENCES users (id),
    -- 保護者の承認を経た求め（承認の後の最初の poll が登録する端末を「保護者が承認した端末」にする。第3章 4.4）。
    guardian_approved boolean NOT NULL DEFAULT false,
    approved_session_id uuid REFERENCES sessions (id),
    last_poll_at timestamptz,
    created_at timestamptz NOT NULL,
    CHECK (state <> 'approved' OR approved_user_id IS NOT NULL)
);
CREATE UNIQUE INDEX device_codes_user_code_pending ON device_codes (user_code) WHERE state = 'pending';
CREATE INDEX device_codes_expires ON device_codes (expires_at);

-- パスキーの作り直しのリンク（第3章 4.6。24 時間、1 回）。
CREATE TABLE reset_links (
    token_sha256 bytea PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id),
    admin_id uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at timestamptz
);

-- 生の IP（28 日。第12章 11）。ip は端末の持ち主の DEK のエンベロープ（fenv/1。全体実装設計書 4.2、第12章 6.2）。
CREATE TABLE ip_log (
    device_id uuid NOT NULL REFERENCES devices (id),
    ip bytea NOT NULL,
    at timestamptz NOT NULL
);
CREATE INDEX ip_log_device ON ip_log (device_id, at);
CREATE INDEX ip_log_at ON ip_log (at);
