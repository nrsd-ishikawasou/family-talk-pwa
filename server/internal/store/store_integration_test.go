// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgconn"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store/migrations"
)

// 試験の postgres（全試験で 1 つ）。
var (
	testInfo ConnInfo
	testPool *Pool
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	c, err := tcpg.Run(ctx, "postgres:18",
		tcpg.WithDatabase(DbName), tcpg.WithUsername(DbUser), tcpg.WithPassword("test-pw"), tcpg.BasicWaitStrategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres を起こせない:", err)
		os.Exit(1)
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "5432/tcp")
	p, _ := strconv.Atoi(port.Port())
	testInfo = ConnInfo{Host: host, Port: uint16(p), Database: DbName, User: DbUser, Password: []byte("test-pw")}
	testPool, err = Open(ctx, testInfo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "接続できない:", err)
		os.Exit(1)
	}
	files, _ := fs.Glob(migrations.FS, "*.sql")
	rep, err := NewMigrator(testPool).Apply(ctx)
	if err != nil || rep.To != len(files) {
		fmt.Fprintln(os.Stderr, "移行できない:", err, rep)
		os.Exit(1)
	}
	code := m.Run()
	testPool.Close()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

func newIds() *ids.Ids { return ids.New(clock.WallClock{}, rand.Reader) }

// R: R-1-3-2
func TestMigratorIsAppliedOnceAndDetectsDowngrade(t *testing.T) {
	ctx := context.Background()
	m := NewMigrator(testPool)
	files, _ := fs.Glob(migrations.FS, "*.sql")
	rep, err := m.Apply(ctx)
	if err != nil || rep.From != len(files) || rep.To != len(files) || len(rep.Applied) != 0 {
		t.Fatalf("2 回目の Apply が何かを当てた: %+v %v", rep, err)
	}
	if p, _ := m.Pending(ctx); len(p) != 0 {
		t.Fatalf("残りがある: %v", p)
	}
	if err := m.RequireVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE schema_migrations SET version = 99`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = testPool.Exec(ctx, `UPDATE schema_migrations SET version = $1`, len(files)) }()
	if err := m.RequireVersion(ctx); !errors.Is(err, errDowngrade) {
		t.Fatalf("ダウングレードを止めない: %v", err)
	}
}

// R: R-2-2-3
func TestFlushHasNoGapsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	f := NewFeed(testPool)
	user := uuid.Must(uuid.NewV7())
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := testPool.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			ref := FeedRef{Kind: "notice", Id: uuid.FromStringOrNil(fmt.Sprintf("01890a5d-ac96-774b-bcce-%012d", i)),
				Payload: json.RawMessage(`{"kind":"capacity","level":"info","text_key":"k","params":{}}`), Ts: time.Unix(1_800_000_000, 0)}
			p := f.Append(tx, user, ref)
			if i%10 == 0 {
				return // 取り消す：溜めた行は捨てられ、採番は起きない
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
				return
			}
			if p.Pos < 1 {
				errs <- fmt.Errorf("コミットの後に pos が入らない: %d", p.Pos)
				return
			}
			errs <- nil
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.ReadSince(ctx, user, uuid.Nil, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 90 {
		t.Fatalf("コミットした数と違う: %d", len(rows))
	}
	for i, r := range rows {
		if r.Pos != int64(i+1) {
			t.Fatalf("欠番がある: %d 番目が %d", i+1, r.Pos)
		}
	}
	if last, err := f.LastPos(ctx, testPool, user); err != nil || last != 90 {
		t.Fatalf("最新の pos が違う: %d %v", last, err)
	}
	if last, err := f.LastPos(ctx, testPool, uuid.Must(uuid.NewV7())); err != nil || last != 0 {
		t.Fatalf("行の無い利用者の最新の pos が 0 でない: %d %v", last, err)
	}
	if min, _ := f.MinPos(ctx, user); min != 1 {
		t.Fatalf("MinPos が違う: %d", min)
	}
	if n, _ := f.Prune(ctx, time.Unix(1_800_000_001, 0)); n != 90 {
		t.Fatalf("Prune の数が違う: %d", n)
	}
	if min, _ := f.MinPos(ctx, user); min != 91 {
		t.Fatalf("全部消えた後の MinPos が次の pos でない: %d", min)
	}
}

// R: R-1-2-2
func TestJobsClaimIsExclusiveAndClaimIdGuards(t *testing.T) {
	ctx := context.Background()
	c := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	j := NewJobs(testPool, newIds(), c)
	tx, _ := testPool.Begin(ctx)
	id1, err := j.Enqueue(ctx, tx, "gc", map[string]any{}, c.Now(), "gc-daily")
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := j.Enqueue(ctx, tx, "gc", map[string]any{}, c.Now(), "gc-daily")
	if id1 != id2 {
		t.Fatal("同じ重複の鍵で 2 つ積んだ")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 2 つが同時に取りに行っても 1 つだけ取れる
	var got []*Job
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jb, err := j.Claim(ctx, []string{"gc"}, c.Now(), 10*time.Minute)
			if err == nil && jb != nil {
				mu.Lock()
				got = append(got, jb)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 1 || got[0].ClaimId == uuid.Nil {
		t.Fatalf("取れた数が 1 でない: %d", len(got))
	}
	first := *got[0]
	if err := j.Heartbeat(ctx, first, c.Now(), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	// 錠が切れて取り直される
	c.Advance(11 * time.Minute)
	if n, _ := j.Recover(ctx, c.Now()); n != 1 {
		t.Fatalf("Recover の数が違う: %d", n)
	}
	second, _ := j.Claim(ctx, []string{"gc"}, c.Now(), 10*time.Minute)
	if second == nil || second.ClaimId == first.ClaimId {
		t.Fatal("取り直せない")
	}
	// 最初の worker の Complete と Heartbeat は効かない
	tx, _ = testPool.Begin(ctx)
	if err := j.Complete(ctx, tx, first); !errors.Is(err, ErrJobLost) {
		t.Fatalf("取り直された仕事を完了にした: %v", err)
	}
	tx.Rollback(ctx)
	if err := j.Heartbeat(ctx, first, c.Now(), time.Minute); !errors.Is(err, ErrJobLost) {
		t.Fatal("取り直された仕事の錠を延ばした")
	}
	tx, _ = testPool.Begin(ctx)
	if err := j.Complete(ctx, tx, *second); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	st, _ := j.Stats(ctx)
	if st.LastDoneAt == nil || !st.LastDoneAt.Equal(c.Now()) {
		t.Fatalf("最後の完了の時刻が違う: %+v", st)
	}
	c.Advance(31 * 24 * time.Hour)
	if n, _ := j.Prune(ctx, c.Now().Add(-30*24*time.Hour)); n != 1 {
		t.Fatalf("終わった仕事を消さない: %d", n)
	}
}

func TestJobsCancelRemovesOnlyQueued(t *testing.T) {
	ctx := context.Background()
	c := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	j := NewJobs(testPool, newIds(), c)
	tx, _ := testPool.Begin(ctx)
	_, _ = j.Enqueue(ctx, tx, "erasure", map[string]any{"user_id": "a"}, c.Now().Add(time.Hour), "erasure:cancel-a")
	_, _ = j.Enqueue(ctx, tx, "erasure", map[string]any{"user_id": "b"}, c.Now(), "erasure:cancel-b")
	_ = tx.Commit(ctx)
	jb, _ := j.Claim(ctx, []string{"erasure"}, c.Now(), time.Minute)
	if jb == nil {
		t.Fatal("取れない")
	}
	tx, _ = testPool.Begin(ctx)
	okA, errA := j.Cancel(ctx, tx, "erasure:cancel-a")
	okB, errB := j.Cancel(ctx, tx, "erasure:cancel-b")
	okC, errC := j.Cancel(ctx, tx, "erasure:cancel-none")
	_ = tx.Commit(ctx)
	if errors.Join(errA, errB, errC) != nil || !okA || okB || okC {
		t.Fatalf("queued だけを消す：a=%v b=%v none=%v %v", okA, okB, okC, errors.Join(errA, errB, errC))
	}
	var n int
	_ = testPool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE dedupe_key IN ('erasure:cancel-a', 'erasure:cancel-b')`).Scan(&n)
	if n != 1 {
		t.Fatalf("running の仕事は残る：%d", n)
	}
}

func TestJobsFailThreeTimes(t *testing.T) {
	ctx := context.Background()
	c := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	j := NewJobs(testPool, newIds(), c)
	tx, _ := testPool.Begin(ctx)
	_, _ = j.Enqueue(ctx, tx, "ddns", map[string]any{"a": 1}, c.Now(), "")
	_ = tx.Commit(ctx)
	for i := 1; i <= 3; i++ {
		jb, _ := j.Claim(ctx, []string{"ddns"}, c.Now(), time.Minute)
		if jb == nil {
			t.Fatalf("%d 回目に取れない", i)
		}
		if err := j.Fail(ctx, *jb, "NET-004", c.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if jb, _ := j.Claim(ctx, []string{"ddns"}, c.Now(), time.Minute); jb != nil {
		t.Fatal("3 回失敗した仕事がまた取れた")
	}
	if st, _ := j.Stats(ctx); st.Failed < 1 {
		t.Fatalf("failed の数が違う: %+v", st)
	}
}

// R: R-2-2-4
func TestIdempotencyConcurrentDuplicatesProcessOnce(t *testing.T) {
	ctx := context.Background()
	c := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	idem := NewIdempotency(testPool, c)
	user, key := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()).String()
	hash := sha256.Sum256([]byte(`{"body":1}`))
	tx1, _ := testPool.Begin(ctx)
	if st, err := idem.Begin(ctx, tx1, user, key, hash); err != nil || st != nil {
		t.Fatalf("1 つ目が処理に進めない: %v %v", st, err)
	}
	result := make(chan *Stored, 1)
	go func() {
		tx2, _ := testPool.Begin(ctx)
		defer tx2.Rollback(ctx)
		st, _ := idem.Begin(ctx, tx2, user, key, hash) // 1 つ目のコミットを待つ
		result <- st
	}()
	time.Sleep(200 * time.Millisecond) // 2 つ目が一意索引で待つまで（試験の待ち。時刻の読みではない）
	select {
	case <-result:
		t.Fatal("2 つ目が 1 つ目のコミットを待たない")
	default:
	}
	if err := idem.Finish(ctx, tx1, user, key, 201, []byte(`{"id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	st := <-result
	if st == nil || st.Status != 201 || string(st.Body) != `{"id":"x"}` {
		t.Fatalf("2 つ目が入れた応答を受けない: %+v", st)
	}
	tx3, _ := testPool.Begin(ctx)
	defer tx3.Rollback(ctx)
	if _, err := idem.Begin(ctx, tx3, user, key, sha256.Sum256([]byte("違う本文"))); !isCode(err, gen.NET008) {
		t.Fatalf("本文が違う再送を NET-008 にしない: %v", err)
	}
}

func TestIdempotencyRollbackAllowsRetry(t *testing.T) {
	ctx := context.Background()
	idem := NewIdempotency(testPool, clock.NewTestClock(time.Unix(1_800_000_000, 0)))
	user, key := uuid.Must(uuid.NewV7()), "k"
	hash := sha256.Sum256([]byte("b"))
	tx, _ := testPool.Begin(ctx)
	_, _ = idem.Begin(ctx, tx, user, key, hash)
	tx.Rollback(ctx) // 処理の失敗
	tx, _ = testPool.Begin(ctx)
	defer tx.Rollback(ctx)
	if st, err := idem.Begin(ctx, tx, user, key, hash); err != nil || st != nil {
		t.Fatalf("取り消した後の再送が処理に進めない: %v %v", st, err)
	}
}

// R: R-2-2-3
func TestNotifyArrivesOnlyOnCommitAndReconnectSignals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	n := NewNotifier(testPool, c)
	ch, err := n.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u1, u2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tx, _ := testPool.Begin(ctx)
	_ = n.Notify(ctx, tx, u1)
	tx.Rollback(ctx)
	tx, _ = testPool.Begin(ctx)
	_ = n.Notify(ctx, tx, u2)
	_ = n.Notify(ctx, tx, u2) // 同じ中身はまとまる
	_ = tx.Commit(ctx)
	select {
	case got := <-ch:
		if got != u2 {
			t.Fatalf("取り消したトランザクションの知らせが届いた、または違う: %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("コミットした知らせが届かない")
	}
	// 待ちの接続を切ると、再接続の後に uuid.Nil が届く
	if _, err := testPool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
WHERE query LIKE 'LISTEN%' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		c.Advance(time.Second)
		select {
		case got := <-ch:
			if got == uuid.Nil {
				return
			}
		case <-deadline:
			t.Fatal("再接続の合図が届かない")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestRoomLockSerializesAndMissingRoom(t *testing.T) {
	ctx := context.Background()
	room := uuid.Must(uuid.NewV7())
	if _, err := testPool.Exec(ctx, `INSERT INTO rooms (id, kind, state, last_event_at, created_at) VALUES ($1, 'group', 'normal', now(), now())`, room); err != nil {
		t.Fatal(err)
	}
	var lock RoomLock
	tx1, _ := testPool.Begin(ctx)
	if err := lock.Lock(ctx, tx1, room); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		tx2, _ := testPool.Begin(ctx)
		defer tx2.Rollback(ctx)
		_ = lock.Lock(ctx, tx2, room)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("2 つ目の錠が待たない")
	case <-time.After(200 * time.Millisecond):
	}
	tx1.Rollback(ctx)
	<-done
	tx3, _ := testPool.Begin(ctx)
	defer tx3.Rollback(ctx)
	if err := lock.Lock(ctx, tx3, uuid.Must(uuid.NewV7())); !isCode(err, gen.ROOM001) {
		t.Fatalf("無いトークを ROOM-001 にしない: %v", err)
	}
}

func TestRetentionDeletesInBatches(t *testing.T) {
	ctx := context.Background()
	_, _ = testPool.Exec(ctx, `CREATE TABLE ret_test (created_at timestamptz NOT NULL)`)
	_, _ = testPool.Exec(ctx, `INSERT INTO ret_test SELECT to_timestamp(1800000000 - g) FROM generate_series(1, 25000) g`)
	n, err := NewRetention(testPool).DeleteOlder(ctx, "ret_test", "created_at", time.Unix(1_800_000_000-5000, 0))
	if err != nil || n != 20000 {
		t.Fatalf("消した数が違う: %d %v", n, err)
	}
}

func TestAfterCommitRunsOnlyOnCommit(t *testing.T) {
	ctx := context.Background()
	ran := 0
	tx, _ := testPool.Begin(ctx)
	tx.AfterCommit(func() { ran++ })
	tx.Rollback(ctx)
	tx, _ = testPool.Begin(ctx)
	tx.AfterCommit(func() { ran += 10 })
	_ = tx.Commit(ctx)
	tx.Rollback(ctx) // コミット済みなら何もしない
	if ran != 10 {
		t.Fatalf("AfterCommit の呼ばれ方が違う: %d", ran)
	}
}

// TestErr_SRV_004 はエラーコード表の test の欄の名（tools/errors_check が照らす）。届かない DB は SRV-004、params.service は postgres。
func TestErr_SRV_004(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	_, err := Open(context.Background(), ConnInfo{Host: "127.0.0.1", Port: uint16(port), Database: DbName, User: DbUser, Password: []byte("x")})
	if !isCode(err, gen.SRV004) {
		t.Fatalf("届かない DB を SRV-004 にしない: %v", err)
	}
	var ev *fault.Event
	if !errors.As(err, &ev) || ev.Params["service"] != "postgres" || !ev.Retryable || ev.Status != 503 {
		t.Fatalf("SRV-004 の形が違う: %+v", ev)
	}
}

func TestInTxCommitsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `CREATE TABLE intx (n int)`); err != nil {
		t.Fatal(err)
	}
	_ = testPool.InTx(ctx, func(tx *Tx) error { _, err := tx.Exec(ctx, `INSERT INTO intx VALUES (1)`); return err })
	_ = testPool.InTx(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO intx VALUES (2)`); err != nil {
			return err
		}
		return errors.New("取り消す")
	})
	outer, _ := testPool.Begin(ctx)
	_ = testPool.InTx(WithTx(ctx, outer), func(tx *Tx) error { _, err := tx.Exec(ctx, `INSERT INTO intx VALUES (3)`); return err })
	outer.Rollback(ctx) // 外側が取り消せば中も消える
	var n int
	if err := testPool.QueryRow(ctx, `SELECT coalesce(sum(n), 0) FROM intx`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("残った行の和が違う: %d", n)
	}
}

// R: R-2-3-2
func TestFeedReadSinceFiltersByDevice(t *testing.T) {
	ctx := context.Background()
	f := NewFeed(testPool)
	user := uuid.Must(uuid.NewV7())
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	put := func(kind string, dev uuid.UUID, payload string) int64 {
		t.Helper()
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		ref := FeedRef{Kind: kind, Id: uuid.Must(uuid.NewV7()), DeviceId: dev, Payload: json.RawMessage(payload), Ts: time.Unix(1_800_000_000, 0)}
		p := f.Append(tx, user, ref)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return p.Pos
	}
	put("notice", uuid.Nil, `{"kind":"capacity","level":"info","text_key":"k","params":{}}`)
	put("device.inbox", a, `{"count":1}`)
	put("session.revoked", b, `{"reason":"logout"}`)
	put("sync.gap", uuid.Nil, `{"reason":"restore"}`)
	put("sync.gap", a, `{"reason":"restore"}`)
	want := map[uuid.UUID][]int64{a: {1, 2, 4, 5}, b: {1, 3, 4}}
	for dev, poss := range want {
		rows, err := f.ReadSince(ctx, user, dev, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, r := range rows {
			got = append(got, r.Pos)
			if r.Ref.DeviceId != uuid.Nil && r.Ref.DeviceId != dev {
				t.Fatalf("他の端末宛ての行が読めた: %v", r)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(poss) {
			t.Fatalf("端末 %s に届く行が違う: %v（期待 %v）", dev, got, poss)
		}
	}
	// 表の CHECK：宛先の無い session.revoked と、宛先を持つ notice は入らない（Append の確かめをすり抜けても止まる）。
	for _, c := range []struct {
		kind string
		dev  any
	}{{"session.revoked", nil}, {"notice", a}} {
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO feed (user_id, pos, kind, ref_id, device_id, payload, ts) VALUES ($1, 100, $2, $3, $4, '{}', now())`,
			user, c.kind, uuid.Must(uuid.NewV7()), c.dev)
		tx.Rollback(ctx)
		if err == nil {
			t.Fatalf("%s の宛先の規則を CHECK が止めない", c.kind)
		}
	}
}

// noticeRef は試験用の notice の行。
func noticeRef(id int) FeedRef {
	return FeedRef{Kind: "notice", Id: uuid.FromStringOrNil(fmt.Sprintf("01890a5d-ac96-774b-bcce-%012d", id)),
		Payload: json.RawMessage(`{"kind":"capacity","level":"info","text_key":"k","params":{}}`), Ts: time.Unix(1_800_000_000, 0)}
}

// R: R-2-2-3
// 宛先を逆の順に持つ同時の 2 つの書き込み（2 構成員が同時に自分の他の端末を取り消す形。全体実装設計書 2.4）。
// 採番の錠をコミットの直前に識別子の順で取るので、両方がコミットし、デッドロック（40P01）は起きない。
func TestFlushTakesCursorLocksInIdOrderAcrossTransactions(t *testing.T) {
	ctx := context.Background()
	f := NewFeed(testPool)
	a, c := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if bytes.Compare(a[:], c[:]) > 0 {
		a, c = c, a
	}
	barrier := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	run := func(first, second uuid.UUID, id int) error {
		tx, err := testPool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		f.Append(tx, first, noticeRef(id)) // 本人のその端末へ（session.revoked の形）
		ready.Done()
		<-barrier
		f.Append(tx, first, noticeRef(id+1)) // 家族の全員へ（device.changed の形）
		f.Append(tx, second, noticeRef(id+1))
		return tx.Commit(ctx)
	}
	errs := make(chan error, 2)
	go func() { errs <- run(a, c, 1000) }()
	go func() { errs <- run(c, a, 2000) }()
	ready.Wait()
	close(barrier)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("同時の 2 つの書き込みのどちらかが失敗した（デッドロックなら SRV-003）: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("同時の 2 つの書き込みが終わらない")
		}
	}
	for _, u := range []uuid.UUID{a, c} {
		rows, err := f.ReadSince(ctx, u, uuid.Nil, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 3 || rows[0].Pos != 1 || rows[1].Pos != 2 || rows[2].Pos != 3 {
			t.Fatalf("両方の書き込みが連続の pos で入っていない: %+v", rows)
		}
	}
}

// R: R-2-2-3
// 同じ利用者への複数の行は積んだ順の連続の pos。明示の Flush は pos をコミットの前に入れ、Commit は二重に書かない。
// Rollback は溜めた行を捨てる（採番は起きない）。
func TestFlushOrdersRowsWithinUserAndRollbackDiscards(t *testing.T) {
	ctx := context.Background()
	f := NewFeed(testPool)
	user := uuid.Must(uuid.NewV7())
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	p1 := f.Append(tx, user, noticeRef(3001))
	p2 := f.Append(tx, user, noticeRef(3002))
	if err := f.Flush(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if p1.Pos != 1 || p2.Pos != 2 {
		t.Fatalf("明示の Flush が積んだ順の連続の pos を入れない: %d %d", p1.Pos, p2.Pos)
	}
	p3 := f.Append(tx, user, noticeRef(3003))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if p3.Pos != 3 {
		t.Fatalf("Commit が Flush の後に溜めた行を書かない: %d", p3.Pos)
	}
	rows, err := f.ReadSince(ctx, user, uuid.Nil, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Ref.Id != p1.Ref.Id || rows[1].Ref.Id != p2.Ref.Id || rows[2].Ref.Id != p3.Ref.Id {
		t.Fatalf("行の順か数が違う（二重か欠け）: %+v", rows)
	}
	other := uuid.Must(uuid.NewV7())
	tx2, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := f.Append(tx2, other, noticeRef(3004))
	tx2.Rollback(ctx)
	if last, err := f.LastPos(ctx, testPool, other); err != nil || last != 0 || q.Pos != 0 {
		t.Fatalf("Rollback が溜めた行を捨てない: last=%d pos=%d %v", last, q.Pos, err)
	}
}

// R: R-2-2-4
// /sync の初回の一覧（srv-rooms Timeline.Snapshot）が使う 1 つの見え方の読み。見え方の後にコミットした行と pos は、同じ
// トランザクションの後の文に見えない（READ COMMITTED なら見える）。書き込みは DB が拒む（25006）。
func TestBeginSnapshotKeepsOneViewAndRejectsWrites(t *testing.T) {
	ctx := context.Background()
	f := NewFeed(testPool)
	user := uuid.Must(uuid.NewV7())
	put := func(id int) {
		t.Helper()
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		f.Append(tx, user, noticeRef(id))
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	put(9001)
	snap, err := testPool.BeginSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Rollback(ctx)
	count := func() int {
		t.Helper()
		var n int
		if err := snap.QueryRow(ctx, `SELECT count(*) FROM feed WHERE user_id = $1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if last, err := f.LastPos(ctx, snap, user); err != nil || last != 1 || count() != 1 {
		t.Fatalf("見え方の始めの pos と行の数が違う: %d %v", last, err)
	}
	put(9002)
	if last, err := f.LastPos(ctx, snap, user); err != nil || last != 1 || count() != 1 {
		t.Fatalf("見え方の後にコミットした行が見える: %d %v", last, err)
	}
	if last, err := f.LastPos(ctx, testPool, user); err != nil || last != 2 {
		t.Fatalf("Pool の読みが最新でない: %d %v", last, err)
	}
	_, err = snap.Exec(ctx, `UPDATE feed_cursor SET next_pos = next_pos WHERE user_id = $1`, user)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "25006" {
		t.Fatalf("読み専用の書き込みを拒まない: %v", err)
	}
}

// LastRoomEventPos はトークごとの最新の room.event の pos を返し、他の種類（room.member など）と他の利用者の行を数えない。
// 行の無いトークは地図に入らない。読みは部分索引 feed_room_event を使う。
func TestFeedLastRoomEventPosPerRoom(t *testing.T) {
	ctx := context.Background()
	f := NewFeed(testPool)
	user, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	r1, r2, r3 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	put := func(to uuid.UUID, kind string, room uuid.UUID) int64 {
		t.Helper()
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		ref := FeedRef{Kind: kind, Id: uuid.Must(uuid.NewV7()), RoomId: room, Ts: time.Unix(1_800_000_000, 0)}
		if kind != "room.event" {
			ref.Payload = json.RawMessage(`{}`)
		}
		p := f.Append(tx, to, ref)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return p.Pos
	}
	put(user, "room.event", r1)
	last1 := put(user, "room.event", r1)
	put(user, "room.member", r1)
	last2 := put(user, "room.event", r2)
	put(user, "room.settings", r3)
	put(other, "room.event", r3)
	got, err := f.LastRoomEventPos(ctx, testPool, user, []uuid.UUID{r1, r2, r3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[r1] != last1 || got[r2] != last2 {
		t.Fatalf("トークごとの最新の pos が違う: %v（期待 r1=%d r2=%d、r3 なし）", got, last1, last2)
	}
	if empty, err := f.LastRoomEventPos(ctx, testPool, user, nil); err != nil || len(empty) != 0 {
		t.Fatalf("空のトークの一覧: %v %v", empty, err)
	}
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	var plan bytes.Buffer
	rows, err := tx.Query(ctx, `EXPLAIN SELECT r.id, l.pos FROM unnest($2::uuid[]) AS r(id)
CROSS JOIN LATERAL (SELECT pos FROM feed WHERE user_id = $1 AND room_id = r.id AND kind = 'room.event' ORDER BY pos DESC LIMIT 1) AS l`,
		user, []uuid.UUID{r1, r2})
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !bytes.Contains(plan.Bytes(), []byte("feed_room_event")) {
		t.Fatalf("部分索引 feed_room_event を使わない:\n%s", plan.String())
	}
}
