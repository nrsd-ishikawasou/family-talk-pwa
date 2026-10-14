// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package ids

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// fixedRand は決まった列の乱数（同じ列を再現する）。
func fixedRand() *bytes.Reader {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return bytes.NewReader(b)
}

// R: R-1-7-2
func TestNewV7IsVersion7AndOrdered(t *testing.T) {
	c := clock.NewTestClock(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	g := New(c, rand.Reader)
	prev := g.NewV7()
	if prev.Version() != uuid.V7 {
		t.Fatalf("版が 7 でない: %d", prev.Version())
	}
	for i := 0; i < 2000; i++ {
		if i%500 == 0 {
			c.Advance(time.Millisecond)
		}
		u := g.NewV7()
		if bytes.Compare(prev.Bytes(), u.Bytes()) >= 0 {
			t.Fatalf("時系列に並ばない: %s の後に %s", prev, u)
		}
		prev = u
	}
}

func TestNewV7KeepsOrderWhenClockGoesBack(t *testing.T) {
	c := clock.NewTestClock(time.Date(2026, 10, 14, 0, 0, 1, 0, time.UTC))
	g := New(c, rand.Reader)
	a := g.NewV7()
	c.Set(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	b := g.NewV7()
	if bytes.Compare(a.Bytes(), b.Bytes()) >= 0 {
		t.Fatalf("時計が戻ると順序が崩れる: %s の後に %s", a, b)
	}
}

func TestSameClockAndRandReproduceSameIds(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	g1 := New(clock.NewTestClock(at), fixedRand())
	g2 := New(clock.NewTestClock(at), fixedRand())
	for i := 0; i < 10; i++ {
		if a, b := g1.NewV7(), g2.NewV7(); a != b {
			t.Fatalf("同じ時計と乱数で違う id: %s と %s", a, b)
		}
	}
	if g1.UserCode() != g2.UserCode() {
		t.Fatal("同じ乱数で違う user_code")
	}
}

func TestParseAcceptsV7AndRejectsOthers(t *testing.T) {
	g := New(clock.WallClock{}, rand.Reader)
	u := g.NewV7()
	got, ev := g.Parse(strings.ToUpper(u.String()))
	if ev != nil || got != u {
		t.Fatalf("大文字の v7 が読めない: %v %v", got, ev)
	}
	v4 := uuid.Must(uuid.NewV4())
	for _, s := range []string{v4.String(), "", "not-a-uuid", u.String() + "0"} {
		if _, ev := g.Parse(s); ev == nil || ev.Code != gen.NET001 || ev.Status != 400 {
			t.Fatalf("%q を NET-001 で拒まない: %v", s, ev)
		}
	}
}

func TestTokenLengthEncodingAndHash(t *testing.T) {
	g := New(clock.WallClock{}, rand.Reader)
	tok := g.NewToken()
	if len(tok) != 32 {
		t.Fatalf("token が 256 ビットでない: %d", len(tok))
	}
	enc := EncodeToken(tok)
	if len(enc) != 43 || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(enc) {
		t.Fatalf("URL-safe の Base64 の 43 文字でない: %q", enc)
	}
	// SHA-256("abc")（FIPS 180-2 の例）
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := hex.EncodeToString(HashToken([]byte("abc"))); got != want {
		t.Fatalf("SHA-256 が違う: %s", got)
	}
}

func TestUserCodeFormat(t *testing.T) {
	g := New(clock.WallClock{}, rand.Reader)
	re := regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}-[A-HJ-NP-Z2-9]{4}$`)
	seen := map[byte]bool{}
	for i := 0; i < 2000; i++ {
		c := g.UserCode()
		if !re.MatchString(c) {
			t.Fatalf("形が違う: %q", c)
		}
		for j := 0; j < len(c); j++ {
			seen[c[j]] = true
		}
	}
	for _, bad := range "IO01" {
		if seen[byte(bad)] {
			t.Fatalf("除く文字 %c が出た", bad)
		}
	}
	if len(seen) != 33 { // 32 文字と区切りの -
		t.Fatalf("32 文字の表の全部が出ない: %d", len(seen))
	}
}
