// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package rooms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
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

// 本ブロックの 25 操作を本物の srv-boundary に通す試験。応答は api の spec で検証される（ValidateResponses）。エラーコード表の番号ごとの
// 試験（TestErr_ROOM_*。tools/errors_check が L1 に求める名）もここに置く。ROOM-001 は境界の room_member（無いトークも ROOM-002）の
// 後にトークが消えた時に部品が返す番号で、HTTP では作れないので、その経路の Apis を主体つきで直に呼ぶ。

const sessionCookieName = "__Host-session"

type otherImpl struct{ gen.StrictServerInterface }

type noUpWs struct{}

func (noUpWs) Verify(context.Context, string) (uuid.UUID, *fault.Event) {
	return uuid.Nil, fault.New(cgen.AUTH001, nil)
}

type noLinks struct{}

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

// serve は本ブロック、srv-identity、srv-keys を srv-boundary に組み込んで HTTP で受ける。
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
		Devices: apis.Devices, Keys: e.k.Apis(), Rooms: e.b.Apis(), Media: other, Calls: other, Push: other, Admin: other, Shell: other}
	h, err := boundary.NewServer().Build(base, impls, boundary.Verifiers{Session: v.Session, Shell: v.Shell, Ws: v.Ws, UpWs: noUpWs{},
		Membership: e.b.Membership(), State: fakeState{}}, boundary.Links{Snapshot: e.b.Snapshotter(), Events: e.b.EventSource(),
		Typing: e.b.TypingSink(), Up: noLinks{}, Probe: noLinks{}})
	e.must(err)
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return &web{e: e, srv: srv}
}

// cookie は構成員の最初の端末のセッションの Cookie の値。
func (e *env) cookie(m *member) string {
	e.t.Helper()
	var c string
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		c, _, err = e.id.Sessions.Issue(e.ctx, tx, m.id, m.dev, false)
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
	req, err := http.NewRequest(method, w.srv.URL+"/api/v1"+path, rd)
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
func (w *web) problem(status int, code, method, path, cookie string, body any, hdr map[string]string) map[string]any {
	w.e.t.Helper()
	resp, b := w.do(method, path, cookie, body, hdr)
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil || resp.StatusCode != status || p["code"] != code {
		w.e.t.Fatalf("%s %s：%d %s を期待したが %d %s", method, path, status, code, resp.StatusCode, b)
	}
	return p
}

// ok は 200 を確かめて本文を返す（応答は境界が spec で検証済み）。
func (w *web) ok(method, path, cookie string, body any, hdr map[string]string) map[string]any {
	w.e.t.Helper()
	resp, b := w.do(method, path, cookie, body, hdr)
	if resp.StatusCode != 200 {
		w.e.t.Fatalf("%s %s が 200 でない: %d %s", method, path, resp.StatusCode, b)
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

// httpFamily は HTTP の家族のサーバーと 3 人と、3 人の承認済みのグループ。
func httpFamily(t *testing.T) (*env, *web, *member, *member, *member, uuid.UUID) {
	t.Helper()
	e, a, b, c := family3(t)
	return e, e.serve(), a, b, c, e.group(a, b, c)
}

// msg は線の上の送信の本文（ft.event.mls/1 の JSON の形）。
func (e *env) msg(room uuid.UUID, o msgOpt) map[string]any {
	e.t.Helper()
	raw, err := json.Marshal(e.body(room, o))
	e.must(err)
	var m map[string]any
	e.must(json.Unmarshal(raw, &m))
	return m
}

// R: R-4-1-2、R-4-2-1、R-4-4-1、R-4-6-1
// タグ rooms の 25 操作を HTTP で通す（第2章 5.5。応答は spec で検証）：一覧、作成、取得、構成員、自分の設定、送信と読み、関係、書き出し、
// 既読、入力中、予約送信、取消、MLS の受け（epoch、commits、拒みの報告、読めない Commit は KEY-009、Commit を作れる端末がある時の
// 再初期化は KEY-013）、外し、削除。
func TestHttpRoomOperations(t *testing.T) {
	e, w, a, b, c, room := httpFamily(t)
	d := e.member("三郎", boundary.RoleMember)
	e.joinFamily(d)
	ca, cb := e.cookie(a), e.cookie(b)
	r := "/rooms/" + room.String()
	if l := w.ok("GET", "/rooms", ca, nil, nil); len(l["items"].([]any)) != 3 {
		t.Fatalf("一覧: %v", l)
	}
	g := w.ok("POST", "/rooms", ca, map[string]any{"kind": "group", "members": []string{b.id.String(), d.id.String()}}, idemKey())
	if g["kind"] != "group" {
		t.Fatalf("作成: %v", g)
	}
	w.ok("GET", r, cb, nil, nil)
	if ms := w.ok("GET", r+"/members", cb, nil, nil); len(ms["items"].([]any)) != 3 {
		t.Fatalf("構成員: %v", ms)
	}
	if m := w.ok("POST", r+"/members", ca, map[string]any{"user_id": d.id.String()}, nil); m["state"] != "pending_add" {
		t.Fatalf("追加: %v", m)
	}
	if me := w.ok("PUT", r+"/me", cb, map[string]any{"pinned": true}, nil); me["pinned"] != true {
		t.Fatalf("自分の設定: %v", me)
	}
	e.clk.Advance(time.Millisecond)
	p := w.ok("POST", r+"/events", cb, map[string]any{"body": e.msg(room, msgOpt{}), "silent": true}, idemKey())
	eid := p["event_id"].(string)
	e.clk.Advance(time.Millisecond)
	w.ok("POST", r+"/events", ca, map[string]any{"body": e.msg(room, msgOpt{rel: relReply, target: uuid.FromStringOrNil(eid)})}, idemKey())
	if l := w.ok("GET", r+"/events?limit=10&types=ft.event.mls/1", ca, nil, nil); len(l["items"].([]any)) != 2 {
		t.Fatalf("イベントの頁: %v", l)
	}
	w.ok("GET", r+"/events/"+eid, ca, nil, nil)
	if rel := w.ok("GET", r+"/events/"+eid+"/relations?rel_type=reply", ca, nil, nil); len(rel["items"].([]any)) != 1 {
		t.Fatalf("関係: %v", rel)
	}
	w.ok("GET", r+"/export?limit=100", ca, nil, nil)
	w.ok("POST", r+"/receipts", ca, map[string]any{"read": eid}, nil)
	if rs := w.ok("GET", r+"/receipts", cb, nil, nil); len(rs["items"].([]any)) != 1 {
		t.Fatalf("既読の一覧: %v", rs)
	}
	w.ok("PUT", r+"/typing", ca, map[string]any{"typing": true, "timeout": 5}, nil)
	s := w.ok("POST", r+"/scheduled", cb, map[string]any{"send_at": e.clk.Now().Add(time.Hour).Format(time.RFC3339Nano), "body": e.msg(room, msgOpt{})}, idemKey())
	if l := w.ok("GET", r+"/scheduled", cb, nil, nil); len(l["items"].([]any)) != 1 {
		t.Fatalf("予約の一覧: %v", l)
	}
	w.ok("DELETE", r+"/scheduled/"+s["id"].(string), cb, nil, nil)
	w.ok("POST", r+"/events/"+eid+"/redact", cb, map[string]any{}, nil)
	ep := w.ok("GET", r+"/mls/epoch", ca, nil, nil)
	if ep["commit_possible"] != true || ep["generation"] != float64(0) {
		t.Fatalf("epoch: %v", ep)
	}
	w.ok("GET", r+"/mls/commits?since_epoch=0", ca, nil, nil)
	gid := groupIdOf(room, 0)
	bad := base64.RawURLEncoding.EncodeToString(privateMessage(gid, int64(ep["epoch"].(float64)), 1, []byte{1}))
	w.problem(422, cgen.KEY009, "POST", r+"/mls/commit", ca, map[string]any{"epoch": ep["epoch"], "commit": bad}, nil)
	if rj := w.ok("POST", r+"/mls/reject", cb, map[string]any{"generation": 0, "reason": "other"}, nil); rj["reinit_allowed_at"] == nil {
		t.Fatalf("拒みの報告: %v", rj)
	}
	w.problem(409, cgen.KEY013, "POST", r+"/mls/reinit", ca, map[string]any{}, idemKey())
	w.ok("DELETE", r+"/members/"+d.id.String(), ca, nil, nil)
	w.ok("DELETE", r, ca, nil, nil) // 作成者
	_ = c
}

// R: R-12-6-1
// Commit を作れる端末が残らない時の再初期化（第12章 5.2 の (d)、第4章 4.1）：構成員は残るが、leaf を持つ端末が削除待ちか未承認だけ
// （commit_possible が偽）。構成員の承認済みの端末の reinit が通り、世代が進み、削除待ちの構成員は Resolve で外れる。
func TestHttpReinitWhenNoCommitter(t *testing.T) {
	e, w, a, b, c, room := httpFamily(t)
	r := "/rooms/" + room.String()
	ca := e.cookie(a)
	w.ok("DELETE", r+"/members/"+b.id.String(), ca, nil, nil)
	_, err := e.pool.Exec(e.ctx, `DELETE FROM mls_leaves WHERE room_id = $1 AND device_id = $2`, room, a.dev)
	e.must(err)
	c2 := e.addDevice(c, "次郎のタブレット")
	e.must(e.commit(room, []uuid.UUID{c2}, []uuid.UUID{c.dev}))
	_, err = e.pool.Exec(e.ctx, `UPDATE devices SET state = 'unapproved' WHERE id = $1`, c2)
	e.must(err)
	if ep := w.ok("GET", r+"/mls/epoch", ca, nil, nil); ep["commit_possible"] != false {
		t.Fatalf("commit_possible が偽にならない: %v", ep)
	}
	ri := w.ok("POST", r+"/mls/reinit", ca, map[string]any{}, idemKey())
	if ri["generation"] != float64(1) || ri["epoch"] != float64(0) {
		t.Fatalf("再初期化: %v", ri)
	}
	if e.members(room)[b.id] != MemberRemoved {
		t.Fatalf("削除待ちの構成員が外れない: %v", e.members(room))
	}
}

// TestErr_ROOM_001 はトークがない（404）。境界の確かめの後に削除されたトークへの送信（Apis を主体つきで直に呼ぶ）。
func TestErr_ROOM_001(t *testing.T) {
	e, _, a, b, _, room := httpFamily(t)
	body := e.body(room, msgOpt{})
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Rooms.Delete(e.ctx, tx, room, RuleRoomDelete, a.id, false) }))
	ctx := boundary.WithPrincipal(e.ctx, b.p())
	_, err := e.b.Apis().CreateRoomEvent(ctx, gen.CreateRoomEventRequestObject{Id: room.String(),
		Params: gen.CreateRoomEventParams{IdempotencyKey: uuid.Must(uuid.NewV7()).String()}, Body: &gen.CreateRoomEventJSONRequestBody{Body: body}})
	e.wantCode(err, cgen.ROOM001)
	_, err = e.b.Apis().GetRoomMlsEpoch(ctx, gen.GetRoomMlsEpochRequestObject{Id: room.String()})
	e.wantCode(err, cgen.ROOM001)
}

// TestErr_ROOM_002 はトークの構成員ではない（403）。構成員でない人の取得と、構成員でない管理者の削除。
func TestErr_ROOM_002(t *testing.T) {
	e, w, _, _, _, room := httpFamily(t)
	d := e.member("三郎", boundary.RoleAdmin)
	e.joinFamily(d)
	w.problem(403, cgen.ROOM002, "GET", "/rooms/"+room.String(), e.cookie(d), nil, nil)
	w.problem(403, cgen.ROOM002, "DELETE", "/rooms/"+room.String(), e.cookie(d), nil, nil)
}

// TestErr_ROOM_003 はこの操作の権限がない（403）。作成者でない構成員の外しと削除。
func TestErr_ROOM_003(t *testing.T) {
	e, w, a, _, c, room := httpFamily(t)
	w.problem(403, cgen.ROOM003, "DELETE", "/rooms/"+room.String()+"/members/"+a.id.String(), e.cookie(c), nil, nil)
	w.problem(403, cgen.ROOM003, "DELETE", "/rooms/"+room.String(), e.cookie(c), nil, nil)
}

// TestErr_ROOM_004 は 1 対 1 のトークは既にある（409。params.room_id は先のトーク）。
func TestErr_ROOM_004(t *testing.T) {
	e, w, a, b, _, _ := httpFamily(t)
	dm := e.dm(a, b)
	p := w.problem(409, cgen.ROOM004, "POST", "/rooms", e.cookie(b), map[string]any{"kind": "dm", "members": []string{a.id.String()}}, idemKey())
	if params, _ := p["params"].(map[string]any); params["room_id"] != dm.String() {
		t.Fatalf("params.room_id: %v", p)
	}
}

// TestErr_ROOM_005 はイベントの形式（422。暗号文が MLS の PrivateMessage でない）。
func TestErr_ROOM_005(t *testing.T) {
	e, w, a, _, _, room := httpFamily(t)
	m := e.msg(room, msgOpt{})
	m["ciphertext"] = base64.RawURLEncoding.EncodeToString([]byte("not mls"))
	w.problem(422, cgen.ROOM005, "POST", "/rooms/"+room.String()+"/events", e.cookie(a), map[string]any{"body": m}, idemKey())
}

// TestErr_ROOM_006 は送信先の epoch が古い（409）。
func TestErr_ROOM_006(t *testing.T) {
	e, w, a, _, _, room := httpFamily(t)
	w.problem(409, cgen.ROOM006, "POST", "/rooms/"+room.String()+"/events", e.cookie(a), map[string]any{"body": e.msg(room, msgOpt{epochOff: -1})}, idemKey())
}

// TestErr_ROOM_007 はイベントがない（404）。
func TestErr_ROOM_007(t *testing.T) {
	e, w, a, _, _, room := httpFamily(t)
	w.problem(404, cgen.ROOM007, "GET", "/rooms/"+room.String()+"/events/"+uuid.Must(uuid.NewV7()).String(), e.cookie(a), nil, nil)
}

// TestErr_ROOM_008 はイベントは削除済み（410。取消の 2 度目）。
func TestErr_ROOM_008(t *testing.T) {
	e, w, a, _, _, room := httpFamily(t)
	ev := e.send(room, a, msgOpt{})
	path := "/rooms/" + room.String() + "/events/" + ev.String() + "/redact"
	w.ok("POST", path, e.cookie(a), map[string]any{}, nil)
	w.problem(410, cgen.ROOM008, "POST", path, e.cookie(a), map[string]any{}, nil)
}

// TestErr_ROOM_009 は編集・取消の期限の超過（422。他人の物の編集と、7 日の後の取消）。
func TestErr_ROOM_009(t *testing.T) {
	e, w, a, b, _, room := httpFamily(t)
	ev := e.send(room, a, msgOpt{})
	w.problem(422, cgen.ROOM009, "POST", "/rooms/"+room.String()+"/events", e.cookie(b),
		map[string]any{"body": e.msg(room, msgOpt{rel: relReplace, target: ev})}, idemKey())
	e.clk.Advance(8 * 24 * time.Hour)
	w.problem(422, cgen.ROOM009, "POST", "/rooms/"+room.String()+"/events/"+ev.String()+"/redact", e.cookie(a), map[string]any{}, nil)
}

// TestErr_ROOM_010 は予約の時刻（422。過去と 30 日の後）。
func TestErr_ROOM_010(t *testing.T) {
	e, w, a, _, _, room := httpFamily(t)
	for _, at := range []time.Time{e.clk.Now().Add(-time.Minute), e.clk.Now().Add(31 * 24 * time.Hour)} {
		w.problem(422, cgen.ROOM010, "POST", "/rooms/"+room.String()+"/scheduled", e.cookie(a),
			map[string]any{"send_at": at.Format(time.RFC3339Nano), "body": e.msg(room, msgOpt{})}, idemKey())
	}
}

// TestErr_ROOM_011 は家族全員のトークは退出・削除できない（403）。
func TestErr_ROOM_011(t *testing.T) {
	e, w, a, b, _, _ := httpFamily(t)
	fam := e.family().String()
	w.problem(403, cgen.ROOM011, "DELETE", "/rooms/"+fam+"/members/"+b.id.String(), e.cookie(b), nil, nil)
	w.problem(403, cgen.ROOM011, "DELETE", "/rooms/"+fam, e.cookie(a), nil, nil)
}

// TestErr_ROOM_012 は関係の対象が別のトークにある（422。送信と予約送信）。
func TestErr_ROOM_012(t *testing.T) {
	e, w, a, _, _, room := httpFamily(t)
	far := e.send(e.family(), a, msgOpt{})
	w.problem(422, cgen.ROOM012, "POST", "/rooms/"+room.String()+"/events", e.cookie(a),
		map[string]any{"body": e.msg(room, msgOpt{rel: relReply, target: far})}, idemKey())
	w.problem(422, cgen.ROOM012, "POST", "/rooms/"+room.String()+"/scheduled", e.cookie(a),
		map[string]any{"send_at": e.clk.Now().Add(time.Hour).Format(time.RFC3339Nano), "body": e.msg(room, msgOpt{rel: relReply, target: far})}, idemKey())
}

// TestErr_ROOM_013 は予約送信に付けられない関係（422。編集とリアクションの予約）。
func TestErr_ROOM_013(t *testing.T) {
	e, w, a, _, _, room := httpFamily(t)
	ev := e.send(room, a, msgOpt{})
	for _, rel := range []gen.FtEventMls1RelatesToRelType{relReplace, relAnnotation} {
		w.problem(422, cgen.ROOM013, "POST", "/rooms/"+room.String()+"/scheduled", e.cookie(a),
			map[string]any{"send_at": e.clk.Now().Add(time.Hour).Format(time.RFC3339Nano), "body": e.msg(room, msgOpt{rel: rel, target: ev})}, idemKey())
	}
}
