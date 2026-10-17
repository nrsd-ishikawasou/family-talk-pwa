// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package rooms

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store"
)

const (
	relReply      = gen.FtEventMls1RelatesToRelTypeReply
	relReplace    = gen.FtEventMls1RelatesToRelTypeReplace
	relAnnotation = gen.FtEventMls1RelatesToRelTypeAnnotation
)

// R: R-4-2-1
// 送信（第4章 5.1、第12章 5.2、第2章 5.5）：記録し、active の構成員の feed に room.event、応答の pos は本人の feed の pos、Notifier の
// 受け手は送信者を除く active の構成員。構成員でなければ ROOM-002、epoch が古ければ ROOM-006、256 KiB を超えれば ROOM-005。添付は
// MediaBinder.Bind（MEDIA-002、MEDIA-005）。transaction_id は送った端末にだけ返す。silent は記録される。
func TestPostRecordsAndFansOut(t *testing.T) {
	e, a, b, c := family3(t)
	d := e.member("三郎", boundary.RoleMember)
	e.joinFamily(d)
	room := e.group(a, b, c)
	var p Posted
	tid := uuid.Must(uuid.NewV7())
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		p, err = e.b.Timeline.Post(e.ctx, tx, b.p(), room, e.body(room, msgOpt{}), true, tid)
		return err
	}))
	rows := e.feedOf(b.id, kindRoomEvent)
	if rows[len(rows)-1].ref != p.EventId || rows[len(rows)-1].pos != p.Pos || p.TransactionId == nil || *p.TransactionId != tid {
		t.Fatalf("応答の pos と transaction_id: %+v %+v", p, rows[len(rows)-1])
	}
	for _, m := range []*member{a, c} {
		if !slices.Contains(e.roomEvents(m.id, room), p.EventId) {
			t.Fatal("構成員の feed に届かない")
		}
	}
	if slices.Contains(e.roomEvents(d.id, room), p.EventId) {
		t.Fatal("構成員でない人に届いた")
	}
	n, ok := e.notifier.of(p.EventId)
	if !ok || !slices.Equal(n.to, sortedIds([]uuid.UUID{a.id, c.id})) || !n.event.Silent {
		t.Fatalf("Notifier の受け手（送信者を除く）と silent: %+v", n)
	}
	got, err := e.b.Timeline.ForFeed(e.ctx, b.id, b.dev, []uuid.UUID{p.EventId})
	e.must(err)
	var sk map[string]any
	_ = json.Unmarshal(got[p.EventId], &sk)
	if sk["transaction_id"] != tid.String() || sk["silent"] != true {
		t.Fatalf("送った端末への骨: %v", sk)
	}
	got, _ = e.b.Timeline.ForFeed(e.ctx, a.id, a.dev, []uuid.UUID{p.EventId})
	sk = nil
	_ = json.Unmarshal(got[p.EventId], &sk)
	if _, ok := sk["transaction_id"]; ok || sk["id"] != p.EventId.String() {
		t.Fatal("他の端末に transaction_id を返した")
	}
	if got, _ := e.b.Timeline.ForFeed(e.ctx, d.id, d.dev, []uuid.UUID{p.EventId}); len(got) != 0 {
		t.Fatal("構成員でない人の ForFeed が返した")
	}
	_, err = e.post(room, d, msgOpt{})
	e.wantCode(err, cgen.ROOM002)
	_, err = e.post(room, a, msgOpt{epochOff: -1})
	e.wantCode(err, cgen.ROOM006)
	_, err = e.post(room, a, msgOpt{size: eventBodyMax})
	e.wantCode(err, cgen.ROOM005)
	m1, m2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	e.media.fail[m1], e.media.fail[m2] = cgen.MEDIA002, cgen.MEDIA005
	_, err = e.post(room, a, msgOpt{media: []uuid.UUID{m1}})
	e.wantCode(err, cgen.MEDIA002)
	_, err = e.post(room, a, msgOpt{media: []uuid.UUID{m2}})
	e.wantCode(err, cgen.MEDIA005)
	ok3 := uuid.Must(uuid.NewV7())
	ev := e.send(room, a, msgOpt{media: []uuid.UUID{ok3}})
	if !slices.Equal(e.media.boundEvent(ev), []uuid.UUID{ok3}) {
		t.Fatal("添付が結び付かない")
	}
	if e.count(`SELECT count(*) FROM events WHERE room_id = $1 AND schema = 'ft.event.mls/1'`, room) != 2 {
		t.Fatal("誤りの送信が記録された")
	}
}

// R: R-4-3-1
// 関係の規則（第4章 3.2、全体実装設計書 4.2）：返信と annotation は対象が無くても受け付ける。対象が別のトークなら ROOM-012。編集は対象が
// 同じトークに無ければ ROOM-012、他人の物・15 分の後・編集の編集は ROOM-009。関係のコピーは /relations で rel_type で絞って読める。
func TestRelationRules(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	other := e.group(a, b, c)
	ghost := uuid.Must(uuid.NewV7())
	e.send(room, a, msgOpt{rel: relReply, target: ghost})
	e.send(room, a, msgOpt{rel: relAnnotation, target: ghost})
	far := e.send(other, a, msgOpt{})
	for _, rel := range []gen.FtEventMls1RelatesToRelType{relReply, relReplace, relAnnotation} {
		_, err := e.post(room, a, msgOpt{rel: rel, target: far})
		e.wantCode(err, cgen.ROOM012)
	}
	_, err := e.post(room, a, msgOpt{rel: relReplace, target: ghost})
	e.wantCode(err, cgen.ROOM012)
	mine := e.send(room, a, msgOpt{})
	_, err = e.post(room, b, msgOpt{rel: relReplace, target: mine})
	e.wantCode(err, cgen.ROOM009)
	edit := e.send(room, a, msgOpt{rel: relReplace, target: mine})
	_, err = e.post(room, a, msgOpt{rel: relReplace, target: edit})
	e.wantCode(err, cgen.ROOM009)
	react := e.send(room, b, msgOpt{rel: relAnnotation, target: mine})
	reply := e.send(room, c, msgOpt{rel: relReply, target: mine})
	e.clk.Advance(16 * time.Minute)
	_, err = e.post(room, a, msgOpt{rel: relReplace, target: mine})
	e.wantCode(err, cgen.ROOM009)
	all, more, err := e.b.Timeline.Relations(e.ctx, a.p(), room, mine, nil, Cursor{Limit: 10})
	e.must(err)
	ids := []string{}
	for _, s := range all {
		ids = append(ids, s.Id)
	}
	if more || !slices.Equal(ids, []string{edit.String(), react.String(), reply.String()}) {
		t.Fatalf("関係の一覧: %v", ids)
	}
	rt := "annotation"
	only, _, err := e.b.Timeline.Relations(e.ctx, a.p(), room, mine, &rt, Cursor{Limit: 10})
	e.must(err)
	if len(only) != 1 || only[0].Id != react.String() {
		t.Fatalf("rel_type の絞り: %v", only)
	}
	_, _, err = e.b.Timeline.Relations(e.ctx, a.p(), room, far, nil, Cursor{Limit: 10})
	e.wantCode(err, cgen.ROOM007)
}

// R: R-4-2-1
// 読み（第2章 3、5.5）：Get は無いか別のトークなら ROOM-007、削除済みの印は骨（body なし、redacted）。Page は第2章 3 の cursor と
// types の絞り（結果は古い順）。Export も同じ読み。
func TestReadEvents(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	var ids []uuid.UUID
	for range 5 {
		ids = append(ids, e.send(room, a, msgOpt{}))
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Timeline.Redact(e.ctx, tx, a.p(), room, ids[1], nil) }))
	sk, err := e.b.Timeline.Get(e.ctx, b.p(), room, ids[1])
	e.must(err)
	if sk.Body != nil || sk.Redacted == nil || sk.Redacted.By != a.id.String() {
		t.Fatalf("削除済みの印の骨: %+v", sk)
	}
	_, err = e.b.Timeline.Get(e.ctx, b.p(), e.family(), ids[0])
	e.wantCode(err, cgen.ROOM007)
	mls := []string{string(SchemaMls)}
	pg, more, err := e.b.Timeline.Page(e.ctx, b.p(), room, Cursor{Limit: 3}, mls)
	e.must(err)
	if !more || len(pg) != 3 || pg[0].Id != ids[2].String() || pg[2].Id != ids[4].String() {
		t.Fatalf("新しい側の頁（古い順）: %v", pg)
	}
	before := ids[2]
	pg, more, err = e.b.Timeline.Page(e.ctx, b.p(), room, Cursor{Before: &before, Limit: 3}, mls)
	e.must(err)
	if more || len(pg) != 2 || pg[0].Id != ids[0].String() {
		t.Fatalf("before の頁: %v", pg)
	}
	all, _, err := e.b.Timeline.Export(e.ctx, b.p(), room, Cursor{Limit: 100})
	e.must(err)
	kinds := map[string]int{}
	for _, s := range all {
		kinds[string(s.Schema)]++
	}
	if kinds[string(SchemaMls)] != 5 || kinds[string(SchemaSystem)] < 1 || kinds[string(SchemaRedaction)] != 1 {
		t.Fatalf("書き出しの種類: %v", kinds)
	}
}

// R: R-4-4-1
// 既読（第4章 5.4、第2章 11）：3 種を単調に進め、後退と同じ値は無視して feed を書かない。公開の既読は active の構成員へ、私的な既読と
// 読み終えた位置は本人へ。このトークに無いイベントは ROOM-007。一覧は active の構成員だけで、私的な値は本人の分だけ。
func TestReceiptsMonotonicAndAudience(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	m1 := e.send(room, a, msgOpt{})
	m2 := e.send(room, a, msgOpt{})
	put := func(m *member, read, private, fully *uuid.UUID) error {
		return e.tx(func(tx *store.Tx) error { return e.b.Receipts.Put(e.ctx, tx, m.id, room, read, private, fully) })
	}
	receiptsOf := func(m *member) int {
		n := 0
		for _, r := range e.feedOf(m.id, kindRoomReceipt) {
			if r.payload["user_id"] == b.id.String() {
				n++
			}
		}
		return n
	}
	e.must(put(b, &m2, nil, nil))
	if receiptsOf(a) != 1 || receiptsOf(c) != 1 || receiptsOf(b) != 1 {
		t.Fatal("公開の既読が構成員へ届かない")
	}
	e.must(put(b, &m1, nil, nil)) // 後退は無視
	e.must(put(b, &m2, nil, nil)) // 同じ値も無視
	if receiptsOf(a) != 1 {
		t.Fatal("後退か同じ値で feed を書いた")
	}
	e.must(put(b, nil, &m1, &m1))
	if receiptsOf(a) != 1 || receiptsOf(b) != 2 {
		t.Fatal("私的な既読が本人にだけ届かない")
	}
	ghost := uuid.Must(uuid.NewV7())
	e.wantCode(put(b, &ghost, nil, nil), cgen.ROOM007)
	far := e.send(e.family(), a, msgOpt{})
	e.wantCode(put(b, &far, nil, nil), cgen.ROOM007)
	list, err := e.b.Receipts.Of(e.ctx, a.id, room)
	e.must(err)
	var rb Receipt
	for _, r := range list {
		if r.UserId == b.id {
			rb = r
		}
	}
	if rb.Read == nil || *rb.Read != m2 || rb.ReadPrivate != nil || rb.FullyRead != nil {
		t.Fatalf("他の人から見た既読: %+v", rb)
	}
	mine, _ := e.b.Receipts.Of(e.ctx, b.id, room)
	for _, r := range mine {
		if r.UserId == b.id && (r.FullyRead == nil || *r.FullyRead != m1) {
			t.Fatalf("本人の読み終えた位置: %+v", r)
		}
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, room, b.id, false, b.id) }))
	list, _ = e.b.Receipts.Of(e.ctx, a.id, room)
	for _, r := range list {
		if r.UserId == b.id {
			t.Fatal("外れた人の既読を返した")
		}
	}
}

// waitFrames は入力中の送り先が n 個のフレームを受けるまで待つ（期限の goroutine は試験の時計の Advance の後に走る）。
func (e *env) waitFrames(n int) []frame {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f := e.frames.all(); len(f) >= n {
			return f
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("フレームが %d 個にならない: %v", n, e.frames.all())
	return nil
}

// R: R-4-4-2
// 入力中（第4章 5.4、DD-4-5）：feed に書かず、active の構成員へ {type: room.typing, room_id, payload: {user_ids, timeout}} を送る。
// timeout が 0 なら 10 秒。期限で消えて送り直す。打ち直せば前の期限は消さない。typing: false で消える。
func TestTypingBroadcastAndExpiry(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	feedBefore := len(e.feedOf(a.id, ""))
	e.b.Typing.Set(b.id, room, true, 0)
	f := e.waitFrames(1)[0]
	pl := f.payload["payload"].(roomTyping)
	if f.kind != kindRoomTyping || f.payload["room_id"] != room.String() || !slices.Equal(pl.UserIds, []uuid.UUID{b.id}) || pl.Timeout != 10 ||
		!slices.Equal(f.to, sortedIds([]uuid.UUID{a.id, b.id, c.id})) {
		t.Fatalf("入力中のフレーム: %+v", f)
	}
	e.clk.Advance(5 * time.Second)
	e.b.Typing.Set(b.id, room, true, 0) // 打ち直し（前の期限は外さない）
	e.waitFrames(2)
	e.clk.Advance(6 * time.Second) // 最初の期限は過ぎたが打ち直しの期限の前
	time.Sleep(100 * time.Millisecond)
	if fs := e.frames.all(); len(fs) != 2 {
		t.Fatalf("打ち直しの前の期限で消えた: %d", len(fs))
	}
	e.clk.Advance(5 * time.Second)
	fs := e.waitFrames(3)
	if l := fs[2].payload["payload"].(roomTyping).UserIds; len(l) != 0 {
		t.Fatalf("期限で消えない: %v", l)
	}
	e.b.Typing.Set(c.id, room, true, 0)
	e.b.Typing.Set(c.id, room, false, 0)
	fs = e.waitFrames(5)
	if l := fs[4].payload["payload"].(roomTyping).UserIds; len(l) != 0 {
		t.Fatalf("false で消えない: %v", l)
	}
	if len(e.feedOf(a.id, "")) != feedBefore {
		t.Fatal("入力中を feed に書いた")
	}
}

// schedule は予約送信の作成。
func (e *env) schedule(room uuid.UUID, m *member, at time.Time, o msgOpt) (ScheduledView, error) {
	var v ScheduledView
	err := e.tx(func(tx *store.Tx) error {
		var err error
		v, err = e.b.Scheduled.Create(e.ctx, tx, m.p(), room, at, e.body(room, o))
		return err
	})
	return v, err
}

// postScheduled は srv-worker ScheduledSendHandler の 1 回（1 つのトランザクション）。
func (e *env) postScheduled(id string) Posted {
	e.t.Helper()
	e.clk.Advance(time.Millisecond)
	var p Posted
	e.must(e.tx(func(tx *store.Tx) error {
		var err error
		p, err = e.b.Timeline.PostScheduled(e.ctx, tx, uuid.FromStringOrNil(id))
		return err
	}))
	return p
}

// scheduledState は予約の状態。
func (e *env) scheduledState(id string) ScheduledState {
	e.t.Helper()
	var s string
	e.scalar(&s, `SELECT state FROM scheduled_events WHERE id = $1`, uuid.FromStringOrNil(id))
	return ScheduledState(s)
}

// R: R-4-6-1
// 予約送信（第4章 5.8、第12章 5.7）：過去と 30 日の後は ROOM-010。仕事 scheduled_send（重複の鍵）と room.scheduled{pending} を本人へ。
// 関係は予約の時に確かめる：返信は受け付け、別のトークの対象は ROOM-012、編集とリアクションは ROOM-013。添付は予約の時に結び付け
// （MEDIA-002、MEDIA-005）、送る時にイベントへ結び付け直す。取り消しは本人の物だけ（他人の物は ROOM-007）で、仕事と結び付きを外す。
func TestScheduledCreateCancelAndRules(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	other := e.group(a, b, c)
	now := e.clk.Now()
	_, err := e.schedule(room, b, now.Add(-time.Second), msgOpt{})
	e.wantCode(err, cgen.ROOM010)
	_, err = e.schedule(room, b, now.Add(30*24*time.Hour+time.Minute), msgOpt{})
	e.wantCode(err, cgen.ROOM010)
	target := e.send(room, a, msgOpt{})
	far := e.send(other, a, msgOpt{})
	at := e.clk.Now().Add(time.Hour)
	_, err = e.schedule(room, b, at, msgOpt{rel: relReply, target: far})
	e.wantCode(err, cgen.ROOM012)
	for _, rel := range []gen.FtEventMls1RelatesToRelType{relReplace, relAnnotation} {
		_, err = e.schedule(room, a, at, msgOpt{rel: rel, target: target})
		e.wantCode(err, cgen.ROOM013)
	}
	if _, err := e.schedule(room, b, at, msgOpt{rel: relReply, target: uuid.Must(uuid.NewV7())}); err != nil {
		t.Fatalf("対象の無い返信の予約: %v", err)
	}
	m1, m2, ok := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	e.media.fail[m1], e.media.fail[m2] = cgen.MEDIA002, cgen.MEDIA005
	_, err = e.schedule(room, b, at, msgOpt{media: []uuid.UUID{m1}})
	e.wantCode(err, cgen.MEDIA002)
	_, err = e.schedule(room, b, at, msgOpt{media: []uuid.UUID{m2}})
	e.wantCode(err, cgen.MEDIA005)
	v, err := e.schedule(room, b, at, msgOpt{media: []uuid.UUID{ok}})
	e.must(err)
	if v.State != gen.ScheduledStatePending || !v.SendAt.Equal(at) {
		t.Fatalf("予約の応答: %+v", v)
	}
	if !slices.Equal(e.media.boundScheduled(uuid.FromStringOrNil(v.Id)), []uuid.UUID{ok}) {
		t.Fatal("添付が予約に結び付かない")
	}
	if e.count(`SELECT count(*) FROM jobs WHERE dedupe_key = $1 AND state = 'queued'`, "scheduled_send:"+v.Id) != 1 {
		t.Fatal("仕事が積まれない")
	}
	pend := 0
	for _, r := range e.feedOf(b.id, kindRoomScheduled) {
		if r.payload["scheduled_id"] == v.Id && r.payload["state"] == "pending" {
			pend++
		}
	}
	if pend != 1 {
		t.Fatal("room.scheduled{pending} が本人へ届かない")
	}
	cancel := func(m *member, id string) error {
		return e.tx(func(tx *store.Tx) error {
			return e.b.Scheduled.Cancel(e.ctx, tx, m.p(), room, uuid.FromStringOrNil(id))
		})
	}
	e.wantCode(cancel(a, v.Id), cgen.ROOM007)
	e.must(cancel(b, v.Id))
	if e.scheduledState(v.Id) != ScheduledCancelled || e.count(`SELECT count(*) FROM jobs WHERE dedupe_key = $1`, "scheduled_send:"+v.Id) != 0 ||
		len(e.media.boundScheduled(uuid.FromStringOrNil(v.Id))) != 0 {
		t.Fatal("取り消しで仕事か結び付きが残る")
	}
	e.must(cancel(b, v.Id)) // 2 度目は何もしない
	list, more, err := e.b.Scheduled.List(e.ctx, b.p(), room, Cursor{Limit: 10})
	e.must(err)
	if more || len(list) != 2 {
		t.Fatalf("予約の一覧（本人の 2 件）: %d", len(list))
	}
	if l, _, _ := e.b.Scheduled.List(e.ctx, a.p(), room, Cursor{Limit: 10}); len(l) != 0 {
		t.Fatal("他人の予約が一覧に出た")
	}
}

// R: R-4-6-1
// 予約送信を送る（第4章 5.8、8）：サーバーが止まっていた分は時計を進めて送る。ts は送った時刻、scheduled_at は予約の時刻、識別子の順と
// ts の順が一致し、未読に数えられる。添付はイベントへ結び付け直す。同じ仕事の 2 度目は何もしない。送る前に対象が取り消された返信は送る
// （送る時に関係を確かめない）。送信者が active でなければ cancelled と結び付きの外し。
func TestPostScheduled(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	target := e.send(room, a, msgOpt{})
	at := e.clk.Now().Add(time.Hour)
	media := uuid.Must(uuid.NewV7())
	v, err := e.schedule(room, b, at, msgOpt{rel: relReply, target: target, media: []uuid.UUID{media}})
	e.must(err)
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Timeline.Redact(e.ctx, tx, a.p(), room, target, nil) }))
	e.clk.Advance(2 * time.Hour) // サーバーが止まっていた
	early := e.send(room, c, msgOpt{})
	p := e.postScheduled(v.Id)
	if p.EventId == uuid.Nil || e.scheduledState(v.Id) != ScheduledSent {
		t.Fatal("返信の予約が送られない（対象は取り消し済み）")
	}
	var ts, sched time.Time
	e.scalar(&ts, `SELECT ts FROM events WHERE id = $1`, p.EventId)
	e.scalar(&sched, `SELECT scheduled_at FROM events WHERE id = $1`, p.EventId)
	if !ts.Equal(e.clk.Now().UTC().Truncate(time.Microsecond)) || !sched.Equal(at.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("ts は送った時刻、scheduled_at は予約の時刻: %v %v", ts, sched)
	}
	var earlyTs time.Time
	e.scalar(&earlyTs, `SELECT ts FROM events WHERE id = $1`, early)
	if !(early.String() < p.EventId.String()) || !earlyTs.Before(ts) {
		t.Fatal("識別子の順と ts の順が食い違う")
	}
	if !slices.Equal(e.media.boundEvent(p.EventId), []uuid.UUID{media}) || len(e.media.boundScheduled(uuid.FromStringOrNil(v.Id))) != 0 {
		t.Fatal("添付がイベントへ結び付け直されない")
	}
	got, _ := e.b.Timeline.ForFeed(e.ctx, a.id, a.dev, []uuid.UUID{p.EventId})
	var sk map[string]any
	_ = json.Unmarshal(got[p.EventId], &sk)
	if sk["scheduled_at"] == nil {
		t.Fatalf("骨に scheduled_at が無い: %v", sk)
	}
	va, err := e.b.Rooms.View(e.ctx, e.pool, a.id, room)
	e.must(err)
	if va.Unread != 2 {
		t.Fatalf("予約送信が未読に数えられない: %d", va.Unread)
	}
	if again := e.postScheduled(v.Id); again.EventId != uuid.Nil {
		t.Fatal("同じ仕事の 2 度目が送った")
	}
	// 送信者が外れた予約は cancelled。
	v2, err := e.schedule(room, c, e.clk.Now().Add(time.Hour), msgOpt{media: []uuid.UUID{uuid.Must(uuid.NewV7())}})
	e.must(err)
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, room, c.id, false, c.id) }))
	e.clk.Advance(2 * time.Hour)
	if p := e.postScheduled(v2.Id); p.EventId != uuid.Nil || e.scheduledState(v2.Id) != ScheduledCancelled ||
		len(e.media.boundScheduled(uuid.FromStringOrNil(v2.Id))) != 0 {
		t.Fatal("外れた送信者の予約が cancelled で結び付きが外れない")
	}
}

// R: R-4-3-1
// 取消（第4章 3.2、第12章 9）：送信者は 7 日まで（ROOM-009。本人の annotation は期限なし）。送信者でない人はグループの作成者と管理者
// だけで理由が要る（NET-001）、他は ROOM-003。既に削除済みの印なら ROOM-008、mls でなければ ROOM-007。暗号文は直ちに NULL（猶予なし）、
// 媒体の結び付きを外し、同じ送信者の編集も削除済みの印にし、それぞれに取消のイベントを active の構成員へ。Notifier は呼ばない。
func TestRedactRules(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(b, a, c) // 作成者は b、管理者は a
	redact := func(m *member, ev uuid.UUID, reason *string) error {
		return e.tx(func(tx *store.Tx) error { return e.b.Timeline.Redact(e.ctx, tx, m.p(), room, ev, reason) })
	}
	why := "家族の写真"
	media := uuid.Must(uuid.NewV7())
	msg := e.send(room, c, msgOpt{media: []uuid.UUID{media}})
	edit := e.send(room, c, msgOpt{rel: relReplace, target: msg})
	e.wantCode(redact(a, msg, nil), cgen.NET001)
	other := e.send(room, a, msgOpt{})
	e.wantCode(redact(c, other, &why), cgen.ROOM003)
	before := len(e.notifier.calls)
	e.must(redact(c, msg, nil))
	for _, id := range []uuid.UUID{msg, edit} {
		var ct []byte
		var at *time.Time
		e.must(e.pool.QueryRow(e.ctx, `SELECT ciphertext, redacted_at FROM events WHERE id = $1`, id).Scan(&ct, &at))
		if ct != nil || at == nil {
			t.Fatal("暗号文が残るか削除済みの印でない")
		}
		if e.count(`SELECT count(*) FROM events WHERE schema = 'ft.event.redaction/1' AND plain->>'redacts' = $1`, id.String()) != 1 {
			t.Fatal("取消のイベントが 1 件ずつ出ない")
		}
	}
	if len(e.media.boundEvent(msg)) != 0 || len(e.notifier.calls) != before {
		t.Fatal("結び付きが残るか Notifier を呼んだ")
	}
	e.wantCode(redact(c, msg, nil), cgen.ROOM008)
	e.must(redact(b, other, &why)) // 作成者
	var sysId uuid.UUID
	e.scalar(&sysId, `SELECT id FROM events WHERE room_id = $1 AND schema = 'ft.event.system/1' LIMIT 1`, room)
	e.wantCode(redact(a, sysId, &why), cgen.ROOM007)
	old := e.send(room, c, msgOpt{})
	react := e.send(room, c, msgOpt{rel: relAnnotation, target: old})
	e.clk.Advance(8 * 24 * time.Hour)
	e.wantCode(redact(c, old, nil), cgen.ROOM009)
	e.must(redact(c, react, nil)) // 本人の annotation は期限なし
	e.must(redact(a, old, &why))  // 管理者は期限なし
}

// R: R-12-6-1
// 一括の取消（第12章 9、全体実装設計書 I-16）：削除で消す本人の 3,000 件（3 つのトークと、既に退出したトーク）と、削除済みのトークの
// イベント。hook の直後に全部の暗号文が NULL、媒体の参照は 1,000 件ずつ外れ、feed に取消のイベントは無く、送り残しが 3,000 行で仕事が
// queued。仕事の 1 回目で 1,000 件の取消のイベントが出て送り残しが 2,000 行。全部の後に 0 で、取消のイベントは 1 イベントに 1 件。
// 削除済みのトークの分は出ない。止めて再開しても二重に出ない。
func TestBulkRedactionFanout(t *testing.T) {
	e, a, b, c := family3(t)
	r1, r2, r3, left := e.group(a, b, c), e.group(a, b, c), e.group(a, b, c), e.group(a, b, c)
	gone := e.group(a, b, c)
	e.send(gone, b, msgOpt{})
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Rooms.Delete(e.ctx, tx, gone, RuleRoomDelete, a.id, true) }))
	e.must(e.tx(func(tx *store.Tx) error { return e.b.Members.Remove(e.ctx, tx, left, b.id, false, b.id) }))
	e.must(e.commit(left, nil, []uuid.UUID{b.dev}))
	for room, n := range map[uuid.UUID]int{r1: 1200, r2: 1000, r3: 700, left: 100} {
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO events (id, room_id, ts, schema, sender_id, sender_device_id, epoch, ciphertext, plain, silent)
SELECT uuidv7(), $1, now(), 'ft.event.mls/1', $2, $3, 0, '\x01', '{"schema_inner":"ft.content.text/1"}', false FROM generate_series(1, $4)`,
			room, b.id, b.dev, n); err != nil {
			t.Fatal(err)
		}
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, b.id, a.id, true) }))
	if n := e.count(`SELECT count(*) FROM events WHERE sender_id = $1 AND schema = 'ft.event.mls/1' AND ciphertext IS NOT NULL`, b.id); n != 0 {
		t.Fatalf("暗号文が残る: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM redaction_backlog WHERE user_id = $1`, b.id); n != 3000 {
		t.Fatalf("送り残し: %d", n)
	}
	if e.media.logged("unbind_events:") < 4 || e.count(`SELECT count(*) FROM jobs WHERE kind = 'redaction_fanout' AND state = 'queued' AND dedupe_key = $1`,
		"redaction_fanout:"+b.id.String()) != 1 {
		t.Fatal("参照の外しか仕事")
	}
	redactions := func(user uuid.UUID) int {
		n := 0
		for _, r := range e.feedOf(user, kindRoomEvent) {
			if e.count(`SELECT count(*) FROM events WHERE id = $1 AND schema = 'ft.event.redaction/1'`, r.ref) == 1 {
				n++
			}
		}
		return n
	}
	if redactions(a.id) != 0 || redactions(c.id) != 0 {
		t.Fatal("hook が取消のイベントを feed に書いた")
	}
	n, err := e.b.Timeline.FanoutRedactions(e.ctx, b.id, 1000)
	e.must(err)
	if n != 1000 || e.count(`SELECT count(*) FROM redaction_backlog WHERE user_id = $1`, b.id) != 2000 || redactions(a.id) != 1000 {
		t.Fatalf("1 回目: %d", n)
	}
	for {
		n, err := e.b.Timeline.FanoutRedactions(e.ctx, b.id, 1000)
		e.must(err)
		if n == 0 {
			break
		}
	}
	if e.count(`SELECT count(*) FROM redaction_backlog`) != 0 || redactions(a.id) != 3000 || redactions(c.id) != 3000 {
		t.Fatalf("全部の後: %d", redactions(a.id))
	}
	if e.count(`SELECT count(*) FROM events WHERE room_id = $1 AND schema = 'ft.event.redaction/1'`, gone) != 0 {
		t.Fatal("削除済みのトークに取消のイベントを出した")
	}
	if n, _ := e.b.Timeline.FanoutRedactions(e.ctx, b.id, 1000); n != 0 || redactions(a.id) != 3000 {
		t.Fatal("再開で二重に出た")
	}
}

// R: R-4-2-1
// 初回の同期（第2章 5.9）：1 つの見え方の中でトーク（List と同じ形）、家族の構成員、本人の端末、最新の pos。家族全員のトークの知らせは
// FamilyRoom.Flow で active の構成員へ。
func TestSnapshotAndFamilyFlow(t *testing.T) {
	e, a, b, c := family3(t)
	e.group(a, b, c)
	e.must(e.tx(func(tx *store.Tx) error {
		return e.b.FamilyRoom.Flow(e.ctx, tx, "birthday", map[string]any{"user_id": b.id.String()})
	}))
	if k := e.systemKinds(e.family()); k[len(k)-1] != "birthday" {
		t.Fatalf("Flow: %v", k)
	}
	s, err := e.b.Timeline.Snapshot(e.ctx, b.p())
	e.must(err)
	last, err := e.b.d.Feed.LastPos(e.ctx, e.pool, b.id)
	e.must(err)
	if len(s.Snapshot.Rooms) != 3 || len(s.Snapshot.Users) != 3 || len(s.Snapshot.Devices) != 1 || s.Pos != last || last == 0 {
		t.Fatalf("初回の同期: %d %d %d %d %d", len(s.Snapshot.Rooms), len(s.Snapshot.Users), len(s.Snapshot.Devices), s.Pos, last)
	}
}

// R: R-12-6-1
// 送り残しの読みと錠の間に行が消えた時（他の処理が消した）：FanoutRedactions はその行の取消のイベントを出さず、戻りは錠なしで読んだ
// 行の数（0 にしない：仕事が limit の外の送り残しを残して終わらない）。次の回は 0。
func TestFanoutRedactionsCountsReadRows(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	for range 3 {
		e.send(room, b, msgOpt{})
	}
	e.must(e.tx(func(tx *store.Tx) error { return e.id.Users.Delete(e.ctx, tx, b.id, a.id, true) }))
	hold, err := e.pool.Begin(e.ctx)
	e.must(err)
	t.Cleanup(func() { hold.Rollback(e.ctx) })
	e.must(store.RoomLock{}.Lock(e.ctx, hold, room))
	var n int
	ch := within(func() error {
		var err error
		n, err = e.b.Timeline.FanoutRedactions(e.ctx, b.id, 1000)
		return err
	})
	e.waiting(ch, 300*time.Millisecond)
	_, err = hold.Exec(e.ctx, `DELETE FROM redaction_backlog WHERE room_id = $1`, room)
	e.must(err)
	e.must(hold.Commit(e.ctx))
	e.must(<-ch)
	if n != 3 || e.count(`SELECT count(*) FROM events WHERE room_id = $1 AND schema = 'ft.event.redaction/1'`, room) != 0 {
		t.Fatalf("読んだ行の数を返さないか、消えた行の取消のイベントを出した: %d", n)
	}
	if n, _ := e.b.Timeline.FanoutRedactions(e.ctx, b.id, 1000); n != 0 {
		t.Fatalf("次の回: %d", n)
	}
}

// R: R-4-6-1
// 予約送信の取り消しの錠の順（ページの錠の順。rooms が先）：試験がトークの錠を持つ間、Cancel はトークの錠で待ち、予約の行の錠をまだ
// 取っていない（試験が NOWAIT で取れる）。トークの削除（RoomLock の後に媒体の参照と予約の行）と逆向きに待ち合わない。
func TestScheduledCancelLocksRoomFirst(t *testing.T) {
	e, a, b, c := family3(t)
	room := e.group(a, b, c)
	v, err := e.schedule(room, b, e.clk.Now().Add(time.Hour), msgOpt{})
	e.must(err)
	hold, err := e.pool.Begin(e.ctx)
	e.must(err)
	t.Cleanup(func() { hold.Rollback(e.ctx) })
	e.must(store.RoomLock{}.Lock(e.ctx, hold, room))
	ch := within(func() error {
		return e.tx(func(tx *store.Tx) error {
			return e.b.Scheduled.Cancel(e.ctx, tx, b.p(), room, uuid.FromStringOrNil(v.Id))
		})
	})
	e.waiting(ch, 300*time.Millisecond)
	if _, err := hold.Exec(e.ctx, `SELECT 1 FROM scheduled_events WHERE id = $1 FOR UPDATE NOWAIT`, uuid.FromStringOrNil(v.Id)); err != nil {
		t.Fatalf("トークの錠の前に予約の行の錠を取っている: %v", err)
	}
	hold.Rollback(e.ctx)
	e.must(<-ch)
	if e.scheduledState(v.Id) != ScheduledCancelled {
		t.Fatal("取り消されない")
	}
}
