// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package boundary

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gofrs/uuid/v5"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/deps"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

var itPool *store.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	c, err := tcpg.Run(ctx, "postgres:18", tcpg.WithDatabase(store.DbName), tcpg.WithUsername(store.DbUser),
		tcpg.WithPassword("test-pw"), tcpg.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres を起こせない:", err)
		os.Exit(1)
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "5432/tcp")
	p, _ := strconv.Atoi(port.Port())
	itPool, err = store.Open(ctx, store.ConnInfo{Host: host, Port: uint16(p), Database: store.DbName, User: store.DbUser, Password: []byte("test-pw")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "接続できない:", err)
		os.Exit(1)
	}
	if _, err := store.NewMigrator(itPool).Apply(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "移行できない:", err)
		os.Exit(1)
	}
	if _, err := itPool.Exec(ctx, `CREATE TABLE boundary_probe (n int)`); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	itPool.Close()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

// fakeImpl は業務の操作の偽物。使わない操作は埋め込みの nil の interface（呼べば panic、Recover が SRV-003）。
type fakeImpl struct {
	gen.StrictServerInterface
	pool  *store.Pool
	calls atomic.Int32
}

// RequestRecovery は ctx のトランザクションに 1 行を書く（冪等の middleware のトランザクションに乗る）。
func (f *fakeImpl) RequestRecovery(ctx context.Context, _ gen.RequestRecoveryRequestObject) (gen.RequestRecoveryResponseObject, error) {
	f.calls.Add(1)
	if err := f.pool.InTx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO boundary_probe VALUES (1)`)
		return err
	}); err != nil {
		return nil, err
	}
	time.Sleep(50 * time.Millisecond) // 同時の再送が Begin で待つ間を作る
	return gen.RequestRecovery200JSONResponse{RequestId: "01890a5d-ac96-774b-bcce-b302099a8077", ExpiresAt: testNow}, nil
}

// UseFamilyRecoveryKey は要求の本文を持たない（第2章 5.4）。試験では呼ばない（応答のコピー (d) は srv-keys が作る）。
func (f *fakeImpl) UseFamilyRecoveryKey(context.Context, gen.UseFamilyRecoveryKeyRequestObject) (gen.UseFamilyRecoveryKeyResponseObject, error) {
	return gen.UseFamilyRecoveryKey200JSONResponse{}, nil
}

// RunAdminBackup は管理者でなければ届かない（ADM-001）。読み取り専用の門を通ったことだけを見る。
func (f *fakeImpl) RunAdminBackup(context.Context, gen.RunAdminBackupRequestObject) (gen.RunAdminBackupResponseObject, error) {
	return nil, fault.New(cgen.ADM003, nil)
}

type mutableState struct {
	mu     sync.Mutex
	s      ServerState
	reason string
}

func (m *mutableState) State() ServerState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}
func (m *mutableState) ReadOnlyReason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reason
}
func (m *mutableState) set(s ServerState, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s, m.reason = s, reason
}

// 偽の Links。
type fakeSnapshot struct{}

func (fakeSnapshot) Snapshot(context.Context, uuid.UUID) (gen.SyncSnapshot, int64, *fault.Event) {
	return gen.SyncSnapshot{Rooms: []gen.RoomSummary{}, Users: []gen.User{}, Devices: []gen.Device{}}, 0, nil
}

type fakeTyping struct{}

func (fakeTyping) Set(uuid.UUID, uuid.UUID, bool, time.Duration) {}

type fakeUp struct{}

func (fakeUp) Pending(context.Context, uuid.UUID) ([]UpMessage, error) { return nil, nil }
func (fakeUp) Ack(context.Context, uuid.UUID, uuid.UUID) error         { return nil }

type fakeProbe struct{}

func (fakeProbe) Probe(context.Context) (HealthFacts, error) {
	return HealthFacts{CaddyCertDays: 60, DiskFreePct: 50, Mode: "A"}, nil
}

type fakeEvents struct{}

func (fakeEvents) ForFeed(_ context.Context, _, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	out := map[uuid.UUID]json.RawMessage{}
	for _, id := range ids {
		out[id] = json.RawMessage(`{"id":"` + id.String() + `"}`)
	}
	return out, nil
}

type itEnv struct {
	srv    *httptest.Server
	impl   *fakeImpl
	state  *mutableState
	base   Base
	user   uuid.UUID
	dev    uuid.UUID
	cancel context.CancelFunc
	server *Server
}

// newItEnv は Build で全体を組み、hub を回し、httptest のサーバーで受ける。利用者は試験ごとに新しくする。
func newItEnv(t *testing.T) *itEnv {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dl"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<script nonce="__FT_NONCE__"></script>`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "dl", "version.json"), []byte(`{"v":1,"server":{"version":"1.2.3","released_at":"2026-10-01T00:00:00Z"},"boundary":"0.1.0","min_app":{"web":"1.0.0","android":"1.0.0"}}`), 0o600)
	cfg, err := config.Load(map[string]string{"FT_DOMAIN": testHost, "FT_DNS_PROVIDER": "test", "FT_METRICS": "true"}, nil, cgen.Items)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.WallClock{}
	base := Base{Config: cfg, Deps: deps.Deps{Clock: clk}, Log: logx.New(io.Discard, "api", logx.LevelInfo, clk.Now),
		Limiter: ratelimit.New(clk, cgen.Policies), Ids: ids.New(clk, rand.Reader), Pool: itPool,
		Feed: store.NewFeed(itPool), Notifier: store.NewNotifier(itPool, clk), Idempotency: store.NewIdempotency(itPool, clk),
		Jobs: store.NewJobs(itPool, ids.New(clk, rand.Reader), clk), WwwDir: dir, ValidateResponses: true}
	user := uuid.Must(uuid.NewV7())
	dev := uuid.Must(uuid.NewV7())
	pr := Principal{Kind: KindUser, UserId: &user, DeviceId: &dev, Role: RoleMember, DeviceApproved: true}
	state := &mutableState{s: StateNormal}
	v := Verifiers{Session: fakeSession{p: pr}, Shell: fakeToken{ev: fault.New(cgen.AUTH001, nil)}, Ws: fakeToken{p: pr}, UpWs: fakeUpWs{ev: fault.New(cgen.AUTH001, nil)},
		Membership: fakeMembership{}, State: state}
	impl := &fakeImpl{pool: itPool}
	impls := gen.Implementations{Setup: impl, Invites: impl, Auth: impl, Me: impl, Family: impl, Users: impl, Devices: impl,
		Keys: impl, Rooms: impl, Media: impl, Calls: impl, Push: impl, Admin: impl, Shell: impl}
	s := NewServer()
	h, err := s.Build(base, impls, v, Links{Snapshot: fakeSnapshot{}, Events: fakeEvents{}, Typing: fakeTyping{}, Up: fakeUp{}, Probe: fakeProbe{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Hub().Run(ctx) }()
	srv := httptest.NewServer(h)
	t.Cleanup(func() { cancel(); srv.Close() })
	return &itEnv{srv: srv, impl: impl, state: state, base: base, user: user, dev: dev, cancel: cancel, server: s}
}

// do は端末の要求（Cookie、X-Requested-With、Sec-Fetch-Site）を送る。
func (e *itEnv) do(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "__Host-session", Value: "s"})
	req.Header.Set("X-Requested-With", "family-talk")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// appendFeed は利用者の feed に notice を 1 行書き、コミットで NOTIFY する（他のブロックの書き込みの代わり）。
func (e *itEnv) appendFeed(t *testing.T, kind string) int64 {
	t.Helper()
	return e.appendFeedTo(t, kind, uuid.Nil)
}

// appendFeedTo は宛先の端末を添えて feed に 1 行積む（session.revoked と device.inbox は必須。sync.gap は任意。全体実装設計書 2.4）。
func (e *itEnv) appendFeedTo(t *testing.T, kind string, deviceId uuid.UUID) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := itPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ref := store.FeedRef{Kind: kind, Id: uuid.Must(uuid.NewV7()), DeviceId: deviceId, Ts: testNow}
	switch kind {
	case kindRoomEvent:
		ref.RoomId = roomA
	case "device.inbox":
		ref.Payload = json.RawMessage(`{"count":1}`)
	case kindSessionRevoked:
		ref.Payload = json.RawMessage(`{"reason":"logout"}`)
	case "sync.gap":
		ref.Payload = json.RawMessage(`{"reason":"restore"}`)
	default:
		ref.Payload = json.RawMessage(`{"kind":"capacity","level":"info","text_key":"k","params":{}}`)
	}
	p := e.base.Feed.Append(tx, e.user, ref)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return p.Pos
}

// R: R-11-8-1
func TestIntegrationHealthAndVersion(t *testing.T) {
	e := newItEnv(t)
	resp, body := e.do(t, "GET", "/health", "", nil)
	var h gen.GetHealthResponse
	if err := json.Unmarshal(body, &h); err != nil || resp.StatusCode != 200 {
		t.Fatalf("/health：%d %s", resp.StatusCode, body)
	}
	// SFU は試験の環境に無く、worker の仕事もまだ無いので degraded。postgres は真。
	if h.Status != gen.Degraded || !h.Checks.Postgres || h.Checks.Sfu || h.Checks.Mode != "A" || h.Checks.CaddyCertDays != 60 {
		t.Fatalf("/health の中身が違う：%s", body)
	}
	resp, body = e.do(t, "GET", "/version", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"server":"1.2.3"`) || !strings.Contains(string(body), `"deprecations":[]`) {
		t.Fatalf("/version：%d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "GET", "/metrics", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `ft_http_requests_total{op="getHealth",code="200"} 1`) {
		t.Fatalf("/metrics：%d %s", resp.StatusCode, body)
	}
}

// R: R-11-8-1
func TestIntegrationReadOnly(t *testing.T) {
	e := newItEnv(t)
	e.state.set(StateReadOnly, "migration")
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{}`, map[string]string{"Idempotency-Key": idemKey})
	if resp.StatusCode != 503 || !strings.Contains(string(body), `"SRV-001"`) || !strings.Contains(string(body), `"migration"`) {
		t.Fatalf("読み取り専用で拒まない：%d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/health", "", nil); resp.StatusCode != 503 {
		t.Fatalf("読み取り専用の /health が 503 でない：%d", resp.StatusCode)
	}
	key := uuid.Must(uuid.NewV7()).String()
	resp, body = e.do(t, "POST", "/api/v1/admin/backup", `{}`, map[string]string{"Idempotency-Key": key})
	if resp.StatusCode == 503 {
		t.Fatalf("x-readonly: allow の経路を拒んだ：%s", body)
	}
}

// R: R-2-1-1
func TestIntegrationIdempotentConcurrentRetries(t *testing.T) {
	e := newItEnv(t)
	key := uuid.Must(uuid.NewV7()).String()
	var wg sync.WaitGroup
	bodies := make([]string, 5)
	codes := make([]int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, b := e.do(t, "POST", "/api/v1/me/recovery/request", `{}`, map[string]string{"Idempotency-Key": key})
			codes[i], bodies[i] = resp.StatusCode, string(b)
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 200 || bodies[i] != bodies[0] {
			t.Fatalf("%d 番目の応答が違う：%d %s / %s", i, codes[i], bodies[i], bodies[0])
		}
	}
	if n := e.impl.calls.Load(); n != 1 {
		t.Fatalf("操作が %d 回走った", n)
	}
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{"x":1}`, map[string]string{"Idempotency-Key": key})
	if resp.StatusCode != 400 && resp.StatusCode != 409 {
		t.Fatalf("同じ鍵で違う本文が通った：%d %s", resp.StatusCode, body)
	}
}

// R: R-2-2-4
func TestIntegrationSyncCatchUpLongPollAndGone(t *testing.T) {
	e := newItEnv(t)
	first := e.appendFeed(t, "notice")
	e.appendFeed(t, kindRoomEvent)
	resp, body := e.do(t, "GET", "/api/v1/sync?since="+strconv.FormatInt(first-1, 10), "", nil)
	var s gen.GetSyncResponse
	if err := json.Unmarshal(body, &s); err != nil || resp.StatusCode != 200 || s.Events == nil || len(*s.Events) != 2 {
		t.Fatalf("/sync の差分：%d %s", resp.StatusCode, body)
	}
	if (*s.Events)[1]["room_id"] != roomA.String() {
		t.Fatalf("room.event のフレームに room_id が無い：%s", body)
	}
	last := s.Pos
	done := make(chan []byte)
	go func() {
		_, b := e.do(t, "GET", "/api/v1/sync?timeout=10&since="+strconv.Itoa(last), "", nil)
		done <- b
	}()
	time.Sleep(300 * time.Millisecond)
	e.appendFeed(t, "notice")
	select {
	case b := <-done:
		if !strings.Contains(string(b), `"type":"notice"`) {
			t.Fatalf("ロングポールの応答に新しいイベントが無い：%s", b)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ロングポールが通知で起きない")
	}
	// 保持の外：古い行を消すと since が MinPos − 1 より前になり 410。
	if _, err := itPool.Exec(context.Background(), `DELETE FROM feed WHERE user_id = $1 AND pos <= $2`, e.user, first+1); err != nil {
		t.Fatal(err)
	}
	resp, body = e.do(t, "GET", "/api/v1/sync?since=0", "", nil)
	if resp.StatusCode != 410 || !strings.Contains(string(body), `"NET-005"`) {
		t.Fatalf("保持の外が 410 でない：%d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "GET", "/api/v1/sync", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"snapshot"`) {
		t.Fatalf("初回の /sync：%d %s", resp.StatusCode, body)
	}
}

// R: R-2-3-2
func TestIntegrationWebSocketAuthReadyAndCatchUp(t *testing.T) {
	e := newItEnv(t)
	before := e.appendFeed(t, "notice")
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Origin が違えば 403。
	if _, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}}); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("違う Origin を受けた：%v", err)
	}
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://" + testHost}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","token":"t"}`))
	_, data, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"type":"ready"`) || !strings.Contains(string(data), fmt.Sprintf(`"pos":%d`, before)) {
		t.Fatalf("ready が来ない：%s %v", data, err)
	}
	if got := e.server.Hub().Connected(e.user); len(got) != 1 {
		t.Fatalf("接続の表に無い：%v", got)
	}
	after := e.appendFeed(t, kindRoomEvent)
	_, data, err = c.Read(ctx)
	if err != nil || !strings.Contains(string(data), fmt.Sprintf(`"pos":%d`, after)) || !strings.Contains(string(data), `"room_id"`) {
		t.Fatalf("キャッチアップのフレームが来ない：%s %v", data, err)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"unknown"}`))
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != closePolicy {
		t.Fatalf("知らない type で 1008 にならない：%v", err)
	}
}

// R: R-2-2-4
func TestIntegrationSyncDeviceTargetedRowsAndPos(t *testing.T) {
	e := newItEnv(t)
	other := uuid.Must(uuid.NewV7())
	mine := e.appendFeedTo(t, "device.inbox", e.dev)
	e.appendFeedTo(t, "device.inbox", other)
	last := e.appendFeedTo(t, "sync.gap", uuid.Nil)
	resp, body := e.do(t, "GET", "/api/v1/sync?since=0", "", nil)
	var s gen.GetSyncResponse
	if err := json.Unmarshal(body, &s); err != nil || resp.StatusCode != 200 || s.Events == nil || len(*s.Events) != 2 {
		t.Fatalf("他の端末宛ての行を除いた差分でない：%d %s", resp.StatusCode, body)
	}
	if (*s.Events)[0]["pos"] != float64(mine) || (*s.Events)[1]["type"] != "sync.gap" || s.Pos != int(last) {
		t.Fatalf("自分宛てと全端末宛ての行、応答の pos が違う：%s", body)
	}
	// 最新の行が他の端末宛てだけの時も、応答の pos は利用者の最新（第2章 5.9）。
	latest := e.appendFeedTo(t, kindSessionRevoked, other)
	resp, body = e.do(t, "GET", "/api/v1/sync?since="+strconv.FormatInt(last, 10), "", nil)
	if err := json.Unmarshal(body, &s); err != nil || resp.StatusCode != 200 || (s.Events != nil && len(*s.Events) != 0) || s.HasMore || s.Pos != int(latest) {
		t.Fatalf("他の端末宛ての行だけの時の応答：%d %s", resp.StatusCode, body)
	}
}

// R: R-2-3-2
func TestIntegrationWebSocketSessionRevokedCloses4001(t *testing.T) {
	e := newItEnv(t)
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://" + testHost}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","token":"t"}`))
	if _, data, err := c.Read(ctx); err != nil || !strings.Contains(string(data), `"type":"ready"`) {
		t.Fatalf("ready が来ない：%s %v", data, err)
	}
	// 他の端末宛ての session.revoked は届かず、閉じない。
	e.appendFeedTo(t, kindSessionRevoked, uuid.Must(uuid.NewV7()))
	pos := e.appendFeed(t, "notice")
	_, data, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"type":"notice"`) || !strings.Contains(string(data), fmt.Sprintf(`"pos":%d`, pos)) {
		t.Fatalf("他の端末宛ての行の後の notice が来ない：%s %v", data, err)
	}
	// この端末宛ての session.revoked はフレームの後に 4001（第2章 6.3）。
	e.appendFeedTo(t, kindSessionRevoked, e.dev)
	if _, data, err := c.Read(ctx); err != nil || !strings.Contains(string(data), `"type":"session.revoked"`) {
		t.Fatalf("session.revoked が来ない：%s %v", data, err)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != closeRevoked {
		t.Fatalf("session.revoked の後に 4001 で閉じない：%v", err)
	}
	// 表から外れるのは読み手の goroutine が戻った後（閉じた後に少し遅れる）。
	for i := 0; i < 50 && len(e.server.Hub().Connected(e.user)) != 0; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if got := e.server.Hub().Connected(e.user); len(got) != 0 {
		t.Fatalf("閉じた接続が表に残る：%v", got)
	}
}

// expectProblem は応答が code の problem で、状態が status であることを確かめる。
func expectProblem(t *testing.T, resp *http.Response, body []byte, status int, code string) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil || resp.StatusCode != status || p["code"] != code ||
		resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%s（%d）でない：%d %s", code, status, resp.StatusCode, body)
	}
	if def := cgen.Errors[code]; p["retryable"] != (def.Retry == cgen.RetryYes) {
		t.Fatalf("%s の retryable が表と違う：%s", code, body)
	}
	return p
}

// エラーコード表の test の欄の試験（T-L1-13。tools/errors_check が名を照らす）。番号ごとに、起きる条件で本物の経路から起こす。

func TestErr_NET_001(t *testing.T) {
	e := newItEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{"result":"maybe"}`, map[string]string{"Idempotency-Key": uuid.Must(uuid.NewV7()).String()})
	p := expectProblem(t, resp, body, 400, cgen.NET001)
	if errs, _ := p["params"].(map[string]any)["errors"].([]any); len(errs) == 0 {
		t.Fatalf("params.errors[] が空：%s", body)
	}
}

func TestErr_NET_002(t *testing.T) {
	e := newItEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{}`, map[string]string{"Idempotency-Key": uuid.Must(uuid.NewV7()).String(), "Content-Type": "text/plain"})
	expectProblem(t, resp, body, 415, cgen.NET002)
	resp, body = e.do(t, "GET", "/api/v1/sync", "", map[string]string{"Accept": "text/html"})
	expectProblem(t, resp, body, 406, cgen.NET002)
}

func TestErr_NET_003(t *testing.T) {
	e := newItEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{}`, map[string]string{"Idempotency-Key": uuid.Must(uuid.NewV7()).String(), "Sec-Fetch-Site": "cross-site"})
	expectProblem(t, resp, body, 403, cgen.NET003)
}

func TestErr_NET_004(t *testing.T) {
	e := newItEnv(t)
	var resp *http.Response
	var body []byte
	for i := 0; i < 61; i++ { // public：IP あたり 1 分 60 回（第2章 9.3）
		resp, body = e.do(t, "GET", "/version", "", nil)
	}
	p := expectProblem(t, resp, body, 429, cgen.NET004)
	if p["params"].(map[string]any)["policy"] != "public" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("params.policy か Retry-After が違う：%v %s", resp.Header, body)
	}
}

func TestErr_NET_005(t *testing.T) {
	e := newItEnv(t)
	first := e.appendFeed(t, "notice")
	e.appendFeed(t, "notice")
	if _, err := itPool.Exec(context.Background(), `DELETE FROM feed WHERE user_id = $1 AND pos <= $2`, e.user, first); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, "GET", "/api/v1/sync?since=0", "", nil)
	expectProblem(t, resp, body, 410, cgen.NET005)
}

func TestErr_NET_006(t *testing.T) {
	e := newItEnv(t)
	resp, body := e.do(t, "GET", "/api/v1/sync", "", map[string]string{"X-Family-Talk-App": "web/0.9.0"})
	p := expectProblem(t, resp, body, 426, cgen.NET006)
	if p["params"].(map[string]any)["min_app"] != "1.0.0" {
		t.Fatalf("params.min_app が違う：%s", body)
	}
}

func TestErr_NET_007(t *testing.T) {
	e := newItEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{}`, nil)
	expectProblem(t, resp, body, 400, cgen.NET007)
}

func TestErr_NET_008(t *testing.T) {
	e := newItEnv(t)
	key := uuid.Must(uuid.NewV7()).String()
	// 本文の無い空の物体 2 つ（`{}` と `{ }`）。意味は同じでも本文の SHA-256 が違う（第2章 DD-2-5 は本文の一致で見る）。
	if resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{}`, map[string]string{"Idempotency-Key": key}); resp.StatusCode != 200 {
		t.Fatalf("1 回目：%d %s", resp.StatusCode, body)
	}
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{ }`, map[string]string{"Idempotency-Key": key})
	expectProblem(t, resp, body, 409, cgen.NET008)
}

func TestErr_NET_009(t *testing.T) {
	e := newItEnv(t)
	big := `{"pad":"` + strings.Repeat("a", BodyLimitJson) + `"}`
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", big, map[string]string{"Idempotency-Key": uuid.Must(uuid.NewV7()).String()})
	p := expectProblem(t, resp, body, 413, cgen.NET009)
	if p["params"].(map[string]any)["limit"] != float64(BodyLimitJson) {
		t.Fatalf("params.limit が違う：%s", body)
	}
}

func TestErr_SRV_001(t *testing.T) {
	e := newItEnv(t)
	e.state.set(StateReadOnly, "capacity")
	resp, body := e.do(t, "POST", "/api/v1/me/recovery/request", `{}`, map[string]string{"Idempotency-Key": uuid.Must(uuid.NewV7()).String()})
	p := expectProblem(t, resp, body, 503, cgen.SRV001)
	if p["params"].(map[string]any)["reason"] != "capacity" || resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("params.reason か Retry-After が違う：%s", body)
	}
}

func TestErr_SRV_003(t *testing.T) {
	e := newItEnv(t)
	// 偽の業務の操作は getMe を持たない（埋め込みの nil の interface）。呼ぶと panic し、Recover が SRV-003 にする。
	resp, body := e.do(t, "GET", "/api/v1/me", "", nil)
	p := expectProblem(t, resp, body, 500, cgen.SRV003)
	if !strings.HasPrefix(fmt.Sprint(p["instance"]), "urn:uuid:") {
		t.Fatalf("instance が要求の識別子でない：%s", body)
	}
}
