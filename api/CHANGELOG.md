# 境界の変更の記録

境界の文書（api/ の全部）の版ごとの変更を、Keep a Changelog の形で記録する。版は SemVer で、openapi.yaml と asyncapi.yaml の `info.version` と同じ値（基本設計書 第2章 10）。1.0.0 の公開までは 0 系（互換を壊す変更を許す）。

## [未公開]

### 追加

- 0.1.0：基本設計書 第2章 5 と 10 の全 operation（159）と、第2章 6.2 と 6.4 のメッセージ。
- 0.1.0：第2章 9.2 のエラーコード表、9.3 のレート制限、第11章 7.2 の設定の一覧、共有の形（api/schemas）。
- 0.1.0：JS ブリッジのイベント `callHeld`（基本設計書 第9章 4.2。他のアプリの通話による保留と復帰）。
- 0.1.0：`GET /media/config` の応答に `maps_version`（基本設計書 第2章 5.6。端末が地図の今の版を知る）。
- 0.1.0：基本設計書 第2章 5.3〜5.5 の 3 つの経路。
  - `DELETE /me/erase`（抹消の取り消し）
  - `GET /me/keybundle/{kind}/{target}`（規則 `prf_or_approved`）
  - `GET /rooms/{id}/mls/commits`
- 0.1.0：query の引数（基本設計書 第2章 3 の一覧の規則と、第2章 5 の各行の `?名前=`）。それまで 1 つも無かった。
  - 共通：`limit`（1〜100、既定 50）、`before`、`after`、`since`（`components/parameters`）。
  - `GET /rooms/{id}/events` の `types`、relations の `rel_type`。
  - `GET /sync` の `since`・`timeout`、`GET /rooms/{id}/mls/commits` の `since_epoch`。
  - `PUT /me/keybackup/{group_id}/{epoch}` の `rewrap`、`DELETE /admin/users/{id}` の `erase_content`。
- 0.1.0：`POST /admin/family/recovery-key/printed`（家族回復鍵のカードを印刷したことの記録。基本設計書 第3章 3.1 の 6、第2章 5.10）。
- 0.1.0：`MEM-008`（409。明示の版が古い。params.version に今の版）。
- 0.1.0：設定 `server.hosting_provider`（形 D の VPS の事業者の名前。第11章 3.2、第12章 7.1）。

### 変更

- 0.1.0：設定 `keys.kek_rotation`（KEK の 1 年ごとの自動の交代）を `keys.kek_rotation_reminder`（交代を勧める知らせ、2 年）に改めた。KEK は自動では交代しない（基本設計書 第12章 6.2）。
- 0.1.0：設定 `keys.kek_rotation_reminder_repeat`（勧めの知らせを繰り返す間隔、30 日）を足した。
- 0.1.0：属性 `x-readonly: allow`（読み取り専用でも通す書き込み。12 の操作）と `x-deprecation` の定め（基本設計書 第2章 10）。
- 0.1.0：`GET /metrics` の応答の型を `application/openmetrics-text` にした（OpenMetrics の形式。第11章 11）。
- 0.1.0：`GET /health` の応答を第11章 11 の形にした（status に setup と readonly、503 の reason、checks の型）。
- 0.1.0：各経路の `x-errors` に、x- の属性から暗に返る番号（認可、冪等、再認証、レート制限）を足した（基本設計書 第2章 10）。
- 0.1.0：`NET-009`（413。要求の本文が上限を超えた。params.limit）を足した（基本設計書 第2章 3、9.2）。
- 0.1.0：`NET-006` と `NET-009` を全 operation に掛かる番号（scope: global）にした。
- 0.1.0：`SRV-001` の params の reason に `storage`（置き場に書けない）を足した（基本設計書 第1章 7、第2章 9.2）。
- 0.1.0：`GET /internal/auth` の 200 を本文なしにし、ヘッダ `X-User-Id` を載せた（全体実装設計書 2.1 の forward auth）。
- 0.1.0：`GET /manifest.webmanifest` のタグを family に、`POST /internal/sfu/webhook` のタグを calls にした。タグは実装するブロックを決めるので、1 つのタグに 2 つのブロックの操作を混ぜない（経路と認可は変えない）。
- 0.1.0：`GET /sync` の応答に `has_more` を足し、`snapshot` を名前つきの形 `SyncSnapshot` にした（基本設計書 第2章 5.9）。
- 0.1.0：`GET /me/keybackup` の要素を `KeyBackupEntry`（`pos` と `backup`）にした。`fkb-history/1` は pos を持たず、端末が次の `since` を知る手が無かった（基本設計書 第2章 5.4）。
- 0.1.0：初回の管理者の作成と招待の受諾の要求から `avatar` を外した。作成の時点では鍵とトークが無く、写真は後でプロフィールのイベントで送る（基本設計書 第3章 3.1、3.3）。管理者の作成から `recovery_code_written` と `card_printed` も外した（回復コードとカードは作成の後の段）。
- 0.1.0：初回の管理者の作成に `challenge_id`、`device_name`、`platform` を、招待の受諾とパスキーの作り直しに `challenge_id` を足した。
- 0.1.0：登録の options（`POST /auth/passkey/register/options`）の要求に `setup_token` と `reset_token` を足した。`invite_token` と合わせて多くて 1 つ。
- 0.1.0：`POST /auth/passkey/login/verify` の要求に `device_sig` を足した。`device_hint` と組で送る（基本設計書 第12章 5.3）。
- 0.1.0：`DELETE /me/devices/{id}` の再認証を conditional にした（自分の端末の取り消しは要しない。基本設計書 第3章 4.5）。
- 0.1.0：`POST /me/devices/{id}/approve` の認可に `device_self` を足し、再認証を conditional にした。未承認の端末が自分を承認できなかった（自己承認。基本設計書 第12章 5.5 の 2）。
- 0.1.0：`GET /family/disclosure` の応答を、文の鍵から、サーバーが値を埋めた文（`notice`）と外へ送る情報の表（`outbound`）にした。版は 16 進 16 文字の文字列（基本設計書 第12章 7.1）。`POST /me/disclosure-ack` の version も同じ形にし、`Session` に `disclosure_ack_version` を足した。
- 0.1.0：`User` と `/me` の応答・要求から `avatar`、`background`、`bio` を外した。写真、背景、ひとことは家族全員のトークの暗号文で、サーバーは持たない（基本設計書 第3章 6、第2章 5.3）。
- 0.1.0：`POST /me/devices/{id}/approve` のタグを keys にした（承認の確かめは鍵の部品が行う。タグは実装するブロックと 1 対 1）。
