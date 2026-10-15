# 境界の変更の記録

境界の文書（api/ の全部）の版ごとの変更を、Keep a Changelog の形で記録する。版は SemVer で、openapi.yaml と asyncapi.yaml の `info.version` と同じ値（基本設計書 第2章 10）。1.0.0 の公開までは 0 系（互換を壊す変更を許す）。

## [未公開]

### 追加

- 0.1.0：基本設計書 第12章 5.2「拒まれた Commit からの戻し」と第2章 5.5 の経路と欄。
  - `POST /rooms/{id}/mls/reject`（受け手の端末が処理できなかった Commit か Welcome の報告。`{generation, epoch?, commit_ref?, reason}`。応答 `{reinit_allowed_at}`。`KEY-007`、`KEY-009`）
  - `POST /rooms/{id}/mls/reinit`（グループの再初期化。応答 `{generation, group_id, epoch}`。`KEY-013`）
  - `GET /rooms/{id}/mls/epoch` に `generation`、`group_id`、`pending_readd`。`GET /rooms/{id}/mls/commits` に `generation` と項目の `commit_ref`。`POST /rooms/{id}/mls/commit` の応答に `commit_ref`
  - `room.mls` に `generation`（`commit_ref` は任意に。epoch 0 には無い）。`room.member` の `change` に `pending_readd`
  - `KEY-013`（409。MLS のグループの再初期化を許さない。params.reason は `not_pending`、`too_early`、`limit`。params.allowed_at）
  - `fkb-history/1` と `ft.event.mls/1` の `group_id` はグループの識別子（トークの識別子の 16 バイトと世代の 4 バイト）の base64url
- 0.1.0：`GET /recovery/{request_id}/share`（承認する端末が自分宛ての片のコピーを取る。規則 `share_holder`。`AUTH-007`、`KEY-005`。基本設計書 第12章 5.4 (c)、第2章 5.4）。
- 0.1.0：`KEY-012`（403。回復コードの照合の値が合わない。基本設計書 第12章 5.4 (b)、第2章 9.2）。`POST /me/keybundle/recovery-code/verify` の x-errors に加えた。
- 0.1.0：`ft.push/1` に `reason`（`call_end` の時に必須。`missed`、`answered`、`declined`、`cancelled`。基本設計書 第7章 7）。
- 0.1.0：`ft.push.declarative/1` の `notification` に `mutable: true`（必須。外側と両方に置く。基本設計書 第2章 7）。

- 0.1.0：パスキーの追加の 2 つの経路の x-errors に番号を足した。トークンの無い要求は認可 approved を業務が確かめ、その番号が無かった。
  - `POST /auth/passkey/register/options` に `AUTH-001`（主体が無い）と `KEY-004`（端末が未承認）
  - `POST /auth/passkey/register/verify` に `KEY-004`（端末が未承認）
- 0.1.0：基本設計書 第3章 9.1（子の誕生日は保護者が書く）と第7章 9（知らせの通知）の 2 つの経路。
  - `PUT /me/guardian/children/{id}/birthday`（保護者が子の誕生日を書く。再認証。`MEM-001`、`MEM-005`）
  - `GET /me/notices/{id}`（通知を受けた端末が知らせの中身を取る。`MEM-001`）
- 0.1.0：`MEM-012`（409。誕生日と子ども区分が合わない。基本設計書 第3章 9.1、第2章 9.2）。`PUT /admin/users/{id}/child` の x-errors に加えた。
- 0.1.0：`recovery.used` の `method` に `reset`（管理者の再設定のリンクの使用。基本設計書 第3章 4.6、第2章 6.2）。
- 0.1.0：`PATCH /me` の x-errors に `MEM-007`（子ども区分の構成員は自分の誕生日を書けない。基本設計書 第3章 9.1）。`MEM-007` の意味に誕生日の変更を足した。
- 0.1.0：x-errors に 3 つの番号を足した。業務の返す番号の一部が x-errors に無かった。
  - `POST /auth/qr/approve` に `MEM-006`（子ども区分の端末の追加は保護者の承認。基本設計書 第3章 4.4）
  - `POST /me/recovery-cards/{n}` に `MEM-002`（状態が通常でない構成員。第12章 5.4 の 1）
  - `POST /me/guardian/approvals/{id}` に `AUTH-007`（求めの期限切れ。第3章 8.3、9.2）
- 0.1.0：2 つの番号を足し、`POST /me/recovery-cards/{n}` の x-errors に加えた（基本設計書 第12章 5.4 の受け取りの記録の規則、第2章 9.2）。
  - `MEM-010`（403。子ども区分の構成員は家族回復鍵のカードを持てない）
  - `MEM-011`（409。カードの持ち主の重なり。params.held）
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
- 0.1.0：`POST /me/mls/rejoin`（承認済みのまま leaf を失った端末が端末の追加待ちを立て直す。基本設計書 第12章 5.5、第2章 5.4）。`GET /rooms/{id}/mls/epoch` の応答に `pending_add`、`pending_add_devices`、`pending_remove`。`room.member` に `device_id?`。
- 0.1.0：`GET /calls/{id}` の応答に `expires_at`（呼び出しの期限。開いた時の着信の確かめ。基本設計書 第6章 4.1）。
- 0.1.0：`POST /up/register` の応答に `ws_secret`（配布元の接続の秘密）。`GET /up/ws` の `x-authz` を `up_secret` に（基本設計書 第2章 4.4、6.4、10）。
- 0.1.0：`GET /family/recovery-cards`、`POST /me/recovery-cards/{n}`（家族回復鍵のカードの持ち主。基本設計書 第12章 5.4、第3章 3.1）。`MEM-009`（409。その札は別の人が持っている）。
- 0.1.0：`GET /rooms/{id}/receipts`、`GET /me/notices`（全差分の取り直しで既読と知らせを戻す。基本設計書 第2章 13）。`ft.push/1` に `video`、`ft.event.call/1` に `cancelled`、`ft.event.mls/1` に `silent`。`POST /internal/sfu/webhook` の要求の形。`POST /rooms/{id}/events` に `MEDIA-005`。

- 0.1.0：`ft.push.declarative/1`（宣言型の Web Push の包み。基本設計書 第2章 7、第7章 5.1）。欄は `web_push: 8030` と `mutable: true` で、`notification.data` が `ft.push/1`。

### 削除

- 0.1.0：`POST /devices/{did}/inbox` の `kind` から `share` を外した（片は `POST /recovery/{request_id}/approve` だけが置く。基本設計書 第12章 5.2）。
- 0.1.0：`ft.push.declarative/1` の `notification.renotify` を外した（WebKit は実装せず、tag の置き換えに頼らない。基本設計書 第7章 DD-7-7）。
- 0.1.0：`recovery.used` の `method` の `device`（基本設計書 第2章 6.2、第12章 5.4）。既存の端末による承認は端末の追加であって回復ではなく、書く操作が無かった。知らせは `device.changed` と `notice`（`new_device`）が担う。`ft.event.system/1` の `recovery_used` の `params.method` も同じ列挙にした（`reset` を足し `device` を外した）。
- 0.1.0：`POST /rooms/{id}/mls/proposals`（基本設計書 第12章 5.2、第2章 5.5）。サーバーは提案を作らず（外部提案は署名鍵を要する）、端末の提案を配る経路も無かった。Add と Remove は Commit の中に値で入れる。
- 0.1.0：`GET /rooms/{id}/mls/epoch` の `tree_hash`（基本設計書 第2章 5.5。出どころが無かった）。
- 0.1.0：`GET /rooms/{id}/mls/commits` と `POST /rooms/{id}/mls/commit` の `group_info`（使う所が無かった）。
- 0.1.0：`GET /calls/{id}/turn`（回線の形 C は VPS の転送で同じ SFU の内蔵の TURN を使う。基本設計書 第11章 5、第2章 5.7）。

### 変更

- 0.1.0：`POST /me/devices/{id}/keypackages` の `key_packages` の `maxItems` を 21 から 20 に正した（基本設計書 第2章 5.4 の「最大 20 + 最後の手段 1」。最後の手段は別の欄 `last_resort`）。
- 0.1.0：MLS の配送サービスの形を、サーバーが Commit を読む形にした（基本設計書 第12章 5.2、第2章 5.5）。
  - `POST /rooms/{id}/mls/commit` の `commit` は PublicMessage の MLSMessage、`welcome` は Welcome の MLSMessage。サーバーが Add と Remove を読み、規則に合わなければ `KEY-009`。
  - `POST /me/devices/{id}/keypackages` の要素は MLSMessage（`mls_key_package`）。サーバーが credential と署名を確かめる（x-errors の `KEY-009`）。
  - `PUT /me/devices/{id}/keys` の `mls_credential` は第12章 5.3 の形（違えば `KEY-009`）。
- 0.1.0：鍵バンドルの版（基本設計書 第12章 5.4）。`fusr/1` に `version`、`fkb/1` と `fkb-history/1` に `key_version`（必須）、`KeyBundleCopy` に `key_version`。`KEY-011`（409。版が古い。params.version）を足した。x-errors に加えたのは `PUT /me/keybundle/{kind}/{target}` と `PUT /me/keybackup/{group_id}/{epoch}`。
- 0.1.0：`PUT /me/signing-key` の要求に `device_sigs`（承認済みと休眠の全端末の新しい `user_sig`。初回は空）を足した。置き換えは全部が通る時だけ（x-errors に `KEY-001`、`KEY-003`）。`user.changed` の `fields` に `signing_key`（基本設計書 第12章 5.4、第2章 6.2）。
- 0.1.0：回復コードの照合の値（基本設計書 第12章 5.4 (b)）。`PUT /me/recovery-code` の要求を `{verifier, params}` にした。`POST /me/keybundle/recovery-code/verify` の要求を `{verifier}` にした（Argon2id の出力そのものを置く形から）。
- 0.1.0：`POST /family/recovery-key/use` を、本文なしでコピー (d)（`fkb/1`）を返す形にした（x-errors に `KEY-005`）。渡したことを使用として記録する（基本設計書 第12章 5.4 (d)）。
- 0.1.0：秘密分散の断片は片を持つ構成員の全端末に置く（基本設計書 第12章 5.4 (c)）。`PUT /me/keybundle/share/{target}` の `target` は片を持つ構成員の承認済みの端末（x-errors に `KEY-001`）。`recovery.request` の宛先は片を持つ構成員の全端末。規則 `share_holder` はその端末がコピーの対象であること。
- 0.1.0：`POST /devices/{did}/inbox` の `kind` から `welcome` を外した（サーバーが Commit の受け付けで置く）。署名の確かめ（x-errors に `KEY-009`）を足した。`InboxItem` の `sig` を任意にした（welcome だけ無い。基本設計書 第12章 5.2）。
- 0.1.0：`device.changed` の `change` に `guardian_approved`（子の端末の鍵の承認を保護者が通した時。基本設計書 第3章 4.4）。`MEM-006` の意味に鍵の承認を足した。
- 0.1.0：鍵の運搬の 5 つの経路を、端末が送る欄だけの形にした（基本設計書 第2章 5.4）。サーバーが決める欄（作成の時刻、承認の時刻、識別子、受け取りの番号）を端末が送る形になっていた。
  - `PUT /me/devices/{id}/keys` の要求を `{sig_pub, kem_pub, mls_credential}` にした（`fdev/1` から。応答は `fdev/1` のまま）
  - `PUT /me/signing-key` の要求を `{user_sig_pub}` にした（`fusr/1` から。応答は `fusr/1` のまま）。置き換えは再認証（`x-reauth: conditional`）で承認済みの端末だけ（x-errors に `KEY-004`）
  - `POST /recovery/{request_id}/approve` の要求を `{ciphertext, sig}` にした（端末宛メールボックスの受け取りの形から）。自然な鍵は `request_id`
  - `POST /me/recovery/request` の x-errors を `KEY-002`（承認済みの端末は頼めない）と `KEY-005`（秘密分散の断片が無い）にした（`KEY-004` から）
  - `PUT /me/recovery-code` の x-errors に `KEY-004`（置き換えは承認済みの端末だけ）
- 0.1.0：`KEY-003` の意味に「`user_sig` が本人の署名鍵の公開部で通らない」を足した（基本設計書 第2章 9.2）。
- 0.1.0：`AUTH-005` の params.state に `access_denied` を足した（RFC 8628 3.5。基本設計書 第3章 4.4、第2章 5.2・9.2）。保護者が子の端末の追加を承認しなかった時の値。それまで拒否は `expired` に寄せてあり、新しい端末が期限切れと区別できなかった。既存の端末の本人が断る経路は設けない（第3章 DD-3-7）。
- 0.1.0：`DELETE /me/passkeys/{id}` の `id` を base64url にした（UUIDv7 から）。パスキーの識別子は資格情報の識別子で、`GET /me/passkeys` の `id` と同じ値（基本設計書 第2章 5.2）。
- 0.1.0：`GET /me/passkeys` の `before`、`after` を base64url にした（共通の UUIDv7 から）。一覧の cursor は要素の識別子で、パスキーでは資格情報の識別子（基本設計書 第2章 3）。
- 0.1.0：端末の鍵の指紋を端末が求める形にした（基本設計書 第12章 5.3、第3章 4.4）。サーバーは指紋を求めず、鍵の公開部を返す。
  - `POST /auth/qr/start` の要求に `sig_pub` を足した（`kem_pub` と並ぶ）。
  - `GET /auth/qr/{user_code}` の応答の `kem_pub_fingerprint` を `sig_pub`、`kem_pub` に替えた。`platform` を列挙にした。
  - `GET /me/devices` の `Device` に `keys`（`fdev/1`。鍵の登録の前は無い）を足した。
- 0.1.0：`fkb/1` の kind=prf から `prf: {credential_id, salt}` を外した。salt は固定で、資格情報の識別子は `target_id`（第12章 5.4 (e)）。
- 0.1.0：`sync.gap` の `reason` を `restore` だけにした（第2章 6.2：保持の外は `/sync` の 410 で、イベントは復元の時だけ）。
- 0.1.0：`GET /me/notices` のタグを push にした（知らせの記録の持ち主は通知の部品。タグは実装するブロックと 1 対 1）。応答の要素に `level` と `text_key` を足し、`params` を必須にした。WebSocket の `notice` のイベントの payload と同じ欄にし、全差分の取り直しで同じ中身が戻るようにした（基本設計書 第2章 5.3、6.2、13）。
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
- 0.1.0：`POST /me/recovery-cards/{n}` の `n` を整数の 2 か 3 にした（文字列だった。基本設計書 第2章 5.3）。
- 0.1.0：セッションを出す 5 つの操作の 200 に `Set-Cookie`（`__Host-session`）を宣言した（基本設計書 第2章 4.1）。5 つは初回の管理者の作成、招待の受諾、パスキーのログインの検証、QR の poll、パスキーの作り直し。ログアウトと `DELETE /me/devices/{id}` の 200 には消す `Set-Cookie` を宣言した。応答で返すと決めていたが宣言が無く、実装が値を渡す口が無かった。
- 0.1.0：`POST /auth/passkey/login/verify` の要求に `device_name` を必須で足した。新しい端末（未承認）になる時の名前が無かった（基本設計書 第3章 4.2。既知の端末と確かめられた時は使わない）。
