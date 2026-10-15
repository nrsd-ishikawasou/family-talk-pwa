// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package keys

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/deps"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ratelimit"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 本ブロックの操作を本物の srv-boundary に通す試験。応答は api の spec で検証される（ValidateResponses）。エラーコード表の番号ごとの
// 試験（TestErr_KEY_*。tools/errors_check が L1 に求める名）もここに置く。タグ rooms の経路（/rooms/{id}/mls/*）でだけ出る KEY-007 と
// KEY-013 は、受けの srv-rooms がまだ無いので、その経路が呼ぶ Groups を直に呼ぶ。

const sessionCookieName = "__Host-session"

type otherImpl struct{ gen.StrictServerInterface }

type noUpWs struct{}

func (noUpWs) Verify(context.Context, string) (uuid.UUID, *fault.Event) {
	return uuid.Nil, fault.New(cgen.AUTH001, nil)
}

type noRooms struct{}

func (noRooms) IsMember(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil }
func (noRooms) RoomRole(context.Context, uuid.UUID, uuid.UUID) (boundary.RoomRole, error) {
	return boundary.RoomRoleNone, nil
}

type noLinks struct{}

func (noLinks) Snapshot(context.Context, uuid.UUID) (gen.SyncSnapshot, int64, *fault.Event) {
	return gen.SyncSnapshot{Rooms: []gen.RoomSummary{}, Users: []gen.User{}, Devices: []gen.Device{}}, 0, nil
}
func (noLinks) ForFeed(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	return map[uuid.UUID]json.RawMessage{}, nil
}
func (noLinks) Set(uuid.UUID, uuid.UUID, bool, time.Duration)                    {}
func (noLinks) Pending(context.Context, uuid.UUID) ([]boundary.UpMessage, error) { return nil, nil }
func (noLinks) Ack(context.Context, uuid.UUID, uuid.UUID) error                  { return nil }
func (noLinks) Probe(context.Context) (boundary.HealthFacts, error) {
	return boundary.HealthFacts{CaddyCertDays: 60, DiskFreePct: 50, Mode: "A"}, nil
}

// web は HTTP の家族のサーバー（httptest）。
type web struct {
	e   *env
	srv *httptest.Server
}

// serve は e の本ブロックと srv-identity を srv-boundary に組み込んで HTTP で受ける。
func (e *env) serve() *web {
	e.t.Helper()
	dir := e.t.TempDir()
	e.must(os.MkdirAll(filepath.Join(dir, "dl"), 0o700))
	e.must(os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<script nonce="__FT_NONCE__"></script>`), 0o600))
	e.must(os.WriteFile(filepath.Join(dir, "dl", "version.json"), []byte(`{"v":1,"server":{"version":"1.2.3","released_at":"2026-10-01T00:00:00Z"},"boundary":"0.1.0","min_app":{"web":"1.0.0","android":"1.0.0"}}`), 0o600))
	cfg, err := config.Load(map[string]string{"FT_DOMAIN": "family.example", "FT_DNS_PROVIDER": "test", "FT_DATA_DIR": e.t.TempDir()}, nil, cgen.Items)
	e.must(err)
	ix := ids.New(e.clk, rand.Reader)
	base := boundary.Base{Config: cfg, Deps: deps.Deps{Clock: e.clk}, Log: logx.New(io.Discard, "api", logx.LevelInfo, e.clk.Now),
		Limiter: ratelimit.New(e.clk, cgen.Policies), Ids: ix, Pool: e.pool, Feed: store.NewFeed(e.pool), Notifier: store.NewNotifier(e.pool, e.clk),
		Idempotency: store.NewIdempotency(e.pool, e.clk), Jobs: store.NewJobs(e.pool, ix, e.clk), WwwDir: dir, ValidateResponses: true}
	v := e.id.Verifiers()
	apis := e.id.Apis()
	other := otherImpl{}
	impls := gen.Implementations{Setup: apis.Setup, Invites: apis.Invites, Auth: apis.Auth, Me: apis.Me, Family: apis.Family, Users: apis.Users,
		Devices: apis.Devices, Keys: e.k.Apis(), Rooms: other, Media: other, Calls: other, Push: other, Admin: other, Shell: other}
	h, err := boundary.NewServer().Build(base, impls, boundary.Verifiers{Session: v.Session, Shell: v.Shell, Ws: v.Ws, UpWs: noUpWs{},
		Membership: noRooms{}, State: fakeState{}}, boundary.Links{Snapshot: noLinks{}, Events: noLinks{}, Typing: noLinks{}, Up: noLinks{}, Probe: noLinks{}})
	e.must(err)
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return &web{e: e, srv: srv}
}

// cookie は端末のセッションの Cookie の値（reauth なら再認証の済んだセッション）。
func (e *env) cookie(user uuid.UUID, dk *devKeys, reauth bool) string {
	e.t.Helper()
	var c string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		c, _, err = e.id.Sessions.Issue(e.ctx, tx, user, dk.deviceId, reauth)
		return err
	}))
	return c
}

// do は端末の要求を送り、状態と本文を返す。
func (w *web) do(method, path, cookie string, body any, hdr map[string]string) (*http.Response, []byte) {
	w.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		w.e.must(err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rd)
	w.e.must(err)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	}
	req.Header.Set("X-Requested-With", "family-talk")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	w.e.must(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// problem は problem+json の番号と状態を確かめる。
func (w *web) problem(resp *http.Response, body []byte, status int, code string) map[string]any {
	w.e.t.Helper()
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil || resp.StatusCode != status || p["code"] != code {
		w.e.t.Fatalf("%d %s を期待したが %d %s", status, code, resp.StatusCode, body)
	}
	return p
}

// ok は状態 status を確かめて本文を返す。
func (w *web) ok(status int, method, path, cookie string, body any, hdr map[string]string) map[string]any {
	w.e.t.Helper()
	resp, b := w.do(method, path, cookie, body, hdr)
	if resp.StatusCode != status {
		w.e.t.Fatalf("%s %s が %d でない: %d %s", method, path, status, resp.StatusCode, b)
	}
	var m map[string]any
	if len(b) > 0 {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func idemKey() map[string]string {
	return map[string]string{"Idempotency-Key": uuid.Must(uuid.NewV7()).String()}
}

// httpFamily は HTTP の家族のサーバーと、承認した 2 人を作る。
func httpFamily(t *testing.T) (*env, *web, *member, *member) {
	t.Helper()
	t.Parallel()
	e := newEnv(t)
	w := e.serve()
	return e, w, e.member("花子", boundary.RoleAdmin), e.member("太郎", boundary.RoleMember)
}

// R: R-12-3-2
// 鍵の運搬を HTTP で通す（第2章 5.4。応答は spec で検証）：新しい端末の鍵の登録、自己承認でない承認（再認証）、KeyPackage の補充と取り出し、
// コピーの置き場、メールボックスの投入と受け取り、鍵のバックアップ、家族に頼む求めと片の取得と承認。
func TestHttpKeyTransport(t *testing.T) {
	e, w, a, b := httpFamily(t)
	ca := e.cookie(a.id, a.dev(), false)
	caR := e.cookie(a.id, a.dev(), true)
	nd := e.addDevice(a, "花子の新しい電話", false)
	f := w.ok(200, "GET", "/api/v1/me/devices", ca, nil, nil)
	if items, _ := f["items"].([]any); len(items) != 2 {
		t.Fatalf("端末の一覧と鍵: %v", f)
	}
	ap := map[string]any{"user_sig": b64.EncodeToString(userSigFor(t, a.usk, nd))}
	resp, body := w.do("POST", "/api/v1/me/devices/"+nd.deviceId.String()+"/approve", ca, ap, nil)
	w.problem(resp, body, 403, cgen.AUTH006)
	w.ok(200, "POST", "/api/v1/me/devices/"+nd.deviceId.String()+"/approve", caR, ap, nil)
	kps := []string{b64.EncodeToString(e.kp(nd, kpOpts{}))}
	last := b64.EncodeToString(e.kp(nd, kpOpts{}))
	cn := e.cookie(a.id, nd, false)
	r := w.ok(200, "POST", "/api/v1/me/devices/"+nd.deviceId.String()+"/keypackages", cn, map[string]any{"key_packages": kps, "last_resort": last}, nil)
	if r["count"] != float64(1) {
		t.Fatalf("keypackages: %v", r)
	}
	cb := e.cookie(b.id, b.dev(), false)
	w.ok(200, "POST", "/api/v1/users/"+a.id.String()+"/devices/"+nd.deviceId.String()+"/keypackage", cb, map[string]any{}, nil)
	self := a.dev().deviceId.String()
	w.ok(200, "PUT", "/api/v1/me/keybundle/device/"+self, ca, copyOf(a.id, BundleDevice, self, 1, 0), nil)
	g := w.ok(200, "GET", "/api/v1/me/keybundle/device/"+self, ca, nil, nil)
	if g["kind"] != "device" {
		t.Fatalf("keybundle: %v", g)
	}
	ct := []byte("key-request")
	in := map[string]any{"kind": "key_request", "ciphertext": b64.EncodeToString(ct),
		"sig": b64.EncodeToString(signRS(t, a.dev().sig, inboxMessage(InboxKeyRequest, a.dev().deviceId, b.dev().deviceId, ct)))}
	w.ok(200, "POST", "/api/v1/devices/"+b.dev().deviceId.String()+"/inbox", ca, in, idemKey())
	resp, body = w.do("POST", "/api/v1/devices/"+b.dev().deviceId.String()+"/inbox", ca, map[string]any{"kind": "share", "ciphertext": "AA", "sig": "AA"}, idemKey())
	w.problem(resp, body, 400, cgen.NET001)
	ib := w.ok(200, "GET", "/api/v1/me/inbox", cb, nil, nil)
	if items, _ := ib["items"].([]any); len(items) != 1 {
		t.Fatalf("inbox: %v", ib)
	}
	gid := groupIdOf(uuid.Must(uuid.NewV7()), 0)
	hist := gen.FkbHistory1{V: 1, Alg: "A256GCM", UserId: a.id.String(), GroupId: b64.EncodeToString(gid), Epoch: 1, KeyVersion: 1,
		Nonce: b64.EncodeToString(make([]byte, 12)), Ciphertext: b64.EncodeToString([]byte{1}), CreatedAt: e.clk.Now()}
	w.ok(201, "PUT", "/api/v1/me/keybackup/"+b64.EncodeToString(gid)+"/1", ca, hist, nil)
	w.ok(204, "PUT", "/api/v1/me/keybackup/"+b64.EncodeToString(gid)+"/1", ca, hist, nil)
	// 家族に頼む：b の片を a の端末と c の端末が持つ。
	c := e.member("次郎", boundary.RoleMember)
	for i, dk := range []*devKeys{a.dev(), c.dev()} {
		target := dk.deviceId.String()
		e.must(e.putCopy(b.id, b.dev(), BundleShare, target, copyOf(b.id, BundleShare, target, 1, i+1)))
	}
	bn := e.addDevice(b, "太郎の新しい電話", false)
	rq := w.ok(200, "POST", "/api/v1/me/recovery/request", e.cookie(b.id, bn, false), map[string]any{}, idemKey())
	rid, _ := rq["request_id"].(string)
	w.ok(200, "GET", "/api/v1/recovery/"+rid+"/share", ca, nil, nil)
	sct := []byte("share")
	w.ok(200, "POST", "/api/v1/recovery/"+rid+"/approve", caR, map[string]any{"ciphertext": b64.EncodeToString(sct),
		"sig": b64.EncodeToString(signRS(t, a.dev().sig, inboxMessage(InboxShare, a.dev().deviceId, bn.deviceId, sct)))}, nil)
}

// TestErr_KEY_001 は端末がない、または未登録の公開鍵（404。KeyPackage の無い端末の取り出し）。
func TestErr_KEY_001(t *testing.T) {
	e, w, a, b := httpFamily(t)
	resp, body := w.do("POST", "/api/v1/users/"+a.id.String()+"/devices/"+a.dev().deviceId.String()+"/keypackage", e.cookie(b.id, b.dev(), false), map[string]any{}, nil)
	w.problem(resp, body, 404, cgen.KEY001)
}

// TestErr_KEY_002 は承認済みの端末（409。承認済みの端末からの回復の求め）。
func TestErr_KEY_002(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	resp, body := w.do("POST", "/api/v1/me/recovery/request", e.cookie(a.id, a.dev(), false), map[string]any{}, idemKey())
	w.problem(resp, body, 409, cgen.KEY002)
}

// TestErr_KEY_003 は user_sig が本人の署名鍵で通らない（403。自己承認）。
func TestErr_KEY_003(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	nd := e.addDevice(a, "花子の新しい電話", false)
	resp, body := w.do("POST", "/api/v1/me/devices/"+nd.deviceId.String()+"/approve", e.cookie(a.id, nd, false),
		map[string]any{"user_sig": b64.EncodeToString(make([]byte, 64))}, nil)
	w.problem(resp, body, 403, cgen.KEY003)
}

// TestErr_KEY_004 は未承認の端末（403。鍵のバックアップの一覧）。
func TestErr_KEY_004(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	nd := e.addDevice(a, "花子の新しい電話", false)
	resp, body := w.do("GET", "/api/v1/me/keybackup", e.cookie(a.id, nd, false), nil, nil)
	w.problem(resp, body, 403, cgen.KEY004)
}

// TestErr_KEY_005 は鍵バンドルのコピーがない（404。params.kind）。
func TestErr_KEY_005(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	resp, body := w.do("GET", "/api/v1/me/keybundle/device/"+a.dev().deviceId.String(), e.cookie(a.id, a.dev(), false), nil, nil)
	if p := w.problem(resp, body, 404, cgen.KEY005); p["params"].(map[string]any)["kind"] != "device" {
		t.Fatalf("params.kind: %s", body)
	}
}

// TestErr_KEY_006 は最後の手段の KeyPackage を返した警告（200 と warnings）。
func TestErr_KEY_006(t *testing.T) {
	e, w, a, b := httpFamily(t)
	last := buildKeyPackage(t, a.dev(), e.clk.Now(), kpOpts{})
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.KeyPackages.Push(e.ctx, tx, a.dev().deviceId, nil, last)
		return err
	}))
	r := w.ok(200, "POST", "/api/v1/users/"+a.id.String()+"/devices/"+a.dev().deviceId.String()+"/keypackage", e.cookie(b.id, b.dev(), false), map[string]any{}, nil)
	ws, _ := r["warnings"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["code"] != cgen.KEY006 || r["key_package"] != b64.EncodeToString(last) {
		t.Fatalf("警告: %v", r)
	}
}

// TestErr_KEY_007 は Commit の epoch が現在と一致しない（409。/rooms/{id}/mls/commit の受けの Groups.Commit）。
func TestErr_KEY_007(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	room := e.room(a.dev(), a)
	raw := buildCommit(t, commitSpec{groupId: groupIdOf(room, 0), epoch: 3, sender: 0, path: pathLeaf(t, a.dev(), groupIdOf(room, 0), 0, nil)})
	err := e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Commit(e.ctx, tx, room, a.dev().deviceId, 3, raw, nil)
		return err
	})
	if ev := e.wantCode(err, cgen.KEY007); ev.Status != 409 || !ev.Retryable {
		t.Fatalf("KEY-007: %+v", ev)
	}
}

// TestErr_KEY_008 は回復コードの試行の超過（429。policy recovery の 10 回/日）。
func TestErr_KEY_008(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	nd := e.addDevice(a, "花子の新しい電話", false)
	c := e.cookie(a.id, nd, false)
	body := map[string]any{"verifier": b64.EncodeToString(make([]byte, 32))}
	for range 10 {
		resp, b := w.do("POST", "/api/v1/me/keybundle/recovery-code/verify", c, body, nil)
		w.problem(resp, b, 404, cgen.KEY005)
	}
	resp, b := w.do("POST", "/api/v1/me/keybundle/recovery-code/verify", c, body, nil)
	w.problem(resp, b, 429, cgen.KEY008)
}

// TestErr_KEY_009 は形の誤り（422。credential が第12章 5.3 の形でない端末の鍵の登録）。
func TestErr_KEY_009(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	nd := e.addDevice(a, "花子の新しい電話", false)
	resp, body := w.do("PUT", "/api/v1/me/devices/"+nd.deviceId.String()+"/keys", e.cookie(a.id, nd, false), map[string]any{
		"sig_pub": jwkOf(t, newKey(t)), "kem_pub": jwkOf(t, newKey(t)), "mls_credential": b64.EncodeToString(credentialFor(a.id, a.dev().deviceId))}, nil)
	w.problem(resp, body, 422, cgen.KEY009)
}

// TestErr_KEY_010 は端末宛メールボックスの上限（413。1,000 件）。
func TestErr_KEY_010(t *testing.T) {
	e, w, a, b := httpFamily(t)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO device_inbox (id, to_device, from_device, kind, ciphertext, sig, created_at, expires_at)
SELECT gen_random_uuid(), $1, $2, 'key_request', '\x00', decode(repeat('00', 64), 'hex'), $3, $4 FROM generate_series(1, 1000)`,
		b.dev().deviceId, a.dev().deviceId, e.clk.Now(), e.clk.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ct := []byte("x")
	resp, body := w.do("POST", "/api/v1/devices/"+b.dev().deviceId.String()+"/inbox", e.cookie(a.id, a.dev(), false), map[string]any{"kind": "key_request",
		"ciphertext": b64.EncodeToString(ct), "sig": b64.EncodeToString(signRS(t, a.dev().sig, inboxMessage(InboxKeyRequest, a.dev().deviceId, b.dev().deviceId, ct)))}, idemKey())
	w.problem(resp, body, 413, cgen.KEY010)
}

// TestErr_KEY_011 は鍵バンドルの版が古い（409。params.version）。
func TestErr_KEY_011(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	self := a.dev().deviceId.String()
	resp, body := w.do("PUT", "/api/v1/me/keybundle/device/"+self, e.cookie(a.id, a.dev(), false), copyOf(a.id, BundleDevice, self, 2, 0), nil)
	if p := w.problem(resp, body, 409, cgen.KEY011); p["params"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("params.version: %s", body)
	}
}

// TestErr_KEY_012 は回復コードの照合の値が合わない（403。再試行は可）。
func TestErr_KEY_012(t *testing.T) {
	e, w, a, _ := httpFamily(t)
	e.must(e.tx(func(tx *store.Tx) error {
		return e.k.RecoveryCode.PutVerifier(e.ctx, tx, a.id, a.dev().deviceId, make([]byte, 32), RecoveryParams{M: 65536, T: 3, P: 4, Salt: a.id.Bytes()})
	}))
	nd := e.addDevice(a, "花子の新しい電話", false)
	resp, body := w.do("POST", "/api/v1/me/keybundle/recovery-code/verify", e.cookie(a.id, nd, false),
		map[string]any{"verifier": b64.EncodeToString(bytes.Repeat([]byte{1}, 32))}, nil)
	if p := w.problem(resp, body, 403, cgen.KEY012); p["retryable"] != true {
		t.Fatalf("retryable: %s", body)
	}
}

// TestErr_KEY_013 は再初期化を許さない（409。/rooms/{id}/mls/reinit の受けの Groups.Reinit。再参加待ちでない）。
func TestErr_KEY_013(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.member("花子", boundary.RoleAdmin)
	room := e.room(a.dev(), a)
	err := e.tx(func(tx *store.Tx) error {
		_, err := e.k.Groups.Reinit(e.ctx, tx, room, a.dev().deviceId)
		return err
	})
	if ev := e.wantCode(err, cgen.KEY013); ev.Status != 409 || ev.Params["reason"] != "not_pending" {
		t.Fatalf("KEY-013: %+v", ev)
	}
}
