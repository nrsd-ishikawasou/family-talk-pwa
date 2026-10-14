// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"testing"
	"time"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

var policies = []gen.Policy{
	{Name: "login", OnExceed: "AUTH-004", Limits: []gen.Limit{
		{Key: "ip", Limit: 10, Window: 60, Burst: 0},
		{Key: "device", Limit: 3, Window: 60, Burst: 0},
	}},
	{Name: "send", OnExceed: "NET-004", Limits: []gen.Limit{{Key: "user", Limit: 2, Window: 10, Burst: 4}}},
}

func newLimiter() (*Limiter, *clock.TestClock) {
	c := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	return New(c, policies), c
}

// R: R-1-7-2
func TestBurstThenDenyWithRetryAfter(t *testing.T) {
	l, _ := newLimiter()
	s := Subjects{User: "u1"}
	for i := 0; i < 4; i++ {
		if d := l.Allow("send", s); !d.Allowed || d.Limit != 2 || d.Window != 10 || d.Remaining != 3-i {
			t.Fatalf("%d 回目：%+v", i, d)
		}
	}
	d := l.Allow("send", s)
	if d.Allowed || d.RetryAfter != 5*time.Second {
		t.Fatalf("容量を超えて許した、または Retry-After が違う: %+v", d)
	}
}

func TestRefillOverTime(t *testing.T) {
	l, c := newLimiter()
	s := Subjects{User: "u1"}
	for i := 0; i < 4; i++ {
		l.Allow("send", s)
	}
	c.Advance(5 * time.Second) // 2 個/10 秒 で 1 個
	if d := l.Allow("send", s); !d.Allowed {
		t.Fatalf("補充されない: %+v", d)
	}
	if d := l.Allow("send", s); d.Allowed {
		t.Fatal("補充を超えて許した")
	}
}

func TestAllLimitsOfAPolicyAndNoDecrementOnDeny(t *testing.T) {
	l, _ := newLimiter()
	s := Subjects{Ip: "203.0.113.1", Device: "d1"}
	for i := 0; i < 3; i++ {
		if !l.Allow("login", s).Allowed {
			t.Fatal("端末の上限の前に拒んだ")
		}
	}
	d := l.Allow("login", s)
	if d.Allowed {
		t.Fatal("端末の上限を超えて許した")
	}
	// 拒んだ時に IP の bucket を減らしていないこと：別の端末なら IP の残りは 10-3=7
	d2 := l.Allow("login", Subjects{Ip: "203.0.113.1", Device: "d2"})
	if !d2.Allowed {
		t.Fatal("別の端末を拒んだ")
	}
	if got := l.buckets[bucketKey{"login", "ip", "203.0.113.1"}].tokens; got != 6 {
		t.Fatalf("拒んだ時に IP の bucket が減った: %v", got)
	}
}

func TestEmptySubjectsAreAllowedAndUnknownPolicyPanics(t *testing.T) {
	l, _ := newLimiter()
	if !l.Allow("send", Subjects{Ip: "1.2.3.4"}).Allowed {
		t.Fatal("数える相手が無いのに拒んだ")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("表に無い policy で panic しない")
		}
	}()
	l.Allow("nope", Subjects{})
}

func TestSweepRemovesIdleFullBuckets(t *testing.T) {
	l, c := newLimiter()
	l.Allow("send", Subjects{User: "idle"})
	c.Advance(time.Hour)
	for i := 0; i < sweepEvery; i++ {
		l.Allow("send", Subjects{User: "busy"})
		c.Advance(5 * time.Second)
	}
	if _, ok := l.buckets[bucketKey{"send", "user", "idle"}]; ok {
		t.Fatal("使われていない満ちた bucket が残る")
	}
	if _, ok := l.buckets[bucketKey{"send", "user", "busy"}]; !ok {
		t.Fatal("使われている bucket を消した")
	}
}

func TestUpdateKeepsMatchingBuckets(t *testing.T) {
	l, _ := newLimiter()
	l.Allow("send", Subjects{User: "u1"})
	l.Allow("login", Subjects{Ip: "203.0.113.1", Device: "d1"})
	l.Update([]gen.Policy{policies[1]})
	if _, ok := l.buckets[bucketKey{"send", "user", "u1"}]; !ok {
		t.Fatal("残る policy の bucket を消した")
	}
	if len(l.buckets) != 1 || len(l.Policies()) != 1 {
		t.Fatalf("消えた policy の bucket が残る: %d", len(l.buckets))
	}
}

func TestGeneratedPoliciesAreValid(t *testing.T) {
	l := New(clock.WallClock{}, gen.Policies)
	if len(l.Policies()) != len(gen.Policies) {
		t.Fatal("生成の表が入らない")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("window が 0 の上限で panic しない")
		}
	}()
	New(clock.WallClock{}, []gen.Policy{{Name: "x", Limits: []gen.Limit{{Key: "ip", Limit: 1, Window: 0}}}})
}
