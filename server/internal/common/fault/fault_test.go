// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package fault

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// R: R-1-7-2
func TestNewFollowsTheErrorTableForEveryCode(t *testing.T) {
	for _, code := range Codes() {
		def, _ := Lookup(code)
		if def.Status == 0 || def.Retry == gen.RetryNotApplicable {
			assertPanics(t, code, func() { New(code, nil) })
			continue
		}
		e := New(code, map[string]any{"k": 1})
		if e.Code != code || e.Status != def.Status {
			t.Fatalf("%s：番号か状態が表と違う", code)
		}
		if e.Retryable != (def.Retry == gen.RetryYes) {
			t.Fatalf("%s：再試行の可否が表と違う", code)
		}
	}
}

func TestNewPanicsOnUnknownCode(t *testing.T) {
	assertPanics(t, "XXX-999", func() { New("XXX-999", nil) })
}

func assertPanics(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s で panic しない", name)
		}
	}()
	f()
}

func TestProblemShape(t *testing.T) {
	e := New(gen.ROOM003, map[string]any{"room_id": "x"})
	e.Instance = uuid.Must(uuid.NewV7())
	p := e.Problem("https://family.example")
	if p.Type != "https://family.example/errors/ROOM-003" || p.Code != gen.ROOM003 || p.Status != 403 {
		t.Fatalf("type、code、status が違う: %+v", p)
	}
	if p.Instance != "urn:uuid:"+e.Instance.String() {
		t.Fatalf("instance が要求の識別子の URN でない: %q", p.Instance)
	}
	if p.Title != gen.Errors[gen.ROOM003].Title || p.Detail != "" {
		t.Fatalf("title か detail が違う: %+v", p)
	}
	b, _ := json.Marshal(p)
	for _, k := range []string{`"type"`, `"title"`, `"status"`, `"code"`, `"retryable"`, `"params"`, `"instance"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("problem の JSON に %s が無い: %s", k, b)
		}
	}
	if strings.Contains(string(b), `"detail"`) {
		t.Fatalf("空の detail が出ている: %s", b)
	}
	plain := New(gen.SRV003, nil).Problem("https://x")
	if plain.Instance != "" || plain.Params != nil {
		t.Fatalf("instance と params が無い時に出ている: %+v", plain)
	}
}

func TestFaultHidesCauseAndKeepsStack(t *testing.T) {
	cause := errors.New("秘密の値 s3cr3t を含む文")
	f := Wrap(cause)
	if strings.Contains(f.Error(), "s3cr3t") {
		t.Fatalf("Error に cause の文が出る: %q", f.Error())
	}
	if !errors.Is(f, cause) {
		t.Fatal("Unwrap で cause に届かない")
	}
	if !strings.Contains(f.Stack, "TestFaultHidesCauseAndKeepsStack") {
		t.Fatalf("stack に呼んだ関数が無い: %s", f.Stack)
	}
	f.Instance = uuid.Must(uuid.NewV7())
	ev := f.AsEvent()
	if ev.Code != gen.SRV003 || ev.Status != 500 || ev.Instance != f.Instance {
		t.Fatalf("AsEvent が SRV-003 と同じ instance でない: %+v", ev)
	}
}

func TestEventErrorDoesNotLeakParams(t *testing.T) {
	e := New(gen.NET001, map[string]any{"field": "s3cr3t"})
	if e.Error() != "fault: NET-001" {
		t.Fatalf("Error が番号だけでない: %q", e.Error())
	}
}
