# 基本設計書 第9章 Android のネイティブのシェル

家族専用の通信アプリ　第1版（案）　2026-10-13 · 石川宗  
株式会社流山ソフトウェア開発（NRSD）

<sub>© 2026 株式会社流山ソフトウェア開発（NRSD）担当開発者　石川宗　本書の著作権は、株式会社流山ソフトウェア開発（NRSD）に帰属します。本書が参照する第三者の規格、ソフトウェア、法令の解説等の権利は、各権利者に帰属します。</sub>

---

## 本書の位置付け

- 概要設計書 第9章「Android のネイティブのシェル」を詳細化する。シェルの構成、WebView と JS ブリッジ、通知の受信と配布元、着信と通話、権限、鍵保管庫、メディアの受け口、自己更新、複数の家族、OS の制限への対応、見やすさを定める。
- 部品と版は第14章 7.1。通知の規則は第7章、通話の流れは第6章、鍵は第12章、認証は第3章、メディアは第5章、配布は第13章。本章は「シェルが担う OS との接点」を持つ。
- 準拠：
  - OS の提供者の公式の文書と API（調査 I-02、I-03、I-04、O-03、第14章の調査）
    - 通話と通知：Core-Telecom、CallStyle、前面サービスの型、PiP
    - 通知の配布：UnifiedPush の仕様 AND_3.1.0
    - WebView と更新：androidx.webkit、PackageInstaller
    - 鍵と認証：Keystore、Biometric
    - OS の規則：バックアップの規則、Android 14〜17 の挙動の変更
  - WebRTC 1.0、RFC 8291
  - 法務調査 L-11

## 1. 設計上の決定の一覧

|番号|決定|理由|退けた案|
|---|---|---|---|
|DD-9-1|シェルは Kotlin（Compose）の単一プロセスのアプリ。構成：`MainActivity`（家族の切り替えと WebView の入れ物）、`IncomingCallActivity`（全画面の着信）、`CallService`（前面サービス phoneCall・microphone・camera）、`PushService`（前面サービス specialUse。常時接続）、`UploadService`（dataSync。アップロードの継続）、`DecryptHost`（非表示の WebView。通知の復号と返信の暗号化）、Receiver（起動、UnifiedPush、配布元、更新の状態、自分の更新）、`UpdateWorker`（日次の版の確認）|設計計画書 D-5-7、調査 O-03、第14章 DD-14-4、第7章、第6章|別プロセスの WebView（メモリ）。通知の復号をシェルのネイティブで（MLS の実装が 2 系統）|
|DD-9-2|WebView はウェブアプリを家族のサーバーのオリジンから読み、JS ブリッジは `addWebMessageListener("ft", [家族のサーバーのオリジン])` の 1 本。命令は JSON（4 の表）。`addJavascriptInterface` は使わない。資産の読み替え（AssetLoader）は使わない|第14章 DD-14-4、J3（オリジンの許可リスト）|`addJavascriptInterface`（オリジン検査なし）|
|DD-9-3|WebView の設定：JS と DOM storage を有効、ファイルの読み込みと混在コンテンツを禁止、暗転の許可、`textZoom` は呼ばず OS の文字の拡大率に追従、全画面 + セーフエリア、家族ごとの Profile（使えない端末は 1 家族）、WebAuthn の有効化（Digital Asset Links）、カメラとマイクの許可は実行時権限があれば橋渡し、ファイルの選択は写真ピッカーと文書の選択、描画プロセスの死で作り直し、戻るは履歴があれば戻り、家族のサーバー以外の URL は外部のブラウザへ|R-9-1-1、R-9-6-1、J3|`textZoom` の手計算（非線形の拡大に合わない）|
|DD-9-4|WebView の版の下限は 138（Android 8〜9 の最後）。起動時に版を確かめ、138 未満は更新の案内、更新できる WebView がない端末は「使えません」。minSdk 26、targetSdk 37|第14章 DD-14-14。概要設計書 R-9-1-1 の「Chrome 60」は第14章の調査で 138 に改める（14）|Chrome 60（現代の API がない）|
|DD-9-5|通知：チャネルは messages、calls（高、着信音、振動、全画面）、notices、service（低）。UnifiedPush の connector で受け、配布元 2 系統（(ii) OS の提供者の中継の Web Push のエンドポイントの部品、(i) 家族のサーバーへの常駐の WebSocket）を内蔵する。connector が RFC 8291 を復号した平文を、着信は `IncomingCallActivity` へ、それ以外は `DecryptHost` で復号して通知（トークごとのグループ、直接返信、`MessagingStyle`）。バッジは通知の数で（ランチャーに依る）|設計計画書 D-10-2、R-9-3-1、第7章 DD-7-1〜7-4、調査 I-02|OS の提供者の SDK。別のアプリの配布元を入れさせる|
|DD-9-6|着信：平文の着信を受けたら OS の通話の統合（Core-Telecom）に着信として登録し、CallStyle の通知（全画面の Intent、`CATEGORY_CALL`、`setOngoing`、60 秒で停止、チャネルの着信音）を出す。応答で `CallService` を 5 秒以内に前面にし、通話中の CallStyle、WebView に応答を伝える。音声の経路は Core-Telecom のエンドポイント（受話口、スピーカー、有線、無線）を JS ブリッジで WebView に見せる。ビデオ通話は PiP の自動開始。おやすみモードは OS の規則に従う|設計計画書 D-9-2、R-9-2-1、R-9-4-1、調査 I-04、J3|OS の通話の統合なし（他の通話との競合、音声の経路の制御なし）|
|DD-9-7|初回の権限は次の順に求める。1 番目は通知。2 番目は全画面の着信（拒否なら設定へ）。3 番目は電池の最適化の除外（(i) の時は必須）。4 番目は起動時の復帰の案内。5 番目は通知の方式（自動）。カメラとマイクは通話のたび。家族のサーバーが私有アドレスなら `ACCESS_LOCAL_NETWORK`。「不明なアプリのインストール」は第13章|第7章 DD-7-10、調査 I-04、J3（targetSdk 37）|最初に一括|
|DD-9-8|鍵保管庫：OS の鍵保管庫（StrongBox があればそれ、なければ TEE）の AES-256-GCM の鍵で端末鍵を包み、包んだものをバックアップの対象外の領域に置く。利用者の認証は要求しない（背景の復号に要る）。アプリのロックは OS の認証の画面（生体認証と端末の資格情報）で別に行う|第12章 DD-12-3、第3章 DD-3-8、J3|鍵に利用者の認証を要求（背景で復号できない）|
|DD-9-9|メディア：写真ピッカー（50 件まで。HEIC は端末が復号して JPEG に）、共有の受け口（他のアプリからの「共有」を受けて「送る」画面へ）、端末への保存（MediaStore）、アップロードの継続（WebView が背景になる間は `UploadService`）、回線の種類の橋渡し|第5章 DD-5-6、DD-5-7、R-5-3-2、J3|Share Target（WebView にない）|
|DD-9-10|自己更新：日次と起動時に家族のサーバーの版の情報を確かめ、APK を取得し、SHA-256 と署名の証明書の指紋を自分の署名と照合してから、OS のインストールの API で更新する。Android 12 以上では自分自身の更新を無人で行える設定を使い、11 以下は確認の画面。設定「自動で更新」（既定 ON。夜間・Wi-Fi）。更新の後に常時接続を再開|設計計画書 D-15-6、R-9-5-1、第13章、J3|利用者が毎回 APK を入れ直す|
|DD-9-11|複数の家族：シェルが家族の一覧（URL、名前、Profile、シェルのトークン）を持ち、切り替える。通知と着信は全家族から受ける（配布元の instance = 家族）|第1章 13.4、第3章 DD-3-16|1 家族のみ|
|DD-9-12|OS の制限：バックアップからシェルのデータを全部除外（クラウドのバックアップと端末間の転送）。起動時に起こすのは `PushService`（specialUse）だけ。通話の前面サービスは応答（前面の操作）で起動。強制停止を検知して次の起動で案内。targetSdk 37 の背景の音声は `CallService` で満たす|J3（Android 15・17 の挙動）、調査 I-02|バックアップに含める（鍵とセッションが漏れる）|
|DD-9-13|通話の部品の Android 版（ネイティブの通話の画面）に切り替える基準：実測で (a) 音声の経路の切り替えが Core-Telecom のエンドポイントで効かない、(b) WebView の自動の音声モードがエンドポイントを上書きして戻せない、(c) WebView 固有の音声の遅延や途切れ、のいずれかが国内の主な端末の 2 割以上で起きれば、通話の画面だけをシェルのネイティブに切り替える（鍵は JS ブリッジで渡す）。判断は設計の検証の実測|R-9-4-1、J3（`setSinkId` 不可、Chromium の自動モード）|最初からネイティブ（実装が 2 系統）|

## 2. 目的と範囲

### 2.1 受ける要件

概要設計書 第9章の R-9-1-1〜R-9-6-1。対応は 13。

### 2.2 扱うもの・扱わないもの

- 扱う：
  - シェルの構成、WebView の設定と版、JS ブリッジ
  - 通知の受信と配布元、着信と通話の OS との接点
  - 権限、鍵保管庫、メディアの受け口
  - 自己更新、複数の家族、OS の制限、見やすさ
- 扱わない：ウェブアプリの中身、配布と署名（第13章）、CI（第14章）、画面（第10章）。

## 3. 構成（DD-9-1）

|部品|役割|生存|
|---|---|---|
|`MainActivity`|Compose のシェル（家族の切り替え、シェルの設定、初回の案内）と WebView の入れ物。1 つの WebView を保持し、家族の切り替えで Profile と URL を替える|前面の間|
|`IncomingCallActivity`|全画面の着信（`showWhenLocked`、`turnScreenOn`）。応答・拒否|着信の間|
|`CallService`|前面サービス（phoneCall、microphone、camera）。通話中の CallStyle の通知、Core-Telecom の通話の制御、音声の経路、Wake Lock|通話の間|
|`PushService`|前面サービス（specialUse。`PROPERTY_SPECIAL_USE_FGS_SUBTYPE: "UnifiedPush distributor for family messaging"`）。常駐の WebSocket（配布元 (i)）。再接続、ping|配布元 (i) を選んだ間|
|`UploadService`|前面サービス（dataSync）。WebView を背景で生かしアップロードを続ける。通知「送信中 n/m」|アップロードの間（1 日 6 時間の制限内）|
|`DecryptHost`|非表示の WebView（家族のサーバーの `/notify.html`。同じ Profile。画面の部品を含まない復号の入口だけのバンドル。全体実装設計書 2.2）。通知の復号と直接返信の暗号化。10 秒で結果|必要な時に起動、60 秒で停止|
|`BootReceiver`|`BOOT_COMPLETED` と `LOCKED_BOOT_COMPLETED` で `PushService` を起こす（(i) の時）|—|
|`UpReceiver`|UnifiedPush の connector の受け口（`PUSH_EVENT`、`NEW_ENDPOINT`、`UNREGISTERED`、`REGISTRATION_FAILED`）|—|
|配布元の Receiver|(ii) の部品の受け口、(i) の `REGISTER`／`UNREGISTER` の受け口（配布元の共通部品）|—|
|`InstallReceiver`、`UpdatedReceiver`|インストールの状態、`MY_PACKAGE_REPLACED`|—|
|`UpdateWorker`|WorkManager。日次（Wi-Fi、充電中は任意）の版の確認と取得|—|

- 単一プロセス。WebView は 1 つ（通知用の非表示を除く）。メモリの逼迫で `onTrimMemory` の時は非表示の WebView を捨てる。

## 4. WebView と JS ブリッジ（DD-9-2、DD-9-3、DD-9-4）

### 4.1 設定

|項目|値|
|---|---|
|JavaScript、DOM storage|有効|
|ファイルの読み込み（`allowFileAccess` 等）|無効|
|混在コンテンツ|`MIXED_CONTENT_NEVER_ALLOW`|
|暗転|`setAlgorithmicDarkeningAllowed(true)`。アプリのテーマと `prefers-color-scheme`|
|文字の拡大率|`setTextZoom` を呼ばない（OS の `fontScale` に追従）|
|表示|全画面（edge-to-edge）。ページ側が `viewport-fit=cover` と `env(safe-area-inset-*)`|
|Profile|家族ごと（`MULTI_PROFILE`）。使えない端末は 1 家族（H-9-3）|
|WebAuthn|`setWebAuthenticationSupport(WEB_AUTHENTICATION_SUPPORT_FOR_APP)`。`assetlinks.json` を家族のサーバーが配る（パッケージ名と署名の指紋。第13章）|
|権限の橋渡し|`onPermissionRequest`：オリジンが家族のサーバーで、`RECORD_AUDIO`／`CAMERA` の実行時権限があれば grant、なければ要求してから|
|ファイルの選択|`onShowFileChooser`：`accept` が画像・動画なら写真ピッカー（`PickMultipleVisualMedia`）、それ以外は `OPEN_DOCUMENT`。HEIC は端末が復号して JPEG に（第5章）|
|描画プロセスの死|`onRenderProcessGone`：WebView を破棄し作り直して同じ URL へ。状態はウェブアプリが IndexedDB から復元|
|戻る|`OnBackPressedDispatcher`：`canGoBack` なら `goBack`、なければ JS ブリッジで `backPressed` をウェブアプリへ（画面の階層の戻り）、それでも消費されなければシェルの既定（ホームへ）|
|URL|家族のサーバーのオリジン以外は外部のブラウザへ（子どもの確認はウェブアプリ側で済んでいる）|
|版|起動時に `getCurrentWebViewPackage()`。138 未満なら更新の案内の画面（OS の提供者の更新の経路）。null なら「この端末では使えません」|
|ユーザーエージェント|既定に `FamilyTalkShell/<版>` を付加（ウェブアプリがシェルを検出）|
|Cookie|WebView の Cookie（セッション）。シェルのネイティブの要求はシェルのトークン（第2章 4.4）|

### 4.2 JS ブリッジの命令

形式：ウェブアプリからシェルへは `{id, cmd, args}`、シェルからウェブアプリへの応答は `{id, ok, result|error}`。シェルからのイベントは `{event, data}`。

|cmd（ウェブアプリからシェルへ）|args|result|
|---|---|---|
|`getInfo`|—|シェルの版、OS の版、WebView の版、`fontScale`、家族の識別子、配布元の種類|
|`requestPermission`|`kind`：`notifications`、`camera`、`microphone`、`fullScreenIntent`、`batteryOptimization`、`localNetwork` のいずれか|許可の状態|
|`setBadge`|`n`|—|
|`callStarted`|`call_id, room_id, video, outgoing`|—（`CallService` の起動、Core-Telecom の登録）|
|`callEnded`|`call_id`|—|
|`callState`|`muted, video_on`|—（通話中の通知の更新）|
|`getAudioRoutes`／`setAudioRoute`|—／`route`|エンドポイントの一覧／—|
|`enterPip`|`aspect`|—|
|`keepAwake`|`on`|—|
|`pickMedia`|`kind, max`|—（結果は `onShowFileChooser` の経路で返る）|
|`saveToGallery`|`blob_url, name, mime`|—|
|`share`|`title, text, blob_url?`|—|
|`openExternal`|`url`|—|
|`getNetworkType`|—|`wifi`、`cellular`、`none` のいずれか|
|`wrapDeviceKey`／`unwrapDeviceKey`|`key`／—|包んだ鍵／端末鍵|
|`biometricUnlock`|`reason`|成否|
|`registerPush`|`vapid_pub`|endpoint（配布元の種類）|
|`uploadInBackground`|`state`（`start`、`progress`、`end` のいずれか）、`n`、`m`|—（`UploadService`）|
|`checkUpdate`|—|版の情報の結果|
|`listFamilies`／`switchFamily`／`addFamily`／`removeFamily`|—／`id`／`url`／`id`|—|
|`log`|`level, msg`|—（シェルのログ。本文を含めない）|
|`setShellToken`|`token, expires_at`|—（WebView が取り直したシェルのトークンをシェルに渡す。`sessionNeeded` の復路）|
|`updateNameCache`|`{user_id: name}`|—（表示名のコピー。通知の汎用の本文「{name}さんから」用）|
|`notifyResult`|`title, body, room_id, sender_id, kind, badge`|—（`/notify.html` が復号の結果をシェルに返す）|
|`replyDone`|`room_id, ok`|—（`/notify.html` が直接返信の送信を終えた）|

|event（シェルからウェブアプリへ）|data|
|---|---|
|`push`|平文の通知の中身（`DecryptHost` 用）|
|`callAnswered`／`callDeclined`／`callEndedByOs`|`call_id`|
|`callHeld`|`call_id, held`（他のアプリの通話による保留は true、復帰は false）|
|`audioRouteChanged`|`route, available[]`|
|`pipChanged`|`in_pip`|
|`networkChanged`|`type`|
|`fontScaleChanged`|`scale`|
|`backPressed`|—|
|`familySwitched`|`id`|
|`sessionNeeded`|—（シェルのトークンの再取得）|
|`updateAvailable`|版|
|`pushEndpoint`|`endpoint, kind`（配布元の endpoint の発行と変化。ウェブアプリが `PUT /me/devices/{id}/push`）|
|`shareReceived`|`items: [{blob_url, name, mime}], text?`（共有の受け口。「送る」画面へ）|
|`updateFailed`|`code, reason`（自己更新の検証の失敗。ウェブアプリが `POST /me/report`）|
|`reply`|`room_id, text`（直接返信。`/notify.html` へ）|

## 5. 通知（DD-9-5）

|段|処理|
|---|---|
|配布元の選択|初回：OS の提供者のサービスがあれば (ii)、なければ (i)。設定で切り替え。切り替えると再登録|
|(ii)|OS の提供者の中継の Web Push のエンドポイントの部品（第14章 7.1。ライセンスの確認が条件。確認できなければ自前）。エンドポイントを connector へ|
|(i)|`PushService` が `/api/v1/up/register` で token を得て endpoint を組み、`wss://<サーバー>/api/v1/up/ws` にシェルのトークンで接続。再接続は指数（1〜300 秒）。ping 60 秒。受けた暗号文を connector の経路へ渡し ack|
|connector|`UpReceiver.onMessage` で RFC 8291 を復号（VAPID の検証）し、平文 `{v, event_id, room_id, sender_id, kind, call_id}` を得る|
|分岐|`kind: call` は 6 へ。`kind: call_end` は着信の通知を消す。`kind: notice` はシェルが「知らせ」の通知を出す（本文は汎用）。それ以外は `DecryptHost` へ|
|`DecryptHost`|非表示の WebView に `push` を渡す。ウェブアプリが取得・復号し、`notify` の結果（title、body、room_id、sender）を返す。シェルが通知を出す（`messages` チャネル、グループ = room_id、`MessagingStyle`、直接返信、タップで `MainActivity` の `/r/<room_id>`）。10 秒で結果がなければ「{name}さんからメッセージ」（sender_id からシェルが持つ表示名のコピー。なければ家族の名前）|
|直接返信|`RemoteInput` の文を `DecryptHost` に `reply` で渡し、ウェブアプリが暗号化して送信する。既読は付けない。送信を終えた `replyDone` で、同じ通知 ID の通知を更新する（OS の直接返信の規則。更新しないと送信中の表示が残る）。成功なら自分の返信を `MessagingStyle` に足し、失敗なら「送れませんでした」を足す|
|バッジ|`setNumber(未読の合計)`、`setShowBadge(true)`。ランチャーの数の表示は保証されない（H-9-2）|
|設定の反映|内容の表示の 3 択は `DecryptHost` の結果の形で（ウェブアプリが判定）。音と振動はチャネル（OS の設定へ）|
|内容の表示なし|ロック画面の表示は OS の設定（`VISIBILITY_PRIVATE` で本文を隠す）|

## 6. 着信と通話（DD-9-6）

|順|処理|
|---|---|
|1|平文の着信（`kind: call`）を受けたら `CallsManager.addCall(INCOMING, 相手の名前, video)` で登録する（Core-Telecom。API 33 以下は内部の ConnectionService、34 以上はトランザクション）。成功なら CallStyle の着信の通知を出す（`forIncomingCall`、全画面 Intent の行き先は `IncomingCallActivity`、`CATEGORY_CALL`、`setOngoing`、チャネル `calls` の着信音を繰り返し、60 秒で自動停止）。`canUseFullScreenIntent()` が偽ならヘッドアップの通知（鳴る）|
|2|応答（通知の操作または全画面の画面）で、`CallService` を前面サービスの型 phoneCall、microphone、camera で `startForeground` する（5 秒以内）。続けて CallStyle の通話中の通知（`forOngoingCall`）を出し、`MainActivity` を前面にして JS ブリッジで `callAnswered` を送る。ウェブアプリが `/calls/{id}/join` を呼ぶ|
|3|拒否では JS ブリッジで `callDeclined` を送り、ウェブアプリが `/decline` を呼び、シェルが `disconnect` する。60 秒で応答がなければ `callEndedByOs` を送る（missed はサーバーが記録）|
|4|通話中：`CallControlScope` の `isMuted`、`currentCallEndpoint`、`availableEndpoints` を JS ブリッジで `audioRouteChanged`。ウェブアプリの `setAudioRoute` は `requestEndpointChange` を呼ぶ。OS の通話の UI（Bluetooth のボタン等）の操作も同じ経路で WebView に反映|
|5|発信：ウェブアプリの `callStarted(outgoing)` で `addCall(OUTGOING)` を呼び、`CallService` を起動して通話中の通知を出す|
|6|終了：`callEnded` で `disconnect` し、サービスを止める。OS 側の終了（`onDisconnect`）では `callEndedByOs` を送る|
|7|PiP：ビデオ通話中に `setAutoEnterEnabled(true)` と `setSourceRectHint`（WebView の映像の領域）。PiP 中も WebView を描画。`pipChanged` でウェブアプリが画面を小さい形に|
|8|Wake Lock：`CallService` が `keepScreenOn`（ビデオ）と `PARTIAL_WAKE_LOCK`（音声）|
|9|WebView の音声モード：WebView がマイクを開くと OS の通話モードに自動で入る。Core-Telecom のエンドポイントの制御との相互作用は［要実測］。切り替えの基準は DD-9-13|

- おやすみモード：CallStyle の着信は OS の「通話の例外」に従う（利用者が許していれば鳴る）。
- 通話中の別の着信：サーバーが `CALL-003` で発信者に返すためシェルには来ない。
- 通話中に他のアプリの通話（電話）が来た時：OS の通話の統合により本システムの通話は保留になる（`onSetInactive`）。シェルはウェブアプリにイベント `callHeld{held: true}` を送り、ウェブアプリがマイクとカメラを止める。復帰（`onSetActive`）では `callHeld{held: false}` を送り、ウェブアプリが再開する。

## 7. 権限（DD-9-7）

|権限|時|要求|
|---|---|---|
|`POST_NOTIFICATIONS`|初回|実行時|
|`USE_FULL_SCREEN_INTENT`|初回|`canUseFullScreenIntent()` が偽なら設定の画面へ|
|電池の最適化の除外|初回（(i) は必須、(ii) は勧める）|`ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`|
|`RECORD_AUDIO`、`CAMERA`|通話のたび（なければ）|実行時|
|`ACCESS_LOCAL_NETWORK`|家族のサーバーが私有アドレスの時（targetSdk 37）|実行時|
|`REQUEST_INSTALL_PACKAGES`|自己更新の初回|設定の画面へ|
|`MANAGE_OWN_CALLS`、`FOREGROUND_SERVICE_*`、`RECEIVE_BOOT_COMPLETED`、`WAKE_LOCK`、`SCHEDULE_EXACT_ALARM`（使わない）|マニフェスト|—|

- 写真と動画の読み取りは写真ピッカーのため権限不要。位置はウェブアプリ（WebView の `onGeolocationPermissionsShowPrompt` で実行時権限を求める）。

## 8. 鍵保管庫と生体認証（DD-9-8）

- 鍵：`KeyGenParameterSpec` で作る。
  - 方式は AES/GCM/NoPadding、256 ビット
  - `setIsStrongBoxBacked(true)` を試み、`StrongBoxUnavailableException` なら TEE で作り直す
  - `setUserAuthenticationRequired(false)`
- 端末鍵（第12章 5.3）はウェブアプリが生成し `wrapDeviceKey` で包んでもらい、包んだものをシェルの `getNoBackupFilesDir()` に置く。起動のたびに `unwrapDeviceKey`。ウェブアプリの IndexedDB には置かない（Android ではシェルが持つ）。
- アプリのロック：`BiometricPrompt`（`BIOMETRIC_STRONG or DEVICE_CREDENTIAL`）。`biometricUnlock` の結果だけを渡す（鍵とは結び付けない。第3章 DD-3-8）。
- 鍵保管庫の鍵が失われた（OS の初期化）：端末鍵も失われ、端末は新しい端末として承認（第3章 4.4）。

## 9. メディア（DD-9-9）

|機能|シェル|
|---|---|
|写真ピッカー|`onShowFileChooser` から `PickMultipleVisualMedia(50)` を開く（画像・動画）。HEIC は `ImageDecoder.isMimeTypeSupported` で復号できれば JPEG に変換して渡す|
|共有の受け口|`ACTION_SEND`／`SEND_MULTIPLE`（画像、動画、テキスト、ファイル）を `MainActivity` が受け、JS ブリッジでウェブアプリの「送る」画面へ渡す（blob を渡す）|
|端末への保存|`saveToGallery` で MediaStore に保存（`Pictures/<家族の名前>/`、`Movies/…`、`Download/…`）|
|アップロードの継続|ウェブアプリが `uploadInBackground(start)` を送ると、`UploadService`（dataSync、通知「送信中 n/m」）が WebView を生かす。`end` で停止。1 日 6 時間の制限に当たれば通知で「アプリを開いてください」|
|回線|`ConnectivityManager` の状態を `getNetworkType` と `networkChanged` で渡す|
|カメラ（直接の撮影）|`capture` の `<input>` で OS のカメラを開く|

## 10. 自己更新（DD-9-10）

|順|処理|
|---|---|
|1|`UpdateWorker`（日次。制約：Wi-Fi。充電中は任意）と起動時に `GET /dl/version.json`（家族のサーバー）|
|2|新しい `version_code` なら APK を取得（`/dl/app.apk`。再開可。保存は内部の領域）|
|3|検証：SHA-256 が `version.json` と一致、`getPackageArchiveInfo(GET_SIGNING_CERTIFICATES)` の署名の証明書の指紋が自分の署名と一致（第13章の鍵の交代の系譜も許容）|
|4|設定「自動で更新」が ON なら夜間（2〜5 時）に、OFF なら「更新があります」の通知で利用者の操作を待つ|
|5|`PackageInstaller` の session（`setRequireUserAction(USER_ACTION_NOT_REQUIRED)`。Android 12 以上で自分自身の更新は無人。11 以下は OS の確認の画面）を作り、`commit` する|
|6|`MY_PACKAGE_REPLACED` を受けて `PushService` を再開し、通知「更新しました」を出す|
|7|失敗（署名の不一致、検証の失敗）では、APK を破棄して記録し、管理者に知らせる（ウェブアプリ経由）|

- 初回のインストール（第13章）：ブラウザで APK を取得し、「不明なアプリのインストール」の許可を案内してから、OS のインストールに進む。
- 通話中は更新しない。

## 11. 複数の家族（DD-9-11）

- シェルの `families`：`[{id, url, name, profile, shell_token, distributor_instance}]`。追加は URL の入力または招待のリンク（App Links `/i/*` をシェルが受ける）。
- 切り替え：`MainActivity` の上部の家族の切り替えで、WebView の Profile と URL を替える。通知のタップは家族を切り替えて開く。
- 通知と着信：全家族の配布元の instance を登録。`PushService` は家族ごとに接続（(i) の場合）。
- 削除：Profile のデータ、シェルのトークン、購読を消す。

## 12. OS の制限と見やすさ（DD-9-12）

|項目|対応|
|---|---|
|バックアップ|`allowBackup=false`、`dataExtractionRules`（cloud-backup と device-transfer を全部除外）、`fullBackupContent`（API 30 以下で全部除外）。秘密は `getNoBackupFilesDir()`|
|起動時|`BOOT_COMPLETED` から起こすのは `PushService`（specialUse）だけ|
|強制停止（Android 15）|`ApplicationStartInfo.wasForceStopped()` で検知し、「通知が止まっていました。設定をご確認ください」と知らせる|
|背景の音声（Android 17）|`CallService`（phoneCall）で満たす（該当は未確認）|
|16 KB のページ|Kotlin のみのため対応済み|
|文字の拡大率|WebView は OS に追従。シェルの Compose は sp。`fontScaleChanged` でウェブアプリが再描画|
|TalkBack|WebView の内容の a11y（ウェブアプリの ARIA。第10章）。シェルの画面は `contentDescription`|
|ダーク|OS のテーマに追従（設定で固定可）|
|言語|日本語のみ|

## 13. 要件との対応

|要件|対応|
|---|---|
|R-9-1-1 Kotlin（Compose）のシェルと WebView、権限・通話の音声・PiP・文字の拡大率を JS ブリッジで、WebView の版の下限と minSdk|3、4、DD-9-1〜DD-9-4（下限は 138）|
|R-9-2-1 CallStyle、全画面、Core-Telecom、着信音の繰り返し、ロック中、おやすみモード、初回の許可の案内|6、7、DD-9-6、DD-9-7|
|R-9-3-1 UnifiedPush の connector、内蔵の配布元 2 系統、RFC 8291 の復号、起動時の復帰|5、12、DD-9-5|
|R-9-4-1 WebView の WebRTC（許可の橋渡し、通話の音声モード、前面サービス）、ネイティブに切り替える基準|4.1、6、DD-9-13|
|R-9-5-1 版の情報、ハッシュと鍵の指紋の検証、OS の API で更新、初回の案内|10、DD-9-10|
|R-9-6-1 文字の拡大率の反映、TalkBack|12、DD-9-3|

## 14. 失敗の扱い

|失敗|扱い|
|---|---|
|Core-Telecom の `addCall` が失敗（OEM の制約）|CallStyle の通知だけで着信（OS の通話の統合なし）。音声の経路は OS の既定|
|全画面の許可がない|ヘッドアップの通知（鳴る）。設定への誘導|
|`DecryptHost` が 10 秒で応答しない|汎用の本文。WebView を作り直す|
|常時接続が切れ続ける|指数の待ち（最大 5 分）。1 時間つながらなければ通知「通知の接続が不安定です」。(ii) への切り替えの提案|
|OS が `PushService` を止めた（省電力）|起動時に再開。電池の最適化の除外を再案内|
|更新の APK の署名が一致しない|破棄、記録、管理者に知らせ。手動のインストールの案内（`SHELL-005`）|
|無人の更新ができない端末（Android 11 以下、targetSdk の下限）|確認の画面|
|WebView の描画プロセスが死んだ|作り直し。通話中なら通話は切れる（再参加の案内）|
|Profile が使えない端末|1 家族のみ。2 つ目の追加で案内|
|鍵保管庫が使えない（StrongBox も TEE もない）|ソフトウェアの鍵保管庫（OS の既定）。鍵の強度の低下を設定に表示|
|`ACCESS_LOCAL_NETWORK` が拒否|家族のサーバー（LAN）に届かないため、案内を出す|
|アップロードの前面サービスが 6 時間の制限で止まった|通知「アプリを開いてください」。キューは残る|
|JS ブリッジに未知の命令、または args が型に合わない|応答 `{ok: false, error: {code: SHELL-001}}`。記録（4.2）|
|家族のサーバー以外のオリジンからの JS ブリッジのメッセージ|無視して記録（`SHELL-002`。DD-9-2 により通常は届かない）|
|WebView の版が下限（138）未満|更新の案内の画面（`SHELL-003`。DD-9-4）。更新できる WebView が無い端末は「使えません」|
|鍵保管庫の鍵が失われた（OS の初期化）|端末鍵の復号ができない（`SHELL-004`）。新しい端末として承認の流れへ（8、第3章 4.4）|

## 15. 本章で宣言する既知の制約

|番号|既知の制約|理由|緩和|
|---|---|---|---|
|H-9-1|サイドロードのアプリの全画面の着信の許可の既定は端末により異なり、拒否されていればヘッドアップの通知になる|OS とストアの方針|案内と設定への誘導|
|H-9-2|ランチャーのバッジの数の表示はランチャーに依る|OS の仕組み|通知の数で代える|
|H-9-3|Profile が使えない WebView の端末では 1 家族に限る|部品の対応|案内|
|H-9-4|常時接続は製造者の省電力で止まり得る|第7章 H-7-5|2 系統、再案内|
|H-9-5|WebView の通話の音声の経路の制御は OS の通話の統合と WebView の自動のモードの相互作用に依り、効かない端末があり得る|J3|DD-9-13 の切り替えの基準|
|H-9-6|Android の画面共有は第1版で提供しない|第6章 H-6-3|—|
|H-9-7|更新を受けない WebView の端末では動かない|第14章 H-14-1|案内|
|H-9-8|OS の提供者の中継（(ii)）の部品のライセンスが確認できなければ自前の実装になり、公開の値の扱いの確認が遅れる|第14章 7.1|確認の後に決める|

［要実測］の事項は次のとおり。

- 着信と通話：全画面の着信の既定、Core-Telecom と WebView の音声モードの相互作用、PiP 中の WebRTC
- 通知と電池：`DecryptHost` の時間と電池、常駐の電池
- 更新と権限：無人更新の下限（Android 16・17）、`ACCESS_LOCAL_NETWORK`
- WebView：非線形の文字の拡大、WebView 138 の OPFS、Profile の対応端末、HEIC の MIME の渡り方

## 16. 章の依存

- 本章が与える：JS ブリッジの命令（ウェブアプリの全章）、シェルの通知と着信（第7章、第6章）、配布と更新のシェル側（第13章）、シェルの画面（第10章）。
- 本章が受ける：
  - 第2章（シェルのトークン、up の経路）
  - 第3章（WebAuthn、ロック）
  - 第5章（メディア）
  - 第6章、第7章
  - 第12章（端末鍵）
  - 第14章（部品）

## 17. 他の章・概要設計書への訂正

- 概要設計書 R-9-1-1「WebView の版の下限（Chrome 60）」：138 に改める（第14章 DD-14-14）。
- 第2章 4.4、5.2：シェルのトークンの用途に `/api/v1/up/*` と `/dl/*` の取得を加える（版の情報は無認証なので不要。`up/register` はシェルのトークン）。5.11：`/notify.html`（非表示の WebView 用の小さなページ）を静的に配る。
- 第13章：`assetlinks.json` の中身、初回のインストールの案内、鍵の交代の系譜の許容。
- 第12章 5.3：Android の端末鍵はシェルの鍵保管庫で包みシェルが持つ（IndexedDB に置かない）。
- 第5章 5.1：アップロードの継続は `UploadService`（本章 9）。
- 第14章 7.1：`UploadService` の dataSync の制限、`DecryptHost` のための `/notify.html`、androidx.work を部品に加える。

改版の記録は次のとおり。

- 2026-10-14 の訂正（文書の検査の規則に合わせる）：矢印を言葉に改め、長い文と列挙を分けた。表の中の `\|` の区切りを「いずれか」の書き方に改めた。5 の直接返信に、`replyDone` の後の通知の更新を足した（ブロックのページのクラス図を書く時に、受ける操作が無いと分かった）。4.2 にイベント `callHeld` を足し、6 の保留の文を改めた。`callState` はウェブアプリからの命令で、保留を伝えるイベントが無かった。
- 2026-10-14 の訂正（画面の文言の埋め込みの書き方）：名前が入る所の白丸 2 つの記号を、第10章 9.3 の埋め込みの形に改めた（意味を明示しない記号を使わない）。形は `{name}`、`{admin}`、`{device}`、`{provider}`。
- 2026-10-14 の訂正（境界の正本 api/ を作る時の検査）：14 に 4 行を足し、SHELL-001〜005 の番号を添えた（第2章 9.2）。足した行は、JS ブリッジの未知の命令、オリジンの不一致、WebView の版の不足、鍵保管庫の鍵の喪失。
- 2026-10-14 の訂正（シーケンス（層9）の抜け。全体実装設計書 11.1）：4.2 に命令とイベントを足した（G-02〜G-07）。
  - 命令：`setShellToken`、`updateNameCache`、`notifyResult`、`replyDone`
  - イベント：`pushEndpoint`、`shareReceived`、`updateFailed`、`reply`
- 2026-10-13 第1版（案）の訂正：全体実装設計書 第1稿 15 の改訂を反映した。3 の `DecryptHost` の行に `/notify.html` の中身を書いた。JS ブリッジの契約は api/schemas/ft.bridge/1 から生成する（第2章 10）。
- 2026-10-13 第1版（案）。
