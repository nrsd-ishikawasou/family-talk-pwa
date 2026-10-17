// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package rooms

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
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
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/keys"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

// 試験の DB：移行を済ませた雛形から試験ごとに新しい DB を作る。本ブロックと下のブロック（srv-store、srv-identity、srv-keys）は本物、
// srv-push（Notifier）、srv-media（MediaBinder）、srv-calls（CallEnder）と入力中の送り先は記録だけの偽物。MLS の Commit は端末が
// 作る物で、本ブロックの試験は srv-keys の Groups.Commit が残す記録（mls_leaves と epoch）を DB に置いてから Members.Resolve を呼ぶ
// （Commit の確かめは srv-keys の試験が持つ）。
const itTemplate = "ft_rooms_tpl"

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
	name := "ft_rooms_" + strconv.FormatInt(itSeq.Add(1), 10)
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

// testSecrets は KEK 1 本とセッションの署名鍵を置いた秘密の置き場（srv-identity のエンベロープとセッションに要る）。
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

// srv-identity に渡す上のブロックの偽物。

type fakePush struct{}

func (fakePush) SubscribedDevices(context.Context, store.Querier, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

type fakeAudit struct{}

func (fakeAudit) Record(context.Context, *store.Tx, uuid.UUID, string, string, map[string]any) error {
	return nil
}

type fakeState struct{}

func (fakeState) State() boundary.ServerState { return boundary.StateNormal }
func (fakeState) ReadOnlyReason() string      { return "" }

// noticeLog は hook OnNotice に渡った知らせ（コミットされた物の kind と宛先の列）。
type noticeLog struct {
	mu   sync.Mutex
	rows []identity.Notice
}

func (l *noticeLog) hook(_ context.Context, tx *store.Tx, n identity.Notice) error {
	if n.Key == "" || len(n.Audiences) == 0 {
		return fmt.Errorf("知らせ %s の鍵か宛先が空", n.Kind)
	}
	tx.AfterCommit(func() {
		l.mu.Lock()
		l.rows = append(l.rows, n)
		l.mu.Unlock()
	})
	return nil
}

// notified は Notifier.Notify の 1 回（コミットされた物）。
type notified struct {
	event Event
	to    []uuid.UUID
}

// fakeNotifier は srv-push の Notifier の代わり（呼ばれたイベントと受け手を記録する）。
type fakeNotifier struct {
	mu    sync.Mutex
	calls []notified
}

func (f *fakeNotifier) Notify(_ context.Context, tx *store.Tx, e Event, to []uuid.UUID) error {
	n := notified{event: e, to: slices.Clone(to)}
	tx.AfterCommit(func() {
		f.mu.Lock()
		f.calls = append(f.calls, n)
		f.mu.Unlock()
	})
	return nil
}

// of はイベントの識別子の通知（無ければ偽）。
func (f *fakeNotifier) of(id uuid.UUID) (notified, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.event.Id == id {
			return c, true
		}
	}
	return notified{}, false
}

// fakeMedia は srv-media の MediaBinder の代わり。結び付きを記録し（コミットされた物だけ）、fail の媒体は Bind と BindScheduled で
// その番号を返す。
type fakeMedia struct {
	mu        sync.Mutex
	fail      map[uuid.UUID]string
	events    map[uuid.UUID][]uuid.UUID // イベント → 媒体
	scheduled map[uuid.UUID][]uuid.UUID // 予約 → 媒体
	log       []string
}

func newFakeMedia() *fakeMedia {
	return &fakeMedia{fail: map[uuid.UUID]string{}, events: map[uuid.UUID][]uuid.UUID{}, scheduled: map[uuid.UUID][]uuid.UUID{}}
}

func (f *fakeMedia) check(ids []uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if code, ok := f.fail[id]; ok {
			return fault.New(code, nil)
		}
	}
	return nil
}

func (f *fakeMedia) after(tx *store.Tx, what string, fn func()) {
	tx.AfterCommit(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.log = append(f.log, what)
		fn()
	})
}

func (f *fakeMedia) Bind(_ context.Context, tx *store.Tx, _, _, eventId uuid.UUID, mediaIds []uuid.UUID) error {
	if err := f.check(mediaIds); err != nil {
		return err
	}
	ids := slices.Clone(mediaIds)
	f.after(tx, "bind", func() { f.events[eventId] = ids })
	return nil
}

func (f *fakeMedia) BindScheduled(_ context.Context, tx *store.Tx, _, _, scheduledId uuid.UUID, mediaIds []uuid.UUID) error {
	if err := f.check(mediaIds); err != nil {
		return err
	}
	ids := slices.Clone(mediaIds)
	f.after(tx, "bind_scheduled", func() { f.scheduled[scheduledId] = ids })
	return nil
}

func (f *fakeMedia) Rebind(_ context.Context, tx *store.Tx, scheduledId, eventId uuid.UUID) error {
	f.after(tx, "rebind", func() {
		f.events[eventId] = f.scheduled[scheduledId]
		delete(f.scheduled, scheduledId)
	})
	return nil
}

func (f *fakeMedia) Unbind(_ context.Context, tx *store.Tx, eventId uuid.UUID) error {
	f.after(tx, "unbind", func() { delete(f.events, eventId) })
	return nil
}

func (f *fakeMedia) UnbindEvents(_ context.Context, tx *store.Tx, eventIds []uuid.UUID) error {
	if len(eventIds) > redactionBatch {
		return fmt.Errorf("UnbindEvents に %d 件（1,000 件まで）", len(eventIds))
	}
	ids := slices.Clone(eventIds)
	f.after(tx, "unbind_events:"+strconv.Itoa(len(ids)), func() {
		for _, id := range ids {
			delete(f.events, id)
		}
	})
	return nil
}

func (f *fakeMedia) UnbindScheduled(_ context.Context, tx *store.Tx, scheduledId uuid.UUID) error {
	f.after(tx, "unbind_scheduled", func() { delete(f.scheduled, scheduledId) })
	return nil
}

func (f *fakeMedia) UnbindRoom(_ context.Context, tx *store.Tx, roomId uuid.UUID) error {
	f.after(tx, "unbind_room:"+roomId.String(), func() {})
	return nil
}

// boundEvent はイベントに結び付いた媒体。boundScheduled は予約に結び付いた媒体。
func (f *fakeMedia) boundEvent(id uuid.UUID) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[id]
}

func (f *fakeMedia) boundScheduled(id uuid.UUID) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduled[id]
}

func (f *fakeMedia) logged(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.log {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// fakeCalls は srv-calls の CallEnder の代わり（EndAll を呼ばれたトーク）。
type fakeCalls struct {
	mu    sync.Mutex
	ended []uuid.UUID
}

func (f *fakeCalls) EndAll(_ context.Context, tx *store.Tx, roomId uuid.UUID) error {
	tx.AfterCommit(func() {
		f.mu.Lock()
		f.ended = append(f.ended, roomId)
		f.mu.Unlock()
	})
	return nil
}

// frame は入力中の送り先に渡ったフレーム。
type frame struct {
	to      []uuid.UUID
	kind    string
	payload map[string]any
}

// frames は入力中の送り先の記録。
type frames struct {
	mu   sync.Mutex
	list []frame
}

func (f *frames) send(to []uuid.UUID, kind string, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, frame{to: slices.Clone(to), kind: kind, payload: payload.(map[string]any)})
}

func (f *frames) all() []frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list)
}

// env は 1 つの試験の家族のサーバー。
type env struct {
	t        *testing.T
	ctx      context.Context
	pool     *store.Pool
	clk      *clock.TestClock
	cfg      *config.Config
	id       *identity.Identity
	k        *keys.Keys
	b        *Block
	notices  *noticeLog
	notifier *fakeNotifier
	media    *fakeMedia
	calls    *fakeCalls
	frames   *frames
}

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
	jobs := store.NewJobs(pool, ix, clk)
	id := identity.New(identity.Deps{Pool: pool, Feed: feed, Jobs: jobs, Retention: store.NewRetention(pool), Ids: ix,
		Envelope: envelope.New(rand.Reader), Secrets: testSecrets(t, clk.Now()), Clock: clk, Config: cfg})
	k := keys.New(keys.Deps{Pool: pool, Feed: feed, RoomLock: store.RoomLock{}, Ids: ix, Clock: clk, Identity: id})
	b := New(Deps{Pool: pool, Feed: feed, RoomLock: store.RoomLock{}, Jobs: jobs, Ids: ix, Clock: clk, Config: cfg, Identity: id, Keys: k})
	e := &env{t: t, ctx: context.Background(), pool: pool, clk: clk, cfg: cfg, id: id, k: k, b: b, notices: &noticeLog{},
		notifier: &fakeNotifier{}, media: newFakeMedia(), calls: &fakeCalls{}, frames: &frames{}}
	k.Register(id)
	b.Rooms.Register(id)
	id.SetKeys(k.Reader())
	id.SetPush(fakePush{})
	id.SetAudit(fakeAudit{})
	id.SetState(fakeState{})
	id.Hooks.RegisterOnNotice(e.notices.hook)
	k.SetRooms(b.Reader())
	b.SetNotifier(e.notifier)
	b.SetMediaBinder(e.media)
	b.SetCallEnder(e.calls)
	b.SetBroadcast(e.frames.send)
	for _, ready := range []func() error{k.Ready, id.Ready, b.Ready} {
		if err := ready(); err != nil {
			t.Fatal(err)
		}
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

// member は試験の構成員と最初の端末。
type member struct {
	id   uuid.UUID
	dev  uuid.UUID
	role boundary.Role
}

// p は端末 dev の主体（承認済み）。
func (m *member) p() boundary.Principal {
	u, d := m.id, m.dev
	return boundary.Principal{Kind: boundary.KindUser, UserId: &u, DeviceId: &d, Role: m.role, DeviceApproved: true}
}

// member は構成員と承認済みの最初の端末を作り、参加の hook（家族全員のトークと自分用メモ）を本ブロックに通す（srv-identity の設置と
// 招待の受諾が同じトランザクションで hook OnUserCreated を呼ぶ形）。端末の承認は srv-keys の鍵の承認の結果（devices.state）を置く。
func (e *env) member(name string, role boundary.Role) *member {
	e.t.Helper()
	m := &member{role: role}
	e.must(e.tx(func(tx *store.Tx) error {
		usr, err := e.id.Users.Create(e.ctx, tx, name, role, nil)
		if err != nil {
			return err
		}
		m.id = usr.Id
		dv, err := e.id.Devices.Register(e.ctx, tx, usr.Id, identity.PlatformIphone, name+"の電話")
		if err != nil {
			return err
		}
		m.dev = dv.Id
		if err := e.approveDevice(tx, dv.Id); err != nil {
			return err
		}
		return e.b.Rooms.onUserCreated(e.ctx, tx, usr.Id, dv.Id)
	}))
	return m
}

// approveDevice は端末を承認済みにする（srv-keys の鍵の承認が残す devices の状態）。
func (e *env) approveDevice(tx *store.Tx, dev uuid.UUID) error {
	_, err := tx.Exec(e.ctx, `UPDATE devices SET state = 'approved', first_approved_at = coalesce(first_approved_at, $2) WHERE id = $1`, dev, e.clk.Now())
	return err
}

// addDevice は構成員の承認済みの端末を足す。
func (e *env) addDevice(m *member, name string) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	e.must(e.tx(func(tx *store.Tx) error {
		dv, err := e.id.Devices.Register(e.ctx, tx, m.id, identity.PlatformAndroid, name)
		if err != nil {
			return err
		}
		id = dv.Id
		return e.approveDevice(tx, dv.Id)
	}))
	return id
}

// family は家族全員のトークの識別子。
func (e *env) family() uuid.UUID {
	e.t.Helper()
	id, err := e.b.FamilyRoom.Id(e.ctx, e.pool)
	e.must(err)
	return id
}

// selfRoom は本人の自分用メモ。
func (e *env) selfRoom(m *member) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	e.scalar(&id, `SELECT r.id FROM rooms r JOIN room_members m ON m.room_id = r.id WHERE r.kind = 'self' AND m.user_id = $1`, m.id)
	return id
}

// leaf は端末を MLS のグループの leaf の記録に置く（srv-keys Groups.Commit の Add が残す記録。leaf の番号は空きの最小）。
func (e *env) leaf(tx *store.Tx, room, dev uuid.UUID) error {
	_, err := tx.Exec(e.ctx, `INSERT INTO mls_leaves (room_id, leaf_index, device_id, since_epoch)
SELECT $1, (SELECT min(i) FROM generate_series(0, 1000) i WHERE i NOT IN (SELECT leaf_index FROM mls_leaves WHERE room_id = $1)), $2,
  (SELECT epoch FROM mls_groups WHERE room_id = $1)`, room, dev)
	return err
}

// commit は端末の Add と Remove の Commit が通った後の記録を置き（leaf の出し入れと epoch の 1 つ進め）、/mls/commit の受けと同じく
// 同じトランザクションで Members.Resolve を呼ぶ。
func (e *env) commit(room uuid.UUID, added, removed []uuid.UUID) error {
	e.clk.Advance(time.Millisecond)
	return e.tx(func(tx *store.Tx) error {
		if err := e.b.d.RoomLock.Lock(e.ctx, tx, room); err != nil {
			return err
		}
		for _, d := range added {
			if err := e.leaf(tx, room, d); err != nil {
				return err
			}
		}
		if len(removed) > 0 {
			if _, err := tx.Exec(e.ctx, `DELETE FROM mls_leaves WHERE room_id = $1 AND device_id = ANY($2)`, room, removed); err != nil {
				return err
			}
			if _, err := tx.Exec(e.ctx, `DELETE FROM mls_pending WHERE room_id = $1 AND device_id = ANY($2) AND kind = 'remove'`, room, removed); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(e.ctx, `DELETE FROM mls_pending WHERE room_id = $1 AND device_id = ANY($2) AND kind = 'add'`, room, added); err != nil {
			return err
		}
		if _, err := tx.Exec(e.ctx, `UPDATE mls_groups SET epoch = epoch + 1 WHERE room_id = $1`, room); err != nil {
			return err
		}
		return e.b.Members.Resolve(e.ctx, tx, room, added, removed)
	})
}

// joinFamily は家族全員のトークの追加待ちを Commit で解く（参加の hook の後）。
func (e *env) joinFamily(m *member) {
	e.t.Helper()
	e.must(e.commit(e.family(), []uuid.UUID{m.dev}, nil))
}

// group は creator が作り others が active のグループ（作った後に Commit で全員を Add）。
func (e *env) group(creator *member, others ...*member) uuid.UUID {
	e.t.Helper()
	list := make([]uuid.UUID, len(others))
	devs := make([]uuid.UUID, len(others))
	for i, m := range others {
		list[i], devs[i] = m.id, m.dev
	}
	var v RoomView
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		v, err = e.b.Rooms.Create(e.ctx, tx, creator.p(), KindGroup, list)
		return err
	}))
	room := uuid.FromStringOrNil(v.Id)
	e.must(e.commit(room, devs, nil))
	return room
}

// dm は 2 人の 1 対 1（相手を Commit で Add）。
func (e *env) dm(a, b *member) uuid.UUID {
	e.t.Helper()
	var v RoomView
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		v, err = e.b.Rooms.Create(e.ctx, tx, a.p(), KindDm, []uuid.UUID{b.id})
		return err
	}))
	room := uuid.FromStringOrNil(v.Id)
	e.must(e.commit(room, []uuid.UUID{b.dev}, nil))
	return room
}

// epoch はグループの世代と epoch。
func (e *env) epoch(room uuid.UUID) (int, int64) {
	e.t.Helper()
	var g int
	var ep int64
	e.scalar(&g, `SELECT generation FROM mls_groups WHERE room_id = $1`, room)
	e.scalar(&ep, `SELECT epoch FROM mls_groups WHERE room_id = $1`, room)
	return g, ep
}

// tlsVar は RFC 9420 の可変長の長さ（QUIC の形の最小の符号化）と中身。
func tlsVar(b []byte) []byte {
	n := len(b)
	var h []byte
	switch {
	case n < 64:
		h = []byte{byte(n)}
	case n < 16384:
		h = []byte{0x40 | byte(n>>8), byte(n)}
	default:
		h = []byte{0x80 | byte(n>>24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return append(h, b...)
}

// privateMessage は MLSMessage の PrivateMessage（RFC 9420 6.3。版 mls10、wire format 2）。中身の暗号文は試験の任意のバイト。
func privateMessage(groupId []byte, epoch int64, contentType byte, ct []byte) []byte {
	out := []byte{0, 1, 0, 2}
	out = append(out, tlsVar(groupId)...)
	out = binary.BigEndian.AppendUint64(out, uint64(epoch))
	out = append(out, contentType)
	out = append(out, tlsVar(nil)...)
	out = append(out, tlsVar([]byte{1})...)
	return append(out, tlsVar(ct)...)
}

// groupIdOf は MLS のグループの識別子（トークの識別子 16 バイトと世代 4 バイト。第12章 5.2）。
func groupIdOf(room uuid.UUID, generation int) []byte {
	return binary.BigEndian.AppendUint32(slices.Clone(room.Bytes()), uint32(generation))
}

// msgOpt は試験の送信の本体の作り。
type msgOpt struct {
	rel      gen.FtEventMls1RelatesToRelType
	target   uuid.UUID
	size     int
	media    []uuid.UUID
	epochOff int64
}

// body は今の世代と epoch の ft.event.mls/1（暗号文の外側は CheckSender が読める PrivateMessage）。
func (e *env) body(room uuid.UUID, o msgOpt) gen.FtEventMls1 {
	e.t.Helper()
	g, ep := e.epoch(room)
	ep += o.epochOff
	gid := groupIdOf(room, g)
	ct := make([]byte, 16+o.size)
	_, _ = rand.Read(ct)
	b := gen.FtEventMls1{GroupId: base64.RawURLEncoding.EncodeToString(gid), Epoch: int(ep), SchemaInner: "ft.content.text/1",
		Ciphertext: base64.RawURLEncoding.EncodeToString(privateMessage(gid, ep, 1, ct))}
	if o.rel != "" {
		b.RelatesTo = &struct {
			EventId gen.CommonUuidv71               `json:"event_id"`
			Key     *string                         `json:"key,omitempty"`
			RelType gen.FtEventMls1RelatesToRelType `json:"rel_type"`
		}{EventId: o.target.String(), RelType: o.rel}
	}
	if len(o.media) > 0 {
		items := make([]struct {
			Hash gen.CommonSha2561 `json:"hash"`
			Id   gen.CommonUuidv71 `json:"id"`
			Size int               `json:"size"`
		}, len(o.media))
		for i, m := range o.media {
			items[i].Id, items[i].Hash, items[i].Size = m.String(), "00", 1
		}
		b.Attachments = &items
	}
	return b
}

// post は m の端末からの送信（応答と誤り）。時計を 1 ミリ秒進めてから送る（実際の送信の間の時間を写す）。
func (e *env) post(room uuid.UUID, m *member, o msgOpt) (Posted, error) {
	e.clk.Advance(time.Millisecond)
	var out Posted
	err := e.tx(func(tx *store.Tx) error {
		var err error
		out, err = e.b.Timeline.Post(e.ctx, tx, m.p(), room, e.body(room, o), false, uuid.Must(uuid.NewV7()))
		return err
	})
	return out, err
}

// send は通らなければならない送信のイベントの識別子。
func (e *env) send(room uuid.UUID, m *member, o msgOpt) uuid.UUID {
	e.t.Helper()
	p, err := e.post(room, m, o)
	e.must(err)
	return p.EventId
}

// feedRow は feed の 1 行。
type feedRow struct {
	pos     int64
	kind    string
	ref     uuid.UUID
	room    *uuid.UUID
	payload map[string]any
}

// feedOf は利用者の feed の行（種類で絞る。空なら全部）。
func (e *env) feedOf(user uuid.UUID, kind string) []feedRow {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT pos, kind, ref_id, room_id, payload FROM feed WHERE user_id = $1 AND ($2 = '' OR kind = $2) ORDER BY pos`, user, kind)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []feedRow
	for rows.Next() {
		var r feedRow
		var p []byte
		if err := rows.Scan(&r.pos, &r.kind, &r.ref, &r.room, &p); err != nil {
			e.t.Fatal(err)
		}
		if p != nil {
			_ = json.Unmarshal(p, &r.payload)
		}
		out = append(out, r)
	}
	return out
}

// roomEvents は利用者の feed の room.event の識別子（トークで絞る）。
func (e *env) roomEvents(user, room uuid.UUID) []uuid.UUID {
	e.t.Helper()
	var out []uuid.UUID
	for _, r := range e.feedOf(user, kindRoomEvent) {
		if r.room != nil && *r.room == room {
			out = append(out, r.ref)
		}
	}
	return out
}

// members はトークの構成員の状態（利用者 → 状態）。
func (e *env) members(room uuid.UUID) map[uuid.UUID]MemberState {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT user_id, state FROM room_members WHERE room_id = $1`, room)
	e.must(err)
	defer rows.Close()
	out := map[uuid.UUID]MemberState{}
	for rows.Next() {
		var u uuid.UUID
		var s string
		e.must(rows.Scan(&u, &s))
		out[u] = MemberState(s)
	}
	return out
}

// systemKinds はトークの ft.event.system/1 の kind（出た順）。
func (e *env) systemKinds(room uuid.UUID) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT plain->>'kind' FROM events WHERE room_id = $1 AND schema = 'ft.event.system/1' ORDER BY id`, room)
	e.must(err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		e.must(rows.Scan(&k))
		out = append(out, k)
	}
	return out
}

// memberChanges は利用者の feed の room.member の change（トークで絞る。出た順）。
func (e *env) memberChanges(user, room, of uuid.UUID) []string {
	e.t.Helper()
	var out []string
	for _, r := range e.feedOf(user, kindRoomMember) {
		if r.room != nil && *r.room == room && r.payload["user_id"] == of.String() && r.payload["device_id"] == nil {
			out = append(out, r.payload["change"].(string))
		}
	}
	return out
}

// within は fn を別の goroutine で走らせ、d の間に終わらなければ偽（錠で待っている）。終わった時の誤りは ch に入る。
func within(fn func() error) chan error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return ch
}

// waiting は ch が d の間に値を返さないこと（錠で待っている）を確かめる。
func (e *env) waiting(ch chan error, d time.Duration) {
	e.t.Helper()
	select {
	case err := <-ch:
		e.t.Fatalf("錠で待つはずの処理が終わった: %v", err)
	case <-time.After(d):
	}
}

// heldWhileWaiting は、試験が low の錠を持つ間に fn を走らせ、fn が low で待つ時に high の錠を既に持っているか（全体実装設計書 2.4、
// I-18 の確かめ）。試験が持つトランザクションは t.Cleanup でも戻す（落ちた時に持ったまま抜けると pool の Close が待ち続ける）。
func (e *env) heldWhileWaiting(low, high uuid.UUID, fn func() error) bool {
	e.t.Helper()
	hold, err := e.pool.Begin(e.ctx)
	e.must(err)
	e.t.Cleanup(func() { hold.Rollback(context.Background()) })
	e.must(store.RoomLock{}.Lock(e.ctx, hold, low))
	ch := within(fn)
	e.waiting(ch, 500*time.Millisecond)
	probe, err := e.pool.Begin(e.ctx)
	e.must(err)
	_, perr := probe.Exec(e.ctx, `SELECT 1 FROM rooms WHERE id = $1 FOR UPDATE NOWAIT`, high)
	probe.Rollback(e.ctx)
	hold.Rollback(e.ctx)
	select {
	case err := <-ch:
		e.must(err)
	case <-time.After(10 * time.Second):
		e.t.Fatal("錠の後の処理が終わらない")
	}
	return perr != nil
}

// pendingKind はトークのその端末の待ちの種類（無ければ空）。
func (e *env) pendingKind(room, dev uuid.UUID) string {
	e.t.Helper()
	if e.count(`SELECT count(*) FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, dev) == 0 {
		return ""
	}
	var kind string
	e.scalar(&kind, `SELECT kind FROM mls_pending WHERE room_id = $1 AND device_id = $2`, room, dev)
	return kind
}
