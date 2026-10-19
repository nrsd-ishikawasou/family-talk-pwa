# iPhone の確かめのページ

家族専用の通信アプリ　2026-10-17 · 石川宗  
株式会社流山ソフトウェア開発（NRSD）

<sub>© 2026 株式会社流山ソフトウェア開発（NRSD）担当開発者　石川宗　本書の著作権は、株式会社流山ソフトウェア開発（NRSD）に帰属します。</sub>

---

iPhone の Safari に固有の動きを確かめる静的なページです（基本設計書 第14章 11.6、試験設計書 第1章 DT-1-12）。同じページを 2 か所で使います。

- CI の macOS の iOS の Simulator（試験設計書 T-L3-12）。`tools/ios_probe` の副命令 `sim` が開き、結果の JSON を判定します。
- iPhone の実機の測り（試験設計書 T-L6-09）。下の「実機の測りの手順」で、人がホーム画面に追加して行います。

見るのは OS の挙動（WebCrypto の原始的な操作、IndexedDB に置いた取り出せない鍵、パスキー、Web Push、Service Worker、カメラとマイク）だけです。本システムの鍵の形は入れていません。ウェブのクライアントの鍵の部品（web-crypto）の手順 9 で、本物の鍵の操作をこのページで走らせる形に置き換えます。

## ファイル

|ファイル|中身|
|---|---|
|`index.html`|画面。項目ごとの表とボタン|
|`probe.js`|確かめの本体。結果を画面と `window.__ft_probe`（形 `ft.probe/1`）に出す|
|`sw.js`|Service Worker。push を受けるたびに IndexedDB の鍵を読んで復号と署名を試み、記録する|
|`manifest.webmanifest`、`icon.svg`、`icon-180.png`、`icon-512.png`|ホーム画面に追加する時の情報とアイコン|

- ビルドしない静的なページです。ウェブのクライアントのビルドの設定を `web/` に作る時も、このディレクトリはそのまま残します。
- 送る側（実機の測りの Web Push）はこのページに含めません。開発機の `python -m tools.ios_probe push`（暗号化は同じ道具の `webpush.mjs`）です。
- 配り先は公開リポジトリの運営者の静的な配信（`.github/workflows/pages.yml`）です。`https://nrsd-ishikawasou.github.io/family-talk-pwa/` にこのディレクトリだけが置かれます。

## CI での使い方（T-L3-12）

`.github/workflows/ci.yml` の job `ios` が行います。手で走らせる時は macOS の上で次を行います。

```sh
python -m tools.ios_probe sim --runtime 18-latest --xcode /Applications/Xcode_16.4.app --out ios.json
python -m tools.ios_probe sim --runtime 18.0 --download 18.0 --xcode /Applications/Xcode_16.4.app --out ios-18.0.json
```

道具はまず Simulator を作って起こし、走るたびに作る認証局を入れます。次にこのディレクトリを HTTPS で配り、safaridriver でページを 2 回開きます（2 回目で IndexedDB の開き直しを見る）。続けてパスキーのボタンを押し、判定を出します。名前は `probe.family-talk.test`（実行環境の hosts に書く）を先に試し、開けなければ `localhost` で開きます。結果の JSON の `host` と `hosts_tried` に残ります。

## 開発機での確かめ

ページ自体の誤りを潰すために、Linux のブラウザで開いて JSON が出るかを見ます。OS の挙動の合否は見ません（試験設計書 DT-1-12）。

```sh
python -m tools.ios_probe serve --port 8443 --ca-out /tmp/ft-probe-ca.pem
```

`https://localhost:8443/` を開きます。認証局の証明書は走るたびに作り直します。判定は `python -m tools.ios_probe judge <保存した JSON>` です。

## 実機の測りの手順（T-L6-09）

判定の基準は試験設計書 T-L6-09 の判定の欄が正です。ここには手順と、結果のどこを見るかだけを書きます。

### 要る物

- iPhone 2 台：iOS 18.0〜18.3 の端末と、現行の iOS の端末（試験設計書 E-4）。試す人は家族の外の人でも構いません。
- 送る側：開発機（Python 3.12、Node 22 以上）。iPhone は開発機にも家族のサーバーにもつなぎません。送信は開発機から OS の提供者の配信サービスへ外向きに行います。
- ページは名前・写真・連絡先などの個人の情報を扱いません。終わったら、ホーム画面のアイコンを消し、設定でサイトのデータを消します。

### 準備

1. 開発機で VAPID の鍵対を作ります。鍵対はリポジトリの外に置きます。

   ```sh
   python -m tools.ios_probe push keys --keys ~/ft-probe-vapid.json
   ```

   出た公開鍵（87 文字）を試す人に渡します。
2. 試す人は iPhone の Safari で配り先を開き、共有のメニューから「ホーム画面に追加」を選びます。以後は**ホーム画面のアイコンから開きます**（「0. 環境」の `display_mode` が `standalone` になります）。
3. 開くたびに 1〜4 が自動で走ります。「結果をコピー」で写した JSON を、各項目の後に開発機の担当者へ送ります。

### 項目と順

版（端末）ごとに、次の順で行います。各項目は 3 回です。push の項目は 1 回ごとに 10 通を送ります。

|順|項目|試す人が行うこと|開発機で行うこと|見る欄|
|---|---|---|---|---|
|1|(g) 開いた時の保存|ホーム画面から開く（1 行目が `fresh: true`）|—|`device.g`|
|2|(c) PRF|「パスキーを作る」「パスキーで入る」の後、「PRF を取る」を 3 回|—|`device.c` の `prf_first_sha256_8` が 3 回とも同じ|
|3|(f) カメラとマイク|「カメラとマイクを確かめる」を 3 回。許可の確かめが出たら許可する|—|`device.f`|
|4|購読|公開鍵を欄に入れ「通知を許可して購読」、「購読をコピー」で JSON を送る|受けた JSON を `sub.json` に保存|`subscription`|
|5|(d) 通知の形とバッジ|ページを閉じて待つ。2 回目はページを開いて待つ|`push send --item d`（旧来の形 10 通の後に宣言型 10 通）|`sw_log` の `form`、1 通ごとの通知の数、アイコンのバッジ（欄に書く）|
|6|(e) 着信|ページを閉じて待つ。音の回数と、終わった時に着信の通知が消えたかを欄に書く|`push send --item e`（0・20・40 秒の 3 回と 60 秒の `call_end`）|`sw_log` の `call` と `call_end` の `note`|
|7|(a) 端末の鍵を作る|「端末の鍵を作る」|—|`device.a` の `state: ok`|
|8|(b) ロック中|画面をロックする。送り終わってから開き、「受けた記録を読み直す」|ロックの直後に `push send --item b`、5 分後の分は `--delay 300`|`sw_log` の `key` が `ok` の数と、通知の本文|
|9|(a) 復元の後|暗号化しないバックアップを取り、(1) そのバックアップから復元、(2) 端末を初期化して同じバックアップから復元。各々の後にホーム画面から開く。`unreadable` なら「消して作り直す」|—|`device.a` の開いた時の `state` と、`reset` の `steps`|
|10|(g) サイトのデータの消去の後|設定でサイトのデータを消してから、ホーム画面から開く|—|`device.g` の最後の行が `fresh: true`|

開発機の送信の例は次のとおりです。

```sh
python -m tools.ios_probe push send --keys ~/ft-probe-vapid.json --sub sub.json --item d --log d-1.json
python -m tools.ios_probe push send --keys ~/ft-probe-vapid.json --sub sub.json --item e --log e-1.json
python -m tools.ios_probe push send --keys ~/ft-probe-vapid.json --sub sub.json --item b --log b-now-1.json
python -m tools.ios_probe push send --keys ~/ft-probe-vapid.json --sub sub.json --item b --delay 300 --log b-5min-1.json
```

- `--dry-run` で送らずに中身を見られます。中身の形は本システムの通知の形（`ft.push/1`、宣言型の包み `ft.push.declarative/1`）です。
- (b) は配信サービスでの置き換え（`Topic`）を付けずに送ります。鍵を読めたかは SW の記録（`sw_log` の `key`）で数えます。
- (a) の 9 は端末の中身を戻す操作です。試す人の普段使いの端末では行いません。

### 結果の記録

試す人から受けた JSON と送信の記録（`--log`）は、版・端末・回ごとに手順書の試験の記録（`dev/docs/jp/手順/試験の記録.md` の T-L6-09 の表）へ書きます。
