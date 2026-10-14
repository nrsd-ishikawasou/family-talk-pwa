// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

func isCode(err error, code string) bool {
	var ev *fault.Event
	return errors.As(err, &ev) && ev.Code == code
}

// R: R-1-2-2
func TestFileStoreResumeFinalizeAndSweep(t *testing.T) {
	root := t.TempDir()
	s := NewFileStore(root)
	id := uuid.Must(uuid.NewV7())
	w, err := s.OpenPart(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(strings.NewReader("hello "), 100); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	// 再開：同じスロットを開き直すと位置が続きになる
	w, _ = s.OpenPart(id)
	if w.Offset() != 6 {
		t.Fatalf("再開の位置が違う: %d", w.Offset())
	}
	if _, err := w.Append(strings.NewReader("world"), 100); err != nil {
		t.Fatal(err)
	}
	// 上限を超える追記は戻して MEDIA-001
	if _, err := w.Append(bytes.NewReader(make([]byte, 200)), 100); !isCode(err, gen.MEDIA001) || w.Offset() != 11 {
		t.Fatalf("上限を超えた追記を戻さない: %v %d", err, w.Offset())
	}
	_ = w.Close()
	sum := sha256.Sum256([]byte("hello world"))
	if err := s.Finalize(id, sha256.Sum256([]byte("違う"))); !isCode(err, gen.MEDIA004) {
		t.Fatalf("ハッシュの違いを MEDIA-004 にしない: %v", err)
	}
	if _, err := os.Stat(s.partPath(id)); err == nil {
		t.Fatal("ハッシュが違う .part が残る")
	}
	// 書き直して完了
	w, _ = s.OpenPart(id)
	_, _ = w.Append(strings.NewReader("hello world"), 100)
	_ = w.Close()
	if err := s.Finalize(id, sum); err != nil {
		t.Fatal(err)
	}
	if !s.Exists(sum) || !strings.HasPrefix(s.PathFor(sum), filepath.Join(root, "b9")) {
		t.Fatalf("置き場が先頭 2 文字のディレクトリでない: %s", s.PathFor(sum))
	}
	f, err := s.Open(sum)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := s.Finalize(id, sum); !isCode(err, gen.MEDIA002) {
		t.Fatalf("無いスロットの完了を MEDIA-002 にしない: %v", err)
	}
	// 同じ中身の 2 つ目は .part を消して成功
	id2 := uuid.Must(uuid.NewV7())
	w, _ = s.OpenPart(id2)
	_, _ = w.Append(strings.NewReader("hello world"), 100)
	_ = w.Close()
	if err := s.Finalize(id2, sum); err != nil {
		t.Fatal(err)
	}
	// 期限切れの .part の掃除
	old := uuid.Must(uuid.NewV7())
	w, _ = s.OpenPart(old)
	_ = w.Close()
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = os.Chtimes(s.partPath(old), past, past)
	swept, err := s.SweepParts(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(swept) != 1 {
		t.Fatalf("期限切れの .part を消さない: %v %v", swept, err)
	}
	if u, err := s.Usage(); err != nil || u != 11 {
		t.Fatalf("容量が違う: %d %v", u, err)
	}
	if err := s.Remove(sum); err != nil || s.Exists(sum) {
		t.Fatal("消せない")
	}
	if err := s.Remove(sum); err != nil {
		t.Fatal("無い実体の削除が誤りになった")
	}
	if _, err := s.Open(sum); !isCode(err, gen.MEDIA006) {
		t.Fatalf("無い実体を MEDIA-006 にしない: %v", err)
	}
}

func TestDumpCommandsAndPassFile(t *testing.T) {
	var d Dump
	path, cleanup, err := d.WritePassFile(t.TempDir(), []byte(`p:a\ss`))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	b, _ := os.ReadFile(path)
	if st.Mode().Perm() != 0o600 || string(b) != `postgres:5432:*:family_talk:p\:a\\ss`+"\n" {
		t.Fatalf("パスワードのファイルの形か権限が違う: %v %q", st.Mode().Perm(), b)
	}
	cmds := d.DumpCommands(path)
	if len(cmds) != 2 || cmds[0].Args[0] != "pg_dumpall" || cmds[1].Args[0] != "pg_dump" {
		t.Fatalf("globals、dump の順でない: %+v", cmds)
	}
	all := strings.Join(append(cmds[1].Args, cmds[1].Env...), " ")
	for _, want := range []string{"--format=custom", "--host postgres", "--port 5432", "--username family_talk", "--dbname family_talk", "--no-password", "PGPASSFILE=" + path} {
		if !strings.Contains(all, want) {
			t.Fatalf("%q が無い: %s", want, all)
		}
	}
	if strings.Contains(all, "PGPASSWORD") {
		t.Fatal("PGPASSWORD を使っている")
	}
	rs := d.RestoreCommands(path, "g.sql", "d.dump")
	if rs[0].Args[0] != "psql" || rs[1].Args[0] != "pg_restore" || rs[1].Args[len(rs[1].Args)-1] != "d.dump" {
		t.Fatalf("戻しの命令が違う: %+v", rs)
	}
	cleanup()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("cleanup で消えない")
	}
}

func TestRetentionRejectsBadNames(t *testing.T) {
	r := NewRetention(nil)
	for _, n := range [][2]string{{"Sessions", "created_at"}, {"x;drop", "a"}, {"ok", "a b"}, {"", "a"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%v を受けた", n)
				}
			}()
			_, _ = r.DeleteOlder(context.Background(), n[0], n[1], time.Unix(0, 0))
		}()
	}
}

func TestListenBackoff(t *testing.T) {
	d := time.Duration(0)
	var got []time.Duration
	for i := 0; i < 7; i++ {
		d = nextBackoff(d)
		got = append(got, d)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("待ちが 1、2、4、…、30 秒でない: %v", got)
		}
	}
}

func TestFeedAppendRejectsUnknownKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("知らない種類で panic しない")
		}
	}()
	_ = NewFeed(nil).Append(context.Background(), nil, uuid.Nil, 1, FeedRef{Kind: "room.typing"})
}

func TestFeedCheckRef(t *testing.T) {
	id := uuid.Must(uuid.FromString("01890a5d-ac96-774b-bcce-b302099a8057"))
	room := uuid.Must(uuid.FromString("01890a5d-ac96-774b-bcce-b302099a8058"))
	p := json.RawMessage(`{"count":1}`)
	ok := []FeedRef{
		{Kind: "room.event", Id: id, RoomId: room},
		{Kind: "room.receipt", Id: id, RoomId: room, Payload: p},
		{Kind: "device.inbox", Id: id, Payload: p},
	}
	for _, r := range ok {
		checkRef(r)
	}
	bad := []FeedRef{
		{Kind: "device.inbox", Payload: p},                       // id が無い
		{Kind: "room.receipt", Id: id, Payload: p},               // room.* に room_id が無い
		{Kind: "device.inbox", Id: id, RoomId: room, Payload: p}, // room.* でないのに room_id
		{Kind: "room.event", Id: id, RoomId: room, Payload: p},   // room.event に payload
		{Kind: "notice", Id: id},                                 // payload が無い
	}
	for i, r := range bad {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%d：panic しない", i)
				}
			}()
			checkRef(r)
		}()
	}
}

func TestTxFromCtx(t *testing.T) {
	if TxFrom(context.Background()) != nil {
		t.Fatal("空の ctx でトランザクションが返る")
	}
	tx := &Tx{}
	if TxFrom(WithTx(context.Background(), tx)) != tx {
		t.Fatal("入れたトランザクションが返らない")
	}
}

func TestInTxUsesOuterTx(t *testing.T) {
	outer := &Tx{}
	var got *Tx
	if err := (&Pool{}).InTx(WithTx(context.Background(), outer), func(tx *Tx) error { got = tx; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != outer {
		t.Fatal("外側のトランザクションを使わない")
	}
}
