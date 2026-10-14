// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"testing"
	"time"
)

// R: R-1-7-2
func TestWallClockIsUTCMilliseconds(t *testing.T) {
	now := WallClock{}.Now()
	if now.Location() != time.UTC {
		t.Fatalf("UTC でない: %v", now.Location())
	}
	if now.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("ミリ秒に丸められていない: %v", now)
	}
}

func TestTestClockAdvancesOnlyWhenTold(t *testing.T) {
	start := time.Date(2026, 10, 14, 1, 2, 3, 456789000, time.FixedZone("JST", 9*3600))
	c := NewTestClock(start)
	want := start.UTC().Truncate(time.Millisecond)
	if !c.Now().Equal(want) || c.Now().Location() != time.UTC {
		t.Fatalf("始まりが違う: %v", c.Now())
	}
	c.Advance(1500 * time.Millisecond)
	if got := c.Now(); !got.Equal(want.Add(1500 * time.Millisecond)) {
		t.Fatalf("Advance の後が違う: %v", got)
	}
	later := want.Add(time.Hour)
	c.Set(later)
	if !c.Now().Equal(later) {
		t.Fatalf("Set の後が違う: %v", c.Now())
	}
}

func TestTestClockAfterFiresAtDeadline(t *testing.T) {
	c := NewTestClock(time.Unix(0, 0))
	ch := c.After(10 * time.Second)
	c.Advance(9 * time.Second)
	select {
	case <-ch:
		t.Fatal("期限の前に送られた")
	default:
	}
	c.Advance(time.Second)
	select {
	case got := <-ch:
		if !got.Equal(time.Unix(10, 0).UTC()) {
			t.Fatalf("送られた時刻が違う: %v", got)
		}
	default:
		t.Fatal("期限で送られない")
	}
	if len(c.waiters) != 0 {
		t.Fatalf("送った待ちが残っている: %d", len(c.waiters))
	}
}

func TestTestClockAfterNonPositiveFiresImmediately(t *testing.T) {
	c := NewTestClock(time.Unix(5, 0))
	for _, d := range []time.Duration{0, -time.Second} {
		select {
		case <-c.After(d):
		default:
			t.Fatalf("After(%v) が即座に送らない", d)
		}
	}
}

func TestTestClockSetFiresAllDueWaiters(t *testing.T) {
	c := NewTestClock(time.Unix(0, 0))
	a, b, later := c.After(time.Second), c.After(2*time.Second), c.After(time.Hour)
	c.Set(time.Unix(3, 0))
	for i, ch := range []<-chan time.Time{a, b} {
		select {
		case <-ch:
		default:
			t.Fatalf("%d 番目の待ちが送られない", i)
		}
	}
	select {
	case <-later:
		t.Fatal("期限の来ていない待ちが送られた")
	default:
	}
}
