// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/boundary/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	cgen "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

func uid(n byte) uuid.UUID {
	var u uuid.UUID
	u[0], u[15] = 0x01, n
	return u
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *fault.Event
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("%s を期待したが %v", code, err)
	}
}

// R: R-4-2-1
// 一覧の位置（第2章 3）：after は古い側から進み、before と無しは新しい側から読んで古い順に並べ直す。limit + 1 件で has_more。
func TestCursorWhereAndFinish(t *testing.T) {
	a, b := uid(1), uid(2)
	sql, args := whereUUID(Cursor{After: &a, Limit: 2}, `x = $1`, `id`, []any{"r"})
	if sql != `x = $1 AND id > $2 ORDER BY id ASC LIMIT $3` || len(args) != 3 || args[2] != 3 {
		t.Fatalf("after: %s %v", sql, args)
	}
	sql, args = whereUUID(Cursor{Before: &b, Limit: 5}, `x = $1`, `id`, []any{"r"})
	if sql != `x = $1 AND id < $2 ORDER BY id DESC LIMIT $3` || args[2] != 6 {
		t.Fatalf("before: %s %v", sql, args)
	}
	sql, _ = whereUUID(Cursor{Limit: 1}, `x = $1`, `id`, []any{"r"})
	if !strings.HasSuffix(sql, `ORDER BY id DESC LIMIT $2`) {
		t.Fatalf("無し: %s", sql)
	}
	items, more := finish([]int{3, 2, 1}, Cursor{Limit: 2})
	if !more || !slices.Equal(items, []int{2, 3}) {
		t.Fatalf("新しい側から: %v %v", items, more)
	}
	items, more = finish([]int{1, 2}, Cursor{After: &a, Limit: 2})
	if more || !slices.Equal(items, []int{1, 2}) {
		t.Fatalf("after: %v %v", items, more)
	}
	if items, _ := finish[int](nil, Cursor{Limit: 2}); items == nil {
		t.Fatal("空の一覧が nil（線の上で null になる）")
	}
	c, err := newCursor(nil, nil, nil)
	if err != nil || c.Limit != pageDefault {
		t.Fatalf("既定の件数: %+v %v", c, err)
	}
	bad := "x"
	_, err = newCursor(nil, &bad, nil)
	wantCode(t, err, cgen.NET001)
}

// mlsBody は試験の ft.event.mls/1（暗号文は任意のバイト）。
func mlsBody(ct []byte, epoch int) gen.FtEventMls1 {
	return gen.FtEventMls1{GroupId: base64.RawURLEncoding.EncodeToString(make([]byte, 20)), Epoch: epoch, SchemaInner: "ft.content.text/1",
		Ciphertext: base64.RawURLEncoding.EncodeToString(ct)}
}

// R: R-4-2-1
// mls の行の分け方：暗号文のバイトと epoch を外し、他の欄を plain に残す。暗号文が base64url でなければ ROOM-005。
// 骨（ft.room_event/1）は plain に epoch と暗号文を戻す。削除済みの印は body を付けず redacted。取り消した人の無い印を返せば Fault。
func TestMlsPartsAndSkeleton(t *testing.T) {
	body := mlsBody([]byte{1, 2, 3}, 7)
	ct, epoch, plain, err := mlsParts(body)
	if err != nil || !slices.Equal(ct, []byte{1, 2, 3}) || epoch != 7 {
		t.Fatalf("分け方: %v %d %v", ct, epoch, err)
	}
	var m map[string]any
	_ = json.Unmarshal(plain, &m)
	if _, ok := m["ciphertext"]; ok || m["epoch"] != nil || m["schema_inner"] != "ft.content.text/1" {
		t.Fatalf("plain に暗号文か epoch が残るか欄が落ちた: %v", m)
	}
	bad := body
	bad.Ciphertext = "!!"
	_, _, _, err = mlsParts(bad)
	wantCode(t, err, cgen.ROOM005)
	sender, dev, room := uid(1), uid(2), uid(3)
	ep := int64(7)
	tid := uid(9)
	e := Event{Id: uid(4), RoomId: room, Ts: time.Date(2026, 10, 17, 1, 2, 3, 0, time.FixedZone("x", 9*3600)), Schema: SchemaMls, Sender: &sender,
		SenderDevice: &dev, Epoch: &ep, Ciphertext: ct, Plain: plain, TransactionId: &tid, Silent: true}
	sk, err := skeleton(e, dev)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sk)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	b, _ := out["body"].(map[string]any)
	if b["ciphertext"] != body.Ciphertext || b["epoch"] != float64(7) || out["silent"] != true || out["transaction_id"] != tid.String() ||
		!strings.HasSuffix(out["ts"].(string), "Z") {
		t.Fatalf("骨: %s", raw)
	}
	if sk2, _ := skeleton(e, uid(8)); sk2.TransactionId != nil {
		t.Fatal("送った端末でないのに transaction_id を返した")
	}
	at := e.Ts
	e.RedactedAt, e.RedactedBy, e.Ciphertext = &at, &sender, nil
	sk, err = skeleton(e, dev)
	if err != nil || sk.Body != nil || sk.Redacted == nil || sk.Redacted.By != sender.String() {
		t.Fatalf("削除済みの印の骨: %+v %v", sk, err)
	}
	e.RedactedBy = nil
	var f *fault.Fault
	if _, err := skeleton(e, dev); !errors.As(err, &f) {
		t.Fatalf("取り消した人の無い印を返した: %v", err)
	}
}

// R: R-4-4-1
// 既読の位置の比べ方：(ts, id) の順。同じ ts は識別子で比べ、cur が無ければ進む。
func TestMarkAfter(t *testing.T) {
	ts := time.Unix(100, 0)
	a, b := mark{id: uid(1), ts: ts}, mark{id: uid(2), ts: ts}
	if !b.after(&a) || a.after(&b) || a.after(&a) || !a.after(nil) {
		t.Fatal("同じ ts の比べ方")
	}
	later := mark{id: uid(0), ts: ts.Add(time.Second)}
	if !later.after(&b) || b.after(&later) {
		t.Fatal("ts の比べ方")
	}
}

// R: R-4-1-1
// 1 対 1 の一意の鍵は 2 人の順によらない。
func TestDmKeyOf(t *testing.T) {
	if dmKeyOf(uid(2), uid(1)) != dmKeyOf(uid(1), uid(2)) || dmKeyOf(uid(1), uid(2)) != uid(1).String()+":"+uid(2).String() {
		t.Fatal("dm_key")
	}
	if !slices.Equal(sortedIds([]uuid.UUID{uid(3), uid(1), uid(3)}), []uuid.UUID{uid(1), uid(3)}) {
		t.Fatal("sortedIds")
	}
}

// R: R-4-2-3
// 本体の大きさ：暗号文を含む JSON が 256 KiB まで（第2章 6.1）。超えれば ROOM-005。
func TestCheckSize(t *testing.T) {
	small := mlsBody(make([]byte, 1000), 0)
	if err := checkSize(small); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(small)
	room := eventBodyMax - len(raw) + len(small.Ciphertext)
	fit := small
	fit.Ciphertext = strings.Repeat("A", room)
	if err := checkSize(fit); err != nil {
		t.Fatalf("ちょうど 256 KiB が通らない: %v", err)
	}
	over := fit
	over.Ciphertext += "A"
	wantCode(t, checkSize(over), cgen.ROOM005)
}

// R: R-4-6-1
// 予約送信に付けられない関係（第4章 5.8）：編集（replace）とリアクション・票・出欠（annotation）は DB を読む前に ROOM-013。
func TestScheduledRelationRejectsReplaceAndAnnotation(t *testing.T) {
	s := &Scheduled{}
	for _, rel := range []gen.FtEventMls1RelatesToRelType{gen.FtEventMls1RelatesToRelTypeReplace, gen.FtEventMls1RelatesToRelTypeAnnotation} {
		b := mlsBody([]byte{1}, 0)
		b.RelatesTo = &struct {
			EventId gen.CommonUuidv71               `json:"event_id"`
			Key     *string                         `json:"key,omitempty"`
			RelType gen.FtEventMls1RelatesToRelType `json:"rel_type"`
		}{EventId: uid(1).String(), RelType: rel}
		wantCode(t, s.checkRelation(context.Background(), nil, uid(2), uid(3), b, time.Unix(0, 0)), cgen.ROOM013)
	}
	if err := s.checkRelation(context.Background(), nil, uid(2), uid(3), mlsBody([]byte{1}, 0), time.Unix(0, 0)); err != nil {
		t.Fatalf("関係の無い予約: %v", err)
	}
}
