// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/ids"
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
	rep, err := NewMigrator(testPool).Apply(ctx)
	if err != nil || rep.To != 1 {
		fmt.Fprintln(os.Stderr, "移行できない:", err, rep)
		os.Exit(1)
	}
	if _, err := testPool.Exec(ctx, `CREATE TABLE rooms (id uuid PRIMARY KEY)`); err != nil {
		fmt.Fprintln(os.Stderr, err)
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
	rep, err := m.Apply(ctx)
	if err != nil || rep.From != 1 || rep.To != 1 || len(rep.Applied) != 0 {
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
	defer func() { _, _ = testPool.Exec(ctx, `UPDATE schema_migrations SET version = 1`) }()
	if err := m.RequireVersion(ctx); !errors.Is(err, errDowngrade) {
		t.Fatalf("ダウングレードを止めない: %v", err)
	}
}

// R: R-2-2-3
func TestNextPosHasNoGapsUnderConcurrency(t *testing.T) {
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
			pos, err := f.NextPos(ctx, tx, user)
			if err != nil {
				errs <- err
				return
			}
			ref := FeedRef{Kind: "notice", Id: uuid.FromStringOrNil(fmt.Sprintf("01890a5d-ac96-774b-bcce-%012d", i)),
				Payload: json.RawMessage(`{"kind":"capacity","level":"info","text_key":"k","params":{}}`), Ts: time.Unix(1_800_000_000, 0)}
			if err := f.Append(ctx, tx, user, pos, ref); err != nil {
				errs <- err
				return
			}
			if i%10 == 0 {
				return // 取り消す：採番も戻る
			}
			errs <- tx.Commit(ctx)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.ReadSince(ctx, user, 0, 1000)
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
	if last, err := f.LastPos(ctx, user); err != nil || last != 90 {
		t.Fatalf("最新の pos が違う: %d %v", last, err)
	}
	if last, err := f.LastPos(ctx, uuid.Must(uuid.NewV7())); err != nil || last != 0 {
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
	_, _ = testPool.Exec(ctx, `INSERT INTO rooms (id) VALUES ($1)`, room)
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
