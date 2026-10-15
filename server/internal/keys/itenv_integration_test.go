// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package keys

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/envelope"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/identity"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 試験の DB：移行を済ませた雛形から試験ごとに新しい DB を作る。トークの表 rooms は srv-rooms の移行が作る物で、まだ無いので、
// RoomLock が錠を取る識別子の列だけの表を雛形に置く（試験の土台。srv-rooms の移行が入ったら、その表を使う）。
const itTemplate = "ft_keys_tpl"

var (
	itInfo  store.ConnInfo
	itAdmin *store.Pool
	itSeq   atomic.Int64
)

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
	itInfo = store.ConnInfo{Host: host, Port: uint16(p), Database: store.DbName, User: store.DbUser, Password: []byte("test-pw")}
	if itAdmin, err = store.Open(ctx, itInfo); err != nil {
		fmt.Fprintln(os.Stderr, "接続できない:", err)
		os.Exit(1)
	}
	if _, err := itAdmin.Exec(ctx, `CREATE DATABASE `+itTemplate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	info := itInfo
	info.Database = itTemplate
	tpl, err := store.Open(ctx, info)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := store.NewMigrator(tpl).Apply(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "移行できない:", err)
		os.Exit(1)
	}
	if _, err := tpl.Exec(ctx, `CREATE TABLE IF NOT EXISTS rooms (id uuid PRIMARY KEY)`); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tpl.Close()
	code := m.Run()
	itAdmin.Close()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

// freshPool は雛形から新しい DB を作って開く。
func freshPool(t *testing.T) *store.Pool {
	t.Helper()
	ctx := context.Background()
	name := "ft_keys_" + strconv.FormatInt(itSeq.Add(1), 10)
	if _, err := itAdmin.Exec(ctx, `CREATE DATABASE `+name+` TEMPLATE `+itTemplate); err != nil {
		t.Fatal(err)
	}
	info := itInfo
	info.Database = name
	p, err := store.Open(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// testSecrets は KEK 1 本とセッションの署名鍵を置いた秘密の置き場（srv-identity の端末の名前の暗号化に要る）。
func testSecrets(t *testing.T, now time.Time) *secret.Store {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	kek, sess := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(kek)
	_, _ = rand.Read(sess)
	write(secret.NameKek, map[string]any{"keys": []map[string]any{{"id": uuid.Must(uuid.NewV7()).String(), "key": kek, "created_at": now.Add(-time.Hour)}}})
	write(secret.NameSessionKeys, map[string]any{"keys": []map[string]any{
		{"id": "k", "alg": "HS256", "key": sess, "created_at": now.Add(-time.Hour), "retire_after": now.Add(90 * 24 * time.Hour)}}})
	return secret.NewStore(secret.Dirs{Data: dir, Run: dir})
}

// 上のブロックの偽物。

type fakePush struct{}

func (fakePush) SubscribedDevices(context.Context, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }

type fakeAudit struct{}

func (fakeAudit) Record(context.Context, *store.Tx, uuid.UUID, string, string, map[string]any) error {
	return nil
}

type fakeState struct{}

func (fakeState) State() boundary.ServerState { return boundary.StateNormal }
func (fakeState) ReadOnlyReason() string      { return "" }

// fakeRooms は srv-rooms の RoomReader の代わり（トークごとの構成員と状態。active、pending_add、pending_remove）。
type fakeRooms struct {
	mu    sync.Mutex
	rooms map[uuid.UUID]map[uuid.UUID]string
}

func (f *fakeRooms) set(room, user uuid.UUID, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rooms[room] == nil {
		f.rooms[room] = map[uuid.UUID]string{}
	}
	f.rooms[room][user] = state
}

func (f *fakeRooms) RoomsOf(_ context.Context, _ *store.Tx, user uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for r, m := range f.rooms {
		if s := m[user]; s == "active" || s == "pending_add" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRooms) MembersOf(_ context.Context, _ *store.Tx, room uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for u := range f.rooms[room] {
		out = append(out, u)
	}
	return sortedIds(out), nil
}

func (f *fakeRooms) IsMemberOrPending(_ context.Context, _ *store.Tx, room, user uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.rooms[room][user]
	return s == "active" || s == "pending_add", nil
}

// noticeRec はコミットされた知らせの 1 件。
type noticeRec struct {
	kind     string
	audience identity.AudienceKind
	user     uuid.UUID
	params   map[string]any
}

// noticeLog は hook OnNotice に渡った知らせのうち、コミットされた物だけを覚える。
type noticeLog struct {
	mu   sync.Mutex
	rows []noticeRec
}

func (l *noticeLog) hook(_ context.Context, tx *store.Tx, n identity.Notice) error {
	r := noticeRec{kind: n.Kind, audience: n.Audience.Kind, params: n.Params}
	if n.Audience.UserId != nil {
		r.user = *n.Audience.UserId
	}
	tx.AfterCommit(func() {
		l.mu.Lock()
		l.rows = append(l.rows, r)
		l.mu.Unlock()
	})
	return nil
}

func (l *noticeLog) of(kind string) []noticeRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []noticeRec
	for _, r := range l.rows {
		if r.kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// env は 1 つの試験の家族のサーバー（本ブロックと下のブロックは本物、srv-rooms と上のブロックは偽物）。
type env struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	clk     *clock.TestClock
	id      *identity.Identity
	k       *Keys
	rooms   *fakeRooms
	notices *noticeLog
}

// member は試験の構成員と、その本人の署名鍵と端末。
type member struct {
	id   uuid.UUID
	usk  *ecdsa.PrivateKey
	devs []*devKeys
}

// dev は最初の端末。
func (m *member) dev() *devKeys { return m.devs[0] }

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := freshPool(t)
	clk := clock.NewTestClock(clock.WallClock{}.Now())
	ix := ids.New(clk, rand.Reader)
	cfg, err := config.Load(map[string]string{"FT_DOMAIN": "family.example", "FT_DNS_PROVIDER": "test", "FT_DATA_DIR": t.TempDir()}, nil, cgen.Items)
	if err != nil {
		t.Fatal(err)
	}
	feed := store.NewFeed(pool)
	id := identity.New(identity.Deps{Pool: pool, Feed: feed, Jobs: store.NewJobs(pool, ix, clk), Retention: store.NewRetention(pool), Ids: ix,
		Envelope: envelope.New(rand.Reader), Secrets: testSecrets(t, clk.Now()), Clock: clk, Config: cfg})
	k := New(Deps{Pool: pool, Feed: feed, RoomLock: store.RoomLock{}, Ids: ix, Clock: clk, Identity: id})
	e := &env{t: t, ctx: context.Background(), pool: pool, clk: clk, id: id, k: k, rooms: &fakeRooms{rooms: map[uuid.UUID]map[uuid.UUID]string{}},
		notices: &noticeLog{}}
	k.Register(id)
	id.SetKeys(k.Reader())
	id.SetPush(fakePush{})
	id.SetAudit(fakeAudit{})
	id.SetState(fakeState{})
	id.Hooks.RegisterOnNotice(e.notices.hook)
	k.SetRooms(e.rooms)
	if err := k.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := id.Ready(); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(e.ctx, `INSERT INTO families (id, name, color, installed_at) VALUES ($1, '試験の家', '#3366cc', $2)`, ix.NewV7(), clk.Now()); err != nil {
		t.Fatal(err)
	}
	return e
}

// tx は fn を 1 つのトランザクションで走らせ、誤りなら rollback して返す（要求のトランザクションの代わり）。
func (e *env) tx(fn func(tx *store.Tx) error) error {
	return e.pool.InTx(e.ctx, fn)
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("予期しない誤り: %v", err)
	}
}

// wantCode は err が code の Event か。
func (e *env) wantCode(err error, code string) *fault.Event {
	e.t.Helper()
	var ev *fault.Event
	if !errors.As(err, &ev) || ev.Code != code {
		e.t.Fatalf("%s を期待したが %v", code, err)
	}
	return ev
}

// scalar は 1 つの値を読む。
func (e *env) scalar(dst any, sql string, args ...any) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(dst); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// count は数を読む。
func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	e.scalar(&n, sql, args...)
	return n
}

// jwkOf は端末の公開鍵の JWK（正規の形）。
func jwkOf(t testing.TB, k *ecdsa.PrivateKey) gen.CommonJwkP2561 {
	return canonicalJwk(sec1(t, k))
}

// userSig は本人の署名鍵 usk による端末 dk の user_sig（第12章 5.3）。
func userSigFor(t testing.TB, usk *ecdsa.PrivateKey, dk *devKeys) []byte {
	return signRS(t, usk, userSigMessage(dk.userId, dk.deviceId, sec1(t, dk.sig), sec1(t, dk.kem)))
}

// newDevice は端末を登録し（未承認）、鍵を登録する。
func (e *env) newDevice(user uuid.UUID, name string) *devKeys {
	e.t.Helper()
	var dv identity.Device
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		dv, err = e.id.Devices.Register(e.ctx, tx, user, identity.PlatformIphone, name)
		return err
	}))
	dk := newDevKeys(e.t, user, dv.Id)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.DeviceKeys.Put(e.ctx, tx, user, dk.deviceId, jwkOf(e.t, dk.sig), jwkOf(e.t, dk.kem), credentialFor(user, dk.deviceId))
		return err
	}))
	return dk
}

// approve は by の端末（自己承認なら同じ端末）で dk を承認する。
func (e *env) approve(m *member, by, dk *devKeys) error {
	return e.tx(func(tx *store.Tx) error {
		_, err := e.k.DeviceKeys.Approve(e.ctx, tx, m.id, by.deviceId, dk.deviceId, userSigFor(e.t, m.usk, dk))
		return err
	})
}

// member は構成員と最初の端末を作り、本人の署名鍵を置いて自己承認する。
func (e *env) member(name string, role boundary.Role) *member {
	e.t.Helper()
	var usr identity.User
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		usr, err = e.id.Users.Create(e.ctx, tx, name, role, nil)
		return err
	}))
	m := &member{id: usr.Id, usk: newKey(e.t)}
	dk := e.newDevice(usr.Id, name+"の電話")
	m.devs = append(m.devs, dk)
	e.must(e.tx(func(tx *store.Tx) error {
		_, err := e.k.UserSigningKeys.Put(e.ctx, tx, m.id, dk.deviceId, jwkOf(e.t, m.usk), nil)
		return err
	}))
	e.must(e.approve(m, dk, dk))
	return m
}

// addDevice は構成員の新しい端末を登録し、approved なら最初の端末で承認する。
func (e *env) addDevice(m *member, name string, approved bool) *devKeys {
	e.t.Helper()
	dk := e.newDevice(m.id, name)
	m.devs = append(m.devs, dk)
	if approved {
		e.must(e.approve(m, m.dev(), dk))
	}
	return dk
}

// room はトークを作り（rooms の行と偽の構成員の表）、creator の端末を leaf 0 にした世代 0 のグループを作る。
func (e *env) room(creator *devKeys, members ...*member) uuid.UUID {
	e.t.Helper()
	r := uuid.Must(uuid.NewV7())
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO rooms (id) VALUES ($1)`, r); err != nil {
		e.t.Fatal(err)
	}
	for _, m := range members {
		e.rooms.set(r, m.id, "active")
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.k.Groups.Create(e.ctx, tx, r, creator.deviceId) }))
	return r
}

// feedOf は利用者の feed の行（種類で絞る。空なら全部。宛先の端末で絞らない）。
func (e *env) feedOf(user uuid.UUID, kind string) []store.FeedRow {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT pos, kind, ref_id, room_id, device_id, payload FROM feed WHERE user_id = $1 AND ($2 = '' OR kind = $2) ORDER BY pos`,
		user, kind)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []store.FeedRow
	for rows.Next() {
		var r store.FeedRow
		var room, dev *uuid.UUID
		if err := rows.Scan(&r.Pos, &r.Ref.Kind, &r.Ref.Id, &room, &dev, &r.Ref.Payload); err != nil {
			e.t.Fatal(err)
		}
		if room != nil {
			r.Ref.RoomId = *room
		}
		if dev != nil {
			r.Ref.DeviceId = *dev
		}
		out = append(out, r)
	}
	return out
}

// payloads は feed の行の payload を v の並びに読む。
func payloads[T any](t *testing.T, rows []store.FeedRow) []T {
	t.Helper()
	out := make([]T, len(rows))
	for i, r := range rows {
		if err := json.Unmarshal(r.Ref.Payload, &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// principal は端末の主体（承認済みか休眠なら DeviceApproved）。reauth は再認証の時刻（nil は無し）。
func (e *env) principal(user uuid.UUID, dk *devKeys, reauth *time.Time) context.Context {
	e.t.Helper()
	dv, err := e.id.Devices.Get(e.ctx, e.pool, dk.deviceId)
	e.must(err)
	u, d := user, dk.deviceId
	p := boundary.Principal{Kind: boundary.KindUser, UserId: &u, DeviceId: &d, Role: boundary.RoleMember,
		DeviceApproved: dv.State == identity.DeviceApproved || dv.State == identity.DeviceDormant, ReauthAt: reauth}
	return boundary.WithPrincipal(e.ctx, p)
}

// devState は端末の状態。
func (e *env) devState(d uuid.UUID) identity.DeviceState {
	e.t.Helper()
	dv, err := e.id.Devices.Get(e.ctx, e.pool, d)
	e.must(err)
	return dv.State
}

// containsId は並びに id があるか。
func containsId(list []uuid.UUID, id uuid.UUID) bool { return slices.Contains(list, id) }
