// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"context"
	"net/http"
	"time"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
)

// ロングポールの待ちの上限（第2章 5.9）と、/health の外への確かめの打ち切り。
const (
	LongPollMax  = 30 * time.Second
	ProbeTimeout = 2 * time.Second
)

// SyncHandler は /sync（B-044。gen.SyncApi）。
type SyncHandler struct {
	base  Base
	hub   *Hub
	links Links
}

var _ gen.SyncApi = (*SyncHandler)(nil)

// GetSync は since なしで初回（Snapshotter）、since ありで Feed.ReadSince。保持の外は NET-005。
// timeout > 0 は hub の通知を待つ。待ちの間に Shutdown が来たら 200 と空の配列。
func (h *SyncHandler) GetSync(ctx context.Context, request gen.GetSyncRequestObject) (gen.GetSyncResponseObject, error) {
	panic("未実装")
}

// Health は /health（B-046。第11章 11）。
type Health struct {
	base  Base
	links Links
	state ServerStateReader
}

// Get は checks を集める。外への確かめ（postgres、sfu、HealthProbe）は並行に行い、各 2 秒で打ち切る。
func (h *Health) Get(ctx context.Context) (gen.GetHealthResponse, int) {
	panic("未実装")
}

// VersionHandler は /version（ft.version/1）。min_app は dl/version.json を起動の時に読む。
type VersionHandler struct {
	base Base
}

// Get は server、boundary、min_app、released_at、deprecations を返す。
func (h *VersionHandler) Get() gen.FtVersion1 {
	panic("未実装")
}

// MinApp は platform（web、android）の最低の版。AppVersion の middleware が使う。
func (h *VersionHandler) MinApp(platform string) (string, bool) {
	panic("未実装")
}

// Metrics は /metrics（OpenMetrics。FT_ADMIN_BIND の bind だけ。opt-in の FT_METRICS）。
type Metrics struct {
	base Base
}

// Handler は OpenMetrics の形式で書く handler。
func (m *Metrics) Handler() http.Handler {
	panic("未実装")
}

// serverApi は gen.ServerApi（Health、VersionHandler、Metrics を束ねる）。
type serverApi struct {
	health  *Health
	version *VersionHandler
	metrics *Metrics
}

var _ gen.ServerApi = (*serverApi)(nil)

// GetHealth は 200（ok、degraded、setup）か 503（readonly、停止の理由）を返す。
func (s *serverApi) GetHealth(ctx context.Context, request gen.GetHealthRequestObject) (gen.GetHealthResponseObject, error) {
	panic("未実装")
}

// GetVersion は ft.version/1 を返す。
func (s *serverApi) GetVersion(ctx context.Context, request gen.GetVersionRequestObject) (gen.GetVersionResponseObject, error) {
	panic("未実装")
}

// GetMetrics は OpenMetrics の本文を返す。
func (s *serverApi) GetMetrics(ctx context.Context, request gen.GetMetricsRequestObject) (gen.GetMetricsResponseObject, error) {
	panic("未実装")
}

// InternalAuth は /internal/auth（forward auth。B-047。gen.InternalApi）。
type InternalAuth struct {
	verifiers Verifiers
}

var _ gen.InternalApi = (*InternalAuth)(nil)

// CheckForwardAuth は Cookie が有効なら 200 と X-User-Id、無効なら 401。
func (a *InternalAuth) CheckForwardAuth(ctx context.Context, request gen.CheckForwardAuthRequestObject) (gen.CheckForwardAuthResponseObject, error) {
	panic("未実装")
}

// SpaIndex は画面の入口（/、/r/* など）に、CSP の nonce を埋めた index.html を返す。認証なし。
type SpaIndex struct {
	base Base
}

// ServeHTTP は index.html を返す。
func (s *SpaIndex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	panic("未実装")
}
