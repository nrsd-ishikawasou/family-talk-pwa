// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package deps は外と触れる差し替え点（srv-common B-009、全体実装設計書 2.8）。
// interface は本 package が定め、本物は本 package（B-010）、偽物は各試験が持つ。
// 外と触れる操作は先頭に context.Context を取る（取り消しと時間切れ）。
package deps

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
)

// Deps は差し替え点の全部。cmd/ft が本物を入れ、試験が偽物を入れる。
type Deps struct {
	Clock   clock.Clock
	Rand    Rand
	Push    PushTransport
	Fetch   Fetcher
	Dns     DnsProvider
	Restic  Restic
	Docker  Docker
	Luks    Luks
	Systemd Systemd
	Release ReleaseSource
	Sfu     SfuAdmin
	Maps    MapSource
}

// Rand は乱数。本物は crypto/rand、試験は決まった列（UUIDv7 と token の再現）。
type Rand interface {
	io.Reader
}

// PushResponse はプッシュ配信サービスの応答（RFC 8030。201、404、410、429 と Retry-After、5xx）。
type PushResponse struct {
	Status     int
	RetryAfter time.Duration
}

// PushTransport はプッシュ配信サービスへの POST（暗号化と VAPID の署名は srv-push）。
type PushTransport interface {
	Post(ctx context.Context, endpoint string, body []byte, header http.Header) (PushResponse, error)
}

// FetchLimits は外の URL の取得の上限（第1章 4.2：接続 5 秒、全体 10 秒、応答 1 MiB、転送 3 回）。
type FetchLimits struct {
	Connect      time.Duration
	Total        time.Duration
	MaxBytes     int64
	MaxRedirects int
}

// FetchResponse は取得の結果。
type FetchResponse struct {
	Status   int
	Header   http.Header
	Body     []byte
	FinalUrl string
}

// Fetcher は外の URL の取得。転送を 1 段ずつ追い、各段で内部のアドレス帯を拒む（MEDIA-010）。
type Fetcher interface {
	Get(ctx context.Context, url string, limits FetchLimits) (FetchResponse, error)
}

// DnsProvider は DNS の提供者の API（DDNS）。
type DnsProvider interface {
	SetA(ctx context.Context, name string, ip netip.Addr) error
	SetAAAA(ctx context.Context, name string, ip netip.Addr) error
}

// ResticRepo は restic のリポジトリ（置き場とパスワード。S3 互換の時は接続の値）。
type ResticRepo struct {
	Url      string
	Password []byte
	Env      map[string]string
}

// ForgetPolicy は保持の規則（日次、週次、月次の数。第11章 9.1）。
type ForgetPolicy struct {
	Daily   int
	Weekly  int
	Monthly int
	Prune   bool
}

// Snapshot は restic の 1 つのスナップショット。
type Snapshot struct {
	Id    string
	Time  time.Time
	Paths []string
	Tags  []string
}

// BackupInput は 1 回のバックアップの入力。Paths（ファイルの道のり）か、StdinCmd（標準出力を保存する命令）の
// どちらか一方。restic の --stdin-from-command は同じ実行で道のりと一緒に使えない（restic v0.19.1）。
type BackupInput struct {
	Paths         []string
	StdinCmd      []string
	StdinFilename string
	Tags          []string
}

// BackupResult は 1 回のバックアップの結果。
type BackupResult struct {
	SnapshotId string
	AddedBytes int64
}

// Restic は同梱の restic を呼ぶ（第14章 5.4。Go の部品として組み込めない）。
type Restic interface {
	Backup(ctx context.Context, repo ResticRepo, input BackupInput) (BackupResult, error)
	Forget(ctx context.Context, repo ResticRepo, policy ForgetPolicy) error
	Check(ctx context.Context, repo ResticRepo, subset string) error
	Restore(ctx context.Context, repo ResticRepo, snapshot, dest string) error
	KeyAdd(ctx context.Context, repo ResticRepo, newPassword []byte) (string, error)
	KeyRemove(ctx context.Context, repo ResticRepo, id string) error
	Snapshots(ctx context.Context, repo ResticRepo) ([]Snapshot, error)
}

// ImageInfo はイメージの digest と作成の時刻。
type ImageInfo struct {
	Digest  string
	Created time.Time
}

// ExecResult はコンテナの中の命令の結果。
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Docker は Docker と Compose の操作（dir は compose.yaml のある /etc/family-talk）。
// ホストの ft だけが使う。コンテナは Docker を操作しない（第11章 3.4）。
type Docker interface {
	ComposeUp(ctx context.Context, dir string) error
	ComposeStop(ctx context.Context, dir, service string) error
	ComposeRecreate(ctx context.Context, dir string, services []string) error
	ComposeDown(ctx context.Context, dir string) error
	Pull(ctx context.Context, ref string) (string, error)
	Inspect(ctx context.Context, ref string) (ImageInfo, error)
	Exec(ctx context.Context, service string, cmd []string) (ExecResult, error)
}

// Luks は /data の LUKS2（第12章 6.1）。
type Luks interface {
	Format(ctx context.Context, dev string) ([]byte, error)
	EnrollTpm2(ctx context.Context, dev string) error
	Erase(ctx context.Context, dev string) error
}

// Systemd は systemd の drop-in と有効化。
type Systemd interface {
	WriteDropIn(ctx context.Context, unit, name, text string) error
	Enable(ctx context.Context, unit string) error
}

// VersionJson は取得した version.json のバイト列。型への読み込みは srv-admin が openapi の生成ファイルで行う。
type VersionJson []byte

// ReleaseSource は配布物の取得と検証（第13章）。
type ReleaseSource interface {
	VersionJson(ctx context.Context) (VersionJson, error)
	FetchFt(ctx context.Context, arch string, sha256 [32]byte, dest string) error
	VerifyImage(ctx context.Context, digest string) error
}

// SfuAdmin は SFU の管理の操作。SFU は設定と鍵を読み直せないので、読み直しの操作は持たない
// （再起動はホストの要求の受け口。第11章 3.4）。
type SfuAdmin interface {
	DeleteRoom(ctx context.Context, callId uuid.UUID) error
}

// MapSource は地図のデータの供給元から領域を切り出して dest に書く（PMTiles。第2章 8、第5章 8.1）。
type MapSource interface {
	Extract(ctx context.Context, region, dest string) error
}
