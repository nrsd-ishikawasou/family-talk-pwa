# ci-tools（配布。CI のワークフロー、Dockerfile、道具、ライセンスの表示、リポジトリの骨格）

## 受ける節

- [第14章 8 境界の型の生成](../../../../docs/jp/design/14_基本設計書_開発基盤.md#8-境界の型の生成)（生成の道具の呼び出し）
- [第14章 9 ライセンスの方針と第三者の表示](../../../../docs/jp/design/14_基本設計書_開発基盤.md#9-ライセンスの方針と第三者の表示)（ライセンスの区分と道具）
- [第14章 10 リポジトリの構成](../../../../docs/jp/design/14_基本設計書_開発基盤.md#10-リポジトリの構成)（リポジトリの構成、security.txt、CODEOWNERS、Rulesets）
- [第14章 11 CI](../../../../docs/jp/design/14_基本設計書_開発基盤.md#11-ci)（CI：変更ごと、リリース 2 段、定期、ワークフローの安全、署名鍵の扱い）
- [第14章 12 配布物（第13章への入口）](../../../../docs/jp/design/14_基本設計書_開発基盤.md#12-配布物第13章への入口)（配布物の置き場と検証）
- [第14章 13.2 脆弱性の確認](../../../../docs/jp/design/14_基本設計書_開発基盤.md#132-脆弱性の確認)（脆弱性の確認）
- [第14章 14 書き方の取り決め](../../../../docs/jp/design/14_基本設計書_開発基盤.md#14-書き方の取り決め)（書き方の取り決めの検査：禁止語）
- [第14章 15 版の規則](../../../../docs/jp/design/14_基本設計書_開発基盤.md#15-版の規則)（版の規則）
- [第14章 5.1 言語と実行環境](../../../../docs/jp/design/14_基本設計書_開発基盤.md#51-言語と実行環境)（CGO なし、最小の基盤）
- [第14章 5.4 イメージ](../../../../docs/jp/design/14_基本設計書_開発基盤.md#54-イメージ)（イメージの一覧）
- [第13章 3 配布物（DD-13-2）](../../../../docs/jp/design/13_基本設計書_配布と更新.md#3-配布物dd-13-2)（配布物の作り方と置き場）
- [第13章 4.2 手順（リリースのたび）](../../../../docs/jp/design/13_基本設計書_配布と更新.md#42-手順リリースのたび)（リリースのたびの手順の CI の側：前段、後段、APK の検証）
- [第13章 6.1 版](../../../../docs/jp/design/13_基本設計書_配布と更新.md#61-版)（SemVer、versionCode、タグ）
- [第13章 6.3 リリースの確認の一覧](../../../../docs/jp/design/13_基本設計書_配布と更新.md#63-リリースの確認の一覧)（リリースの確認の一覧）
- [第13章 7 公開リポジトリの文書（DD-13-1、DD-13-9）](../../../../docs/jp/design/13_基本設計書_配布と更新.md#7-公開リポジトリの文書dd-13-1dd-13-9) の公開リポジトリの文書
  - README、LICENSE、NOTICE、THIRD_PARTY_NOTICES、REUSE
  - SECURITY.md、security.txt、CHANGELOG
- [第13章 8 失敗の扱い](../../../../docs/jp/design/13_基本設計書_配布と更新.md#8-失敗の扱い) の CI の行
- 試験設計書 第1章 2（L2、L5 の置き場）、第3章 2（T-L2）、第3章 5（T-L5）、第4章 E-2、E-3
- 全体実装設計書 5.1（生成の表）、6.1（段と置き場）、6.3（道具の一覧）、6.4（平文検査）、7（イメージの中身、署名無しの組み方、配布物の試験）、8（運営の流れ）

- [第1章 9.3 公開リポジトリ](../../../../docs/jp/design/01_基本設計書_全体構成.md#93-公開リポジトリ)（公開リポジトリの構成）
- [第14章 16 失敗の扱い](../../../../docs/jp/design/14_基本設計書_開発基盤.md#16-失敗の扱い)（開発基盤の失敗の扱い：CI の失敗、生成の失敗、ランナーの欠け）

## 依存

- 下：全部（組む側）。
- 部品：
  - CI と組み立て：GitHub Actions（全ての uses を SHA で固定。第14章 11.4）、Docker BuildKit、testcontainers（L1）
  - 署名とビルド証明：cosign、`actions/attest-build-provenance`、apksigner
  - SBOM と脆弱性：Syft、Trivy、govulncheck、osv-scanner、cyclonedx-gomod、CycloneDX Gradle Plugin
  - ワークフローの検査：zizmor、actionlint
  - 境界の検査：Spectral、AsyncAPI CLI、oasdiff
  - ライセンス：go-licenses、licensee、AboutLibraries、reuse
  - 文書の検査：markdownlint-cli2、textlint、lychee
- 上：なし。

## クラス図（ワークフローと道具の構造）

![クラス図：ci-tools（画像を開くと原寸で読める）](../fig/クラス図/ci-tools_1.png)

<details><summary>図の元の文（mermaid）</summary>

```mermaid
classDiagram
  class CiWorkflow {
    +jobs boundary
    +jobs server
    +jobs web
    +jobs android
    +jobs docs
    +jobs workflows
    +permissions
  }
  class ReleaseWorkflow {
    +stage1 build
    +manual sign
    +stage2 image
    +checks
    +permissions
  }
  class PeriodicWorkflow {
    +weekly
    +dependabot
  }
  class Dockerfiles {
    +Dockerfile
    +Dockerfile.caddy
  }
  class Tools {
    +gen_errors / gen_config
    +errors_check
    +gen_texts
    +config_check
    +trace_requirements
    +known_red
    +no_plaintext
    +forbidden_words
    +dist_check
    +licenses
    +fetch_vectors
    +gen_testdata
    +ftlint
    +bundle_openapi
    +genimpl
    +tests/
  }
  class RepoSkeleton {
    +README.md
    +LICENSE / NOTICE / LICENSES/ / REUSE.toml
    +SECURITY.md
    +CHANGELOG.md
    +.github/
    +.editorconfig
    +security.txt の雛形
    +Rulesets
  }
  CiWorkflow ..> Tools
  ReleaseWorkflow ..> Dockerfiles
  ReleaseWorkflow ..> Tools
  PeriodicWorkflow ..> Tools
```

</details>

クラス図の注記（図の各メンバーの説明。仕様の節と番号を含む）

|クラス|メンバー|注記|
|---|---|---|
|CiWorkflow|`+jobs boundary`|L2：Spectral、AsyncAPI validate、生成の再現性（第14章 8：tools/gen_* と 4 つの生成を 2 回行って一致、生成した型でビルド）、oasdiff、AsyncAPI diff、tools/errors_check、tools/config_check、tools/trace_requirements|
|CiWorkflow|`+jobs server`|生成と生成の再現性、gofmt、go vet、staticcheck（生成の package `internal/boundary/gen` を除く。oapi-codegen の出力の文体の指摘は直さないため。integration の印つきも見る）、ftlint（自前の検査）、go build、go-licenses check（第14章 9.3 の部品と本システム自身を除く）、govulncheck、go test -race（L0）、integration（L1：testcontainers の postgres + tools/no_plaintext）、イメージのビルド（amd64、arm64。push しない）|
|CiWorkflow|`+jobs web`|eslint、prettier、tsc、許可リストのスクリプト、pnpm audit、Vitest（L0、L3 ブラウザモード Chromium と WebKit、axe）、Playwright（L4。Compose を起動）、ビルド|
|CiWorkflow|`+jobs android`|ktlint、detekt、Android Lint、kotlinc、fabrikt の生成の再現性、licensee、Gradle の依存検証、JUnit（L0）、Gradle Managed Devices（L4 のシェルの分。KVM）、未署名の APK（試験の鍵で署名した版も）|
|CiWorkflow|`+jobs docs`|markdownlint-cli2、textlint、lychee、tools/forbidden_words（禁止語の一覧は secret。fork の PR では secret が無いため skip し、main への merge の前の run で必ず走る：Rulesets の必須の検査）|
|CiWorkflow|`+jobs workflows`|zizmor、actionlint|
|CiWorkflow|`+permissions`|contents: read 既定。書き込みはジョブ単位|
|ReleaseWorkflow|`+stage1 build`|タグ vX.Y.Z：再現可能なビルド（SOURCE_DATE_EPOCH、-trimpath、CGO_ENABLED=0、Vite の決定性を 2 回で確認、Gradle の再現可能なアーカイブ、R8 の非決定を切る、baseline.prof 無効、PNG の最適化を切る）。成果物は ft（amd64、arm64）と sha256、未署名 APK 3 種、version.json の草案、SBOM、ビルド証明（attest-build-provenance。再利用可能なワークフロー。SLSA L3）。これらを Release の草案に載せる|
|ReleaseWorkflow|`+manual sign`|保守者が次の順に行う：ビルド証明の検証、PKCS#11 で署名、一致の検証（署名を除いた中身）、apksigner verify、Release に署名済み APK と sha256 を載せる|
|ReleaseWorkflow|`+stage2 image`|保守者の合図（workflow_dispatch）で次の順に行う。1. Release の APK を取得し、sha256 と指紋を検証する。2. イメージを組む（本システム、caddy。BuildKit rewrite-timestamp、compatibility-version 固定）。3. GHCR に載せる（X.Y.Z と X.Y。latest なし）。4. cosign の鍵なし署名、ビルド証明、SBOM（CycloneDX 1.7）を referrer に付ける。5. Release に digest と version.json を載せ、immutable で公開する|
|ReleaseWorkflow|`+checks`|第13章 6.3 の一覧を job の summary に|
|ReleaseWorkflow|`+permissions`|id-token: write、attestations: write はリリースのジョブだけ。pull_request_target 不使用|
|PeriodicWorkflow|`+weekly`|依存の再走査（osv-scanner）、公開済みのイメージの Trivy、OpenSSF Scorecard、security.txt の Expires の 30 日前の警告|
|PeriodicWorkflow|`+dependabot`|アクション、Go、npm、Gradle、Docker の基盤。cooldown 3 日。security update は即日|
|Dockerfiles|`+Dockerfile`|本システム：多段。ビルド段（Go、Node）の出力を最終段（最小の静的な基盤 + ca-certificates + tzdata）に写す。ft、/srv/www、APK、version.json、手順書、THIRD_PARTY_NOTICES。非 root 10001。HEALTHCHECK。シェルなし|
|Dockerfiles|`+Dockerfile.caddy`|caddy + DNS の提供者のモジュール群（xcaddy 相当）+ Caddyfile|
|Tools|`+ftlint`|Go の自前の検査（go/analysis。`server/internal/lint` と `server/cmd/ftlint`）。`time.Now`・`time.Since`・`time.Until` の直接の呼び出し（srv-common の clock の中を除く。B-003）、`logx.LabelName` への定数でない値の変換、logx の記録の msg に定数でない文字列（第12章 11）、`math/rand` の import（試験を除く）を止める|
|Tools|`+bundle_openapi`|api/openapi.yaml の外のファイルへの参照を components/schemas に名前つきで取り込む（名はディレクトリの名から。`ft.event.mls/1.json` を `FtEventMls1`）。oapi-codegen の設定（static のタグと `x-websocket` の操作を除く一覧）も書く。出力は中間の生成ファイル（コミットしない。第14章 8）|
|Tools|`+genimpl`|server/cmd/genimpl：生成した `StrictServerInterface` をタグごとの interface に分け、srv-boundary の `Implementations` を書く（Go の AST を読む）|
|Tools|`+gen_errors / gen_config`|errors.yaml、config.yaml、ft.bridge から Go と TypeScript の定数を生成する|
|Tools|`+gen_texts`|基本設計書 第10章 9.2 のエラーの文言の表から web/src/i18n/ja/errors.json を作る（鍵 err.<番号>、値 what と next）。エラーコード表と表の番号が 1 対 1 でなければ誤り。生成してコミットし、CI は `--check`|
|Tools|`+errors_check`|エラーコード表の test の欄と L1 の試験（integration の印つきのファイル）の名前、user_text_key と ja.json。試験を求める番号は `--tests`（接頭か番号）で、作り終えたブロックの分に段階で広げる（CI は NET と SRV-001・003・004。SRV-002 は srv-media）。文言の表は `--texts`（CI は web/src/i18n/ja/errors.json）|
|Tools|`+config_check`|config.yaml と docs の数値の表|
|Tools|`+trace_requirements`|試験のコードの注記（R:）と試験設計書 第2章の表|
|Tools|`+known_red`|known_red.txt と各段の記録（go test -json、Vitest JSON、Playwright JSON、JUnit XML）|
|Tools|`+no_plaintext`|固有の語（Git の外の一覧 tools/plaintext_markers.txt、または環境変数 FT_PLAINTEXT_MARKERS）を DB のダンプ、/data/media、ログ、restic の restore から走査。見つけた時も語は出さず番号と場所だけ。一覧が無い・12 文字未満の語は止まる。`--generate N` で乱数の語を作る（CI は走るたびに作る）。走査の置き場は L1 の試験が本文を書くブロックから足す|
|Tools|`+forbidden_words`|他社の名称と痕跡（Git の外の一覧）をリポジトリと配布物から|
|Tools|`+dist_check`|イメージの中身、SHA-256、非 root、HEALTHCHECK、公開ポート、THIRD_PARTY_NOTICES、version.json の整合|
|Tools|`+licenses`|3 層の THIRD_PARTY_NOTICES と許可リスト|
|Tools|`+fetch_vectors`|MLS の相互運用の検証ベクトル（版と SHA-256）|
|Tools|`+gen_testdata`|模擬の家族（D-1）|
|Tools|`+tests/`|道具の試験（L0）|
|RepoSkeleton|`+README.md`|第13章 7 の中身|
|RepoSkeleton|`+SECURITY.md`|報告の手順、14 日、対象の版、公開の方針|
|RepoSkeleton|`+CHANGELOG.md`|Keep a Changelog 1.1.0|
|RepoSkeleton|`+.github/`|workflows、dependabot.yml、CODEOWNERS|
|RepoSkeleton|`+security.txt の雛形`|deploy とイメージ（Expires 1 年未満）|
|RepoSkeleton|`+Rulesets`|main 1 本、PR 必須、状態の検査、force push 禁止、直線の履歴、署名コミット（個人の無料のアカウントで使えない規則は手順で代替【未確認】）|

## 公開操作（このブロックが所有する物。番号は B-640〜）

|番号|物|使う側|由来|
|---|---|---|---|
|B-640|`ci.yml`（L0〜L4 の段）、`release.yml`（前段と後段）、`periodic.yml`|公開リポジトリの CI|[第14章 11 CI](../../../../docs/jp/design/14_基本設計書_開発基盤.md#11-ci)|
|B-641|`Dockerfile`、`Dockerfile.caddy`|`release.yml`、開発機（`ft` のイメージの手元のビルド）|[第14章 5.4 イメージ](../../../../docs/jp/design/14_基本設計書_開発基盤.md#54-イメージ)、[第13章 3 配布物（DD-13-2）](../../../../docs/jp/design/13_基本設計書_配布と更新.md#3-配布物dd-13-2)、全体実装設計書 7.1|
|B-642|`tools/`（11 個 + 試験）|CI の各 job、開発機、試験設計書の記録|全体実装設計書 6.3|
|B-643|`README`、`LICENSE`、`NOTICE`、`SECURITY.md`、`CHANGELOG.md`、`LICENSES/`、`REUSE.toml`、`.github/*`|公開リポジトリ|[第13章 7 公開リポジトリの文書（DD-13-1、DD-13-9）](../../../../docs/jp/design/13_基本設計書_配布と更新.md#7-公開リポジトリの文書dd-13-1dd-13-9)、[第14章 10 リポジトリの構成](../../../../docs/jp/design/14_基本設計書_開発基盤.md#10-リポジトリの構成)|
|B-644|THIRD_PARTY_NOTICES（イメージ、ウェブ、APK）|イメージ（`/srv/www/about/`）、web-ui（このアプリについて）、and-shell（`ShellSettings`）|[第14章 9.2 道具](../../../../docs/jp/design/14_基本設計書_開発基盤.md#92-道具)|

## アルゴリズム（差し替え点）

|事項|実装|切り替え|由来|
|---|---|---|---|
|再現性の確認|同じコミットを 2 回（別のランナー）で組み、イメージの digest・`ft` の sha256・未署名 APK の中身を比較|固定|[第14章 11.2 リリース（タグ `vX.Y.Z` の push。2 段）](../../../../docs/jp/design/14_基本設計書_開発基盤.md#112-リリースタグ-vxyz-の-push2-段)|
|イメージの署名|cosign の鍵なし（OIDC）。NRSD の鍵は不要|固定|[第13章 3 配布物（DD-13-2）](../../../../docs/jp/design/13_基本設計書_配布と更新.md#3-配布物dd-13-2)|
|APK の署名|CI の外（PKCS#11）。CI は試験の鍵で署名した版も出す（L4・L5 用。指紋は version.json の草案に試験の鍵として）|固定|[第14章 11.5 Android の署名鍵の扱い](../../../../docs/jp/design/14_基本設計書_開発基盤.md#115-android-の署名鍵の扱い)、全体実装設計書 7.2|
|arm64|ネイティブのランナー。無ければ QEMU（遅い。一覧の赤にしない）|ランナーの有無|試験設計書 E-3|
|禁止語の一覧|Git の外（CI は secret として渡す）|固定|[第14章 14 書き方の取り決め](../../../../docs/jp/design/14_基本設計書_開発基盤.md#14-書き方の取り決め)|

## データ設計（このブロックが形を持つ記録）

|記録|形|欄|由来|
|---|---|---|---|
|`.github/workflows/ci.yml`、`release.yml`、`periodic.yml`|GitHub Actions（SHA 固定）|上の図の job|[第14章 11 CI](../../../../docs/jp/design/14_基本設計書_開発基盤.md#11-ci)|
|`dependabot.yml`、`CODEOWNERS`|—|第14章 11.4、10|[第14章 11.4 ワークフローの安全](../../../../docs/jp/design/14_基本設計書_開発基盤.md#114-ワークフローの安全)|
|`tools/known_red.txt`|テキスト|1 行：`<試験の名前> <無い物>`（NRSD の物が無い間だけ）|試験設計書 第1章 4.2|
|`dev/generated/fixtures/`|JSON|L1 が書き出す（全体実装設計書 5.4）。コミットする|全体実装設計書 5.4|
|`version.json` の草案|`ft.release/1`|前段が作り、後段が digest を埋める。`min_app`（web、android）は、過去の Release のうち今回の公開日の 12 か月前より後に出た最も古い版（無ければ今回の版。第13章 6.2）。`signing_cert_sha256` は NRSD の鍵の指紋（README の値）だけ。試験の鍵で署名した APK 用の `dev/generated/version.test.json` は CI が別に作る（L4・L5。S-16）|[第13章 3 配布物（DD-13-2）](../../../../docs/jp/design/13_基本設計書_配布と更新.md#3-配布物dd-13-2)|
|SBOM|CycloneDX 1.7|イメージ、`ft`、APK ごと|[第14章 9.2 道具](../../../../docs/jp/design/14_基本設計書_開発基盤.md#92-道具)|
