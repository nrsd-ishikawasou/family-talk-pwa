// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package logx

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
)

var fixedNow = time.Date(2026, 10, 14, 1, 2, 3, 456000000, time.UTC)

func newTest(level Level) (*Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return New(&buf, "api", level, func() time.Time { return fixedNow }), &buf
}

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("1 行が JSON でない: %q", l)
		}
		out = append(out, m)
	}
	return out
}

type counter struct {
	mu   sync.Mutex
	seen []string
}

func (c *counter) Add(component, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, component+"/"+code)
}

// R: R-11-9-1
func TestRecordShapeAndRedaction(t *testing.T) {
	l, buf := newTest(LevelInfo)
	id := uuid.Must(uuid.NewV7())
	l.With(Field{"req_id", Id(id)}).Info("要求を受けた",
		Field{"route", Path("/rooms/{id}/events")},
		Field{"dur_ms", Duration(1500 * time.Millisecond)},
		Field{"token", Secret([]byte("s3cr3t"))},
		Field{"ip", Ip(netip.MustParseAddr("203.0.113.9"))},
		Field{"kind", Label("backup")},
		Field{"n", Count(3)},
	)
	got := lines(t, buf)
	if len(got) != 1 {
		t.Fatalf("1 件 1 行でない: %d", len(got))
	}
	m := got[0]
	want := map[string]any{
		"ts": "2026-10-14T01:02:03.456Z", "level": "info", "component": "api", "msg": "要求を受けた",
		"req_id": id.String(), "route": "/rooms/{id}/events", "dur_ms": float64(1500),
		"token": "<redacted>", "ip": "<ip>", "kind": "backup", "n": float64(3),
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("%s が %v でなく %v", k, v, m[k])
		}
	}
	if strings.Contains(buf.String(), "s3cr3t") || strings.Contains(buf.String(), "203.0.113.9") {
		t.Fatalf("秘密か生の IP が出た: %s", buf.String())
	}
}

func TestLevelFilterAndSetLevel(t *testing.T) {
	l, buf := newTest(LevelInfo)
	child := l.With(Field{"user_id", Id(uuid.Nil)})
	child.Debug("見えない")
	if buf.Len() != 0 {
		t.Fatalf("info で debug が出た: %s", buf.String())
	}
	l.SetLevel(LevelDebug)
	child.Debug("見える")
	if got := lines(t, buf); len(got) != 1 || got[0]["level"] != "debug" {
		t.Fatalf("SetLevel が子に効かない: %s", buf.String())
	}
}

func TestErrorCounterOnWarnAndErrorWithCode(t *testing.T) {
	l, _ := newTest(LevelInfo)
	c := &counter{}
	l.SetCounter(c)
	l.Info("数えない", Field{"code", Code("AUTH-001")})
	l.Warn("数える", Field{"code", Code("AUTH-001")})
	l.With(Field{"code", Code("SRV-003")}).Error("With の code も数える")
	l.Error("code が無いので数えない")
	if strings.Join(c.seen, ",") != "api/AUTH-001,api/SRV-003" {
		t.Fatalf("数えた物が違う: %v", c.seen)
	}
}

func TestNewlineInValuesStaysOnOneLine(t *testing.T) {
	l, buf := newTest(LevelInfo)
	l.Info("改行\nを含む msg", Field{"route", Path("/a\n/b")})
	if strings.Count(strings.TrimSpace(buf.String()), "\n") != 0 {
		t.Fatalf("1 件が複数の行になった: %q", buf.String())
	}
}

func TestTraceWritesCodeLocation(t *testing.T) {
	l, buf := newTest(LevelInfo)
	l.Error("内部の誤り", Field{"stack", Trace("main.f /src/a.go:12\n")})
	got := lines(t, buf)
	if len(got) != 1 || got[0]["stack"] != "main.f /src/a.go:12\n" {
		t.Fatalf("Trace の値が違う: %v", got)
	}
}
