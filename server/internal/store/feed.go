// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
)

// batchRows は大きな削除の 1 回の行の数（長い錠を持たない）。
const batchRows = 10000

// feedKinds は feed に書く種類（第2章 6.2 の pos を持つ 18 種）。
var feedKinds = map[string]bool{
	"room.event": true, "room.receipt": true, "room.member": true, "room.settings": true, "room.me": true,
	"room.mls": true, "room.scheduled": true, "device.inbox": true, "device.changed": true, "user.changed": true,
	"keybundle.changed": true, "recovery.request": true, "recovery.used": true, "call.ring": true,
	"call.state": true, "session.revoked": true, "notice": true, "sync.gap": true,
}

// deviceKinds は宛先の端末を持つ種類（全体実装設計書 2.4）。true は必須、false は任意（空なら本人の全端末）。他の種類は空。
var deviceKinds = map[string]bool{"session.revoked": true, "device.inbox": true, "sync.gap": false}

// FeedRef は feed の 1 行の中身（第2章 6.2 の pos を持つ 18 種）。
type FeedRef struct {
	Kind string
	// Id はフレームの id（本体の行の識別子。本体を持たない種類は積む側が作る UUIDv7）。必須。
	Id uuid.UUID
	// RoomId は room.* の種類だけが持つ（フレームの room_id）。
	RoomId uuid.UUID
	// DeviceId は宛先の端末（session.revoked と device.inbox は必須、sync.gap は任意、他は空）。フレームには出ない。
	DeviceId uuid.UUID
	// Payload は room.event では空（hub と /sync が本体から引く）。他の種類では必須。
	Payload json.RawMessage
	Ts      time.Time
}

// checkRef は種類と欄の組を確かめる。違えばプログラムの誤り。
func checkRef(ref FeedRef) {
	if !feedKinds[ref.Kind] {
		panic("store: feed に書けない種類: " + ref.Kind)
	}
	if ref.Id == uuid.Nil {
		panic("store: feed の id が無い: " + ref.Kind)
	}
	if strings.HasPrefix(ref.Kind, "room.") != (ref.RoomId != uuid.Nil) {
		panic("store: feed の room_id は room.* の種類だけが持つ: " + ref.Kind)
	}
	if required, ok := deviceKinds[ref.Kind]; ok {
		if required && ref.DeviceId == uuid.Nil {
			panic("store: feed の宛先の端末が無い: " + ref.Kind)
		}
	} else if ref.DeviceId != uuid.Nil {
		panic("store: feed の宛先の端末を持てない種類: " + ref.Kind)
	}
	if (ref.Kind == "room.event") != (len(ref.Payload) == 0) {
		panic("store: feed の payload は room.event では空、他の種類では必須: " + ref.Kind)
	}
}

// FeedRow は feed の 1 行。
type FeedRow struct {
	Pos int64
	Ref FeedRef
}

// FeedPending はトランザクションに溜めた feed の 1 行。Pos は Flush が入れる（それまで 0）。
type FeedPending struct {
	UserId uuid.UUID
	Ref    FeedRef
	Pos    int64
}

// Feed は利用者ごとの配信の列（B-022。全体実装設計書 2.4）。
type Feed struct {
	pool *Pool
}

// NewFeed は Feed を作る。
func NewFeed(pool *Pool) *Feed {
	return &Feed{pool: pool}
}

// Append は feed の 1 行をトランザクションに溜める（記録には触れない）。採番と書き込みはコミットの直前の Flush が、
// 宛先の利用者の識別子の順に行う（採番の錠の順。全体実装設計書 2.4）。種類と欄の組が違えばプログラムの誤り（checkRef）。
// 戻りの Pos は Flush の後に入る。
func (f *Feed) Append(tx *Tx, userId uuid.UUID, ref FeedRef) *FeedPending {
	checkRef(ref)
	if userId == uuid.Nil {
		panic("store: feed の宛先の利用者が無い: " + ref.Kind)
	}
	if tx.done {
		panic("store: 終わったトランザクションに feed を溜めた: " + ref.Kind)
	}
	p := &FeedPending{UserId: userId, Ref: ref}
	tx.feed = append(tx.feed, p)
	return p
}

// Flush は溜めた行を、宛先の利用者の識別子の順に、利用者ごとに 1 回の feed_cursor の更新で連続の pos を採って feed に書き、
// 利用者ごとに 1 回 pg_notify する。feed_cursor の更新（upsert）は行の錠をトランザクションの終わりまで持つので、
// 同じ利用者への同時の書き込みは直列になり、rollback で採番も戻る（欠番なし。I-3）。
// Tx.Commit が呼ぶ。応答に pos を要する操作だけが操作の最後に自分で呼び、その後は行の錠を取らない（全体実装設計書 2.4）。
func (f *Feed) Flush(ctx context.Context, tx *Tx) error {
	return tx.flushFeed(ctx)
}

// flushFeed は Flush の本体（Commit からも呼ぶ）。
func (t *Tx) flushFeed(ctx context.Context) error {
	if len(t.feed) == 0 {
		return nil
	}
	pending := t.feed
	t.feed = nil
	// 安定な並べ替え：利用者は識別子の順、同じ利用者の中は積んだ順。
	slices.SortStableFunc(pending, func(a, b *FeedPending) int { return bytes.Compare(a.UserId[:], b.UserId[:]) })
	for i := 0; i < len(pending); {
		user := pending[i].UserId
		j := i
		for j < len(pending) && pending[j].UserId == user {
			j++
		}
		n := int64(j - i)
		var first int64
		err := t.QueryRow(ctx, `INSERT INTO feed_cursor (user_id, next_pos) VALUES ($1, $2::bigint + 1)
ON CONFLICT (user_id) DO UPDATE SET next_pos = feed_cursor.next_pos + $2::bigint
RETURNING next_pos - $2::bigint`, user, n).Scan(&first)
		if err != nil {
			return unavailable(err)
		}
		for k := i; k < j; k++ {
			pending[k].Pos = first + int64(k-i)
			if err := insertFeedRow(ctx, t, pending[k]); err != nil {
				return err
			}
		}
		if err := notify(ctx, t, user); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// insertFeedRow は feed に 1 行を書く。
func insertFeedRow(ctx context.Context, tx *Tx, p *FeedPending) error {
	ref := p.Ref
	var roomId any
	if ref.RoomId != uuid.Nil {
		roomId = ref.RoomId
	}
	var deviceId any
	if ref.DeviceId != uuid.Nil {
		deviceId = ref.DeviceId
	}
	var payload any
	if len(ref.Payload) > 0 {
		payload = []byte(ref.Payload)
	}
	_, err := tx.Exec(ctx, `INSERT INTO feed (user_id, pos, kind, ref_id, room_id, device_id, payload, ts) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.UserId, p.Pos, ref.Kind, ref.Id, roomId, deviceId, payload, ref.Ts.UTC())
	if err != nil {
		return unavailable(err)
	}
	return nil
}

// ReadSince は since より後の行のうち、deviceId に届く物（宛先の端末が空か deviceId と同じ。全体実装設計書 2.4）を pos の順に limit まで（/sync と hub）。
func (f *Feed) ReadSince(ctx context.Context, userId, deviceId uuid.UUID, since int64, limit int) ([]FeedRow, error) {
	rows, err := f.pool.Query(ctx, `SELECT pos, kind, ref_id, room_id, device_id, payload, ts FROM feed
WHERE user_id = $1 AND pos > $2 AND (device_id IS NULL OR device_id = $3) ORDER BY pos LIMIT $4`, userId, since, deviceId, limit)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var out []FeedRow
	for rows.Next() {
		var r FeedRow
		var roomId, deviceId *uuid.UUID
		var payload []byte
		if err := rows.Scan(&r.Pos, &r.Ref.Kind, &r.Ref.Id, &roomId, &deviceId, &payload, &r.Ref.Ts); err != nil {
			return nil, fault.Wrap(err)
		}
		if roomId != nil {
			r.Ref.RoomId = *roomId
		}
		if deviceId != nil {
			r.Ref.DeviceId = *deviceId
		}
		r.Ref.Payload = payload
		r.Ref.Ts = r.Ref.Ts.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fault.Wrap(err)
	}
	return out, nil
}

// MinPos は残っている最小の pos（保持の外の判定。410）。行が無ければ feed_cursor の次の pos、それも無ければ 1。
func (f *Feed) MinPos(ctx context.Context, userId uuid.UUID) (int64, error) {
	var pos *int64
	if err := f.pool.QueryRow(ctx, `SELECT min(pos) FROM feed WHERE user_id = $1`, userId).Scan(&pos); err != nil {
		return 0, unavailable(err)
	}
	if pos != nil {
		return *pos, nil
	}
	var next int64
	err := f.pool.QueryRow(ctx, `SELECT next_pos FROM feed_cursor WHERE user_id = $1`, userId).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, unavailable(err)
	}
	return next, nil
}

// LastPos は利用者の最新の pos（WebSocket の ready。第2章 6.3）。まだ 1 行も無ければ 0。
// feed_cursor の次の pos から求める（rollback した採番は戻るので、次の pos − 1 が最後に書いた pos）。
func (f *Feed) LastPos(ctx context.Context, userId uuid.UUID) (int64, error) {
	var next int64
	err := f.pool.QueryRow(ctx, `SELECT next_pos FROM feed_cursor WHERE user_id = $1`, userId).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, unavailable(err)
	}
	return next - 1, nil
}

// Prune は olderThan より古い行を 10,000 行ずつ消す（持ち主の設定の値で srv-worker が呼ぶ）。
func (f *Feed) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	return deleteInBatches(ctx, f.pool, `DELETE FROM feed WHERE ctid IN (SELECT ctid FROM feed WHERE ts < $1 LIMIT 10000)`, olderThan)
}

// deleteInBatches は 1 回 batchRows 行の削除を 0 になるまで繰り返し、消した数の合計を返す。
func deleteInBatches(ctx context.Context, pool *Pool, sql string, args ...any) (int64, error) {
	var total int64
	for {
		tag, err := pool.Exec(ctx, sql, args...)
		if err != nil {
			return total, unavailable(err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < batchRows {
			return total, nil
		}
	}
}

// feedChannel は NOTIFY の channel の名。
const feedChannel = "ft_feed"

// 再接続の待ちと channel の大きさ。
const (
	listenBackoffMin = time.Second
	listenBackoffMax = 30 * time.Second
	listenBuffer     = 4096
)

// Notifier は feed の書き込みを api の hub に知らせる（B-023）。
type Notifier struct {
	pool *Pool
	clk  clock.Clock
}

// NewNotifier は Notifier を作る（clk は再接続の待ち）。
func NewNotifier(pool *Pool, clk clock.Clock) *Notifier {
	return &Notifier{pool: pool, clk: clk}
}

// Notify はトランザクションの中で pg_notify('ft_feed', user_id) を呼ぶ。届くのはコミットした時だけ（PostgreSQL の NOTIFY）。
// Feed.Flush が宛先ごとに 1 回呼ぶ（書き込み担当は Feed.Append を経る）。
func (n *Notifier) Notify(ctx context.Context, tx *Tx, userId uuid.UUID) error {
	return notify(ctx, tx, userId)
}

// notify は Notify の本体（Flush からも呼ぶ）。
func notify(ctx context.Context, tx *Tx, userId uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, feedChannel, userId.String()); err != nil {
		return unavailable(err)
	}
	return nil
}

// nextBackoff は再接続の待ちを倍にする（上限 30 秒）。
func nextBackoff(d time.Duration) time.Duration {
	if d < listenBackoffMin {
		return listenBackoffMin
	}
	return min(2*d, listenBackoffMax)
}

// Listen は LISTEN ft_feed を専用の接続で持ち、知らせの利用者の id を流す。切れたら待って再接続し、
// つながったら uuid.Nil（全員を追いつかせる合図）を流す。channel が満杯の時は落とし、空いた時に uuid.Nil を流す。
// ctx が終われば channel を閉じる。
func (n *Notifier) Listen(ctx context.Context) (<-chan uuid.UUID, error) {
	conn, err := n.listenConn(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan uuid.UUID, listenBuffer)
	go n.loop(ctx, conn, out)
	return out, nil
}

// listenConn は専用の接続を作って LISTEN する。
func (n *Notifier) listenConn(ctx context.Context) (*pgx.Conn, error) {
	conn, err := n.pool.connect(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+feedChannel); err != nil {
		_ = conn.Close(ctx)
		return nil, unavailable(err)
	}
	return conn, nil
}

// loop は知らせを流し、切れたら再接続する。
func (n *Notifier) loop(ctx context.Context, conn *pgx.Conn, out chan<- uuid.UUID) {
	defer close(out)
	overflow := false
	send := func(id uuid.UUID) {
		if overflow {
			select {
			case out <- uuid.Nil:
				overflow = false
			default:
				return
			}
		}
		select {
		case out <- id:
		default:
			overflow = true
		}
	}
	backoff := time.Duration(0)
	for {
		for conn == nil {
			backoff = nextBackoff(backoff)
			select {
			case <-ctx.Done():
				return
			case <-n.clk.After(backoff):
			}
			c, err := n.listenConn(ctx)
			if err != nil {
				continue
			}
			conn = c
			backoff = 0
			send(uuid.Nil)
		}
		nt, err := conn.WaitForNotification(ctx)
		if err != nil {
			_ = conn.Close(context.Background())
			conn = nil
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if id, err := uuid.FromString(nt.Payload); err == nil {
			send(id)
		}
	}
}
