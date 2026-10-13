# S-14 Android のシェル

由来：第9章 3（構成）、4（WebView と JS ブリッジ）、7（権限）、8（鍵保管庫）、9（メディア）、10（自己更新）、11（複数の家族）、12（OS の制限）、14（失敗）、第13章 4.1（lineage）、5（初回のインストール）、第10章 11（シェルの画面）、第2章 4.2・4.4（シェルのトークン）、第12章 5.3（端末鍵）、and-shell.md、and-keystore.md、and-push.md、and-media.md、and-update.md、web-shell.md。通知と着信のシェルの流れは S-08c〜d、S-07e。

## S-14a 初回のインストールと起動（第13章 5、第9章 4.1、7、第10章 11）
![シーケンス図：S-14a 初回のインストールと起動（第13章 5、第9章 4.1、7、第10章 11）（画像を開くと原寸で読める）](../fig/シーケンス図/14_Androidのシェル_1.png)

<details><summary>図の元の文（mermaid）</summary>

```mermaid
sequenceDiagram
  participant U as 利用者
  participant OS as Android
  participant AS as and-shell
  participant AK as and-keystore
  participant AP as and-push
  participant WH as web-shell（WebView）
  participant BD as srv-boundary
  U->>OS: ブラウザで /i/<token> → APK の案内（S-03a）→ /dl/app.apk → 「不明なアプリのインストール」の許可 → インストール（手順書。開発者の確認の制度の施行後は「上級者の手順」）
  U->>AS: 起動（App Links /i/<token> でも）
  AS->>AS: MainActivity::onCreate：WebView の版（138 未満 → WebViewGuide。null → 使えない）
  AS->>AS: Families::list（B-412）= 0 件 → 家族の追加（URL または招待のリンク）
  AS->>BD: GET /family（名前とアイコン）→ Profile の作成 → families.json
  AS->>U: Onboarding（B-414）：通知の許可（POST_NOTIFICATIONS）→ 全画面の着信（canUseFullScreenIntent）→ 電池の最適化の除外（(i) は必須）→ 通知の方式（DistributorChooser の結果と切り替え）
  AS->>AS: ForceStop::check（Android 15）
  AS->>WH: WebHost::load(https://<family.url>/)（B-411。UA に FamilyTalkShell/<版>）
  WH->>AS: Bridge::call getInfo（B-410/B-341）→ シェルの版、OS、WebView、fontScale、家族の識別子、配布元
  Note over WH: 参加の流れ（S-03b）。端末鍵：WH DeviceKeys::create → Platform.wrapDeviceKey → AS → AK DeviceKeyWrap::wrap（B-400。StrongBox → TEE）→ noBackupFilesDir
  WH->>AS: registerPush(vapidPub) → AP UpConnector::register（B-420）→ pushEndpoint（G-07）→ PUT /me/devices/{id}/push
  WH->>AS: updateNameCache（G-03。表示名のコピー）
```

</details>
- 起動のたび：`unwrapDeviceKey`（AK。鍵が失われていれば SHELL-004 → 新しい端末の流れ S-04c）、`ShellToken::get`（期限が近ければ `sessionNeeded` → WebView が `POST /auth/shell-token` → `setShellToken`（G-02））。

## S-14b JS ブリッジの往復（第9章 4.2、I-15）
![シーケンス図：S-14b JS ブリッジの往復（第9章 4.2、I-15）（画像を開くと原寸で読める）](../fig/シーケンス図/14_Androidのシェル_2.png)

<details><summary>図の元の文（mermaid）</summary>

```mermaid
sequenceDiagram
  participant WH as web-shell（Bridge）
  participant AS as and-shell（Bridge）
  participant H as BridgeHandler（CoreCommands / PushCommands / CallCommands / MediaCommands / UpdateCommands）
  WH->>AS: postMessage {id, cmd, args}（WebMessageListener "ftShell"）
  AS->>AS: オリジンの確認（登録した家族の URL だけ。他は SHELL-002 で無視し記録）
  AS->>AS: args を生成した型で検証（SHELL-001）
  AS->>H: handle(cmd, args)
  H-->>AS: result / error{code, params}
  AS-->>WH: {id, ok, result} / {id, ok: false, error}（10 秒で WH が AppError）
  AS-->>WH: {event, data}（fontScaleChanged、backPressed、familySwitched、sessionNeeded、networkChanged、pushEndpoint、shareReceived、updateAvailable、updateFailed、push、callAnswered …）
```

</details>

## S-14c 家族の追加と切り替え（第9章 11）
![シーケンス図：S-14c 家族の追加と切り替え（第9章 11）（画像を開くと原寸で読める）](../fig/シーケンス図/14_Androidのシェル_3.png)

<details><summary>図の元の文（mermaid）</summary>

```mermaid
sequenceDiagram
  participant U as 利用者
  participant AS as and-shell
  participant AP as and-push
  participant AK as and-keystore
  participant WH as web-shell
  U->>AS: 上部の家族の名前 → 一覧（名前、未読の合計）→ 切り替え
  AS->>AS: Families::switch(id)（B-412）：WebHost.load（Profile と URL を替える）
  AS->>WH: familySwitched{id}
  AP->>AP: 通知の instance は全家族で登録済み（(i) は家族ごとに接続）。通知のタップは家族を切り替えて開く
  U->>AS: 「家族を追加」（URL または /i/<token>）→ Families::add → Profile の作成 → S-14a の後半
  alt Profile が使えない端末
    AS->>U: 1 家族のみ（2 つ目で案内）
  end
  U->>AS: 削除 → Families::remove：Profile のデータ、シェルのトークン、AP UpConnector::unregister、AK DeviceKeyWrap::remove
```

</details>

## S-14d 共有の受け口、保存、アップロードの継続、回線（第9章 9）
![シーケンス図：S-14d 共有の受け口、保存、アップロードの継続、回線（第9章 9）（画像を開くと原寸で読める）](../fig/シーケンス図/14_Androidのシェル_4.png)

<details><summary>図の元の文（mermaid）</summary>

```mermaid
sequenceDiagram
  participant OS as Android
  participant AS as and-shell
  participant AM as and-media
  participant WH as web-shell
  participant WM as web-media
  OS->>AS: ACTION_SEND / SEND_MULTIPLE → MainActivity::onNewIntent
  AS->>AM: ShareReceiver::onReceive（B-461）
  AM->>WH: shareReceived{items, text?}（G-04）→ web-ui の「送る」画面
  WM->>WH: Platform.saveToDevice → AM Gallery::saveToGallery（MediaStore Pictures/<家族の名前>/）
  WM->>WH: Platform.uploadInBackground(start, n, m) → AM UploadService（dataSync。通知「送信中 n/m」。WebView を生かす）
  AM->>WH: 1 日 6 時間の制限 → 通知「アプリを開いてください」（キューは残る）
  AM->>WH: networkChanged{type}（ConnectivityManager）→ WM Network::type
```

</details>

## S-14e 自己更新（第9章 10、第13章 4.1）
![シーケンス図：S-14e 自己更新（第9章 10、第13章 4.1）（画像を開くと原寸で読める）](../fig/シーケンス図/14_Androidのシェル_5.png)

<details><summary>図の元の文（mermaid）</summary>

```mermaid
sequenceDiagram
  participant AU as and-update
  participant AS as and-shell
  participant BD as 家族のサーバー（/dl）
  participant AC as and-calls
  participant OS as PackageInstaller
  participant AP as and-push
  participant WH as web-shell
  AU->>AU: UpdateWorker（日次。Wi-Fi）と起動時
  AU->>BD: GET /dl/version.json（各家族。ShellToken.client は不要：無認証）
  AU->>AU: Checker：android.code > 自分の versionCode → UpdateInfo（ABI の選択、sha256、signing_cert_sha256[]）
  AU->>WH: updateAvailable{version}
  alt 自動で更新 ON
    AU->>BD: Downloader：GET /dl/app-<abi>.apk（再開可）→ files/updates/<code>.apk
  else OFF
    AU->>AP: Notifier::notice「更新があります」（タップで取得）
  end
  AU->>AU: Verifier：SHA-256 の一致、getPackageArchiveInfo の証明書の SHA-256 が自分の署名または lineage の配列のどれかと一致、packageName の一致
  alt 不一致
    AU->>AU: 破棄、記録 → WH updateFailed{code, reason}（G-05）→ ウェブアプリが POST /me/report → 管理者に notice。手動のインストールの案内
  end
  AU->>AC: 通話中なら延期（夜間 2〜5 時に再試行）
  AU->>OS: Installer：PackageInstaller の session（USER_ACTION_NOT_REQUIRED。Android 11 以下は確認の画面）→ commit
  OS->>AU: InstallReceiver（SUCCESS / FAILURE）
  OS->>AU: MY_PACKAGE_REPLACED → UpdatedReceiver → AP PushService の再開、Notifier::notice「更新しました」、古い APK の削除
```

</details>

## S-14f OS の制限（第9章 12、14）
- バックアップの除外（`allowBackup=false`、`dataExtractionRules`）は AK `BackupExclusion`。`BOOT_COMPLETED` で起こすのは `PushService` だけ。強制停止の検知は AS `ForceStop`。文字の拡大率は `fontScaleChanged`。描画プロセスの死は AS `WebHost::onRenderProcessGone`（作り直し。通話中なら AC に通知して通話は切れる → 再参加の案内）。`ACCESS_LOCAL_NETWORK` の拒否 → 案内。鍵保管庫が使えない → ソフトウェアの鍵（AK `strength` を設定に表示）。

## 抜け
- G-02、G-03、G-04、G-05、G-07（既出）。
- 自己更新の `GET /dl/version.json` は無認証（第2章 5.11）のためシェルのトークンは不要。and-update.md の `Downloader` の注記を「無認証。同じ OkHttp の client を使うが Authorization は付けない」に直す（本シーケンスで直す）。
