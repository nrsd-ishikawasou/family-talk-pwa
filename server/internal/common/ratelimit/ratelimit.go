// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package ratelimit はレート制限の器（srv-common B-008、基本設計書 第2章 9.3、第1章 10.6）。
// key ごとの token bucket を記憶の上に持つ（api は 1 プロセス）。表は生成ファイル gen.Policies が初期値。
package ratelimit

import (
	"math"
	"sync"
	"time"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// sweepEvery は掃除の間隔（Allow の回数）。
const sweepEvery = 1000

// Subjects は 1 つの要求の数える相手（ratelimits.yaml の key：ip、user、device、room、connection）。
// 空の欄は、その key の上限を数えない（例：ログインの前は user が無い）。
type Subjects struct {
	Ip         string
	User       string
	Device     string
	Room       string
	Connection string
}

// value は key の名に当たる欄の値。
func (s Subjects) value(key string) string {
	switch key {
	case "ip":
		return s.Ip
	case "user":
		return s.User
	case "device":
		return s.Device
	case "room":
		return s.Room
	case "connection":
		return s.Connection
	}
	return ""
}

// Decision は判定の結果。拒んだ時は RetryAfter が 429 の Retry-After になる（RFC 6585）。
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	Remaining  int
	Limit      int
	// Window は Remaining と Limit の上限の窓（秒。RateLimit-Policy の w。第2章 9.3）。
	Window int
	Reset  time.Time
}

// Limiter は全 policy の bucket を持つ。
type Limiter struct {
	clk      clock.Clock
	mu       sync.Mutex
	policies map[string]gen.Policy
	order    []gen.Policy
	buckets  map[bucketKey]*bucket
	calls    int
}

type bucketKey struct {
	policy string
	key    string
	value  string
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New は clk の時刻で数える Limiter を作る。
func New(clk clock.Clock, policies []gen.Policy) *Limiter {
	l := &Limiter{clk: clk, buckets: map[bucketKey]*bucket{}}
	l.set(policies)
	return l
}

// set は表を入れる（mu の中か、作る時に呼ぶ）。limit と window が正でない上限はプログラムの誤り（panic）。
func (l *Limiter) set(policies []gen.Policy) {
	for _, p := range policies {
		for _, x := range p.Limits {
			if x.Limit <= 0 || x.Window <= 0 || x.Burst < 0 {
				panic("ratelimit: 上限の limit と window は正: " + p.Name)
			}
		}
	}
	l.policies = map[string]gen.Policy{}
	l.order = append([]gen.Policy(nil), policies...)
	for _, p := range policies {
		l.policies[p.Name] = p
	}
}

// capacity は上限の容量（burst が 0 なら limit）。
func capacity(x gen.Limit) float64 {
	if x.Burst > 0 {
		return float64(x.Burst)
	}
	return float64(x.Limit)
}

// rate は補充の速さ（個/秒）。
func rate(x gen.Limit) float64 {
	return float64(x.Limit) / float64(x.Window)
}

// refill は bucket を now まで補充する。
func refill(b *bucket, x gen.Limit, now time.Time) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(capacity(x), b.tokens+elapsed*rate(x))
	}
	b.last = now
}

// Allow は policy の全部の上限を確かめ、どれかが尽きれば拒む。拒んだ時はどの bucket も減らさない。
// 表に無い policy はプログラムの誤りなので panic する。
func (l *Limiter) Allow(policy string, s Subjects) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.policies[policy]
	if !ok {
		panic("ratelimit: 表に無い policy: " + policy)
	}
	now := l.clk.Now()
	type hit struct {
		x gen.Limit
		b *bucket
	}
	var hits []hit
	for _, x := range p.Limits {
		v := s.value(x.Key)
		if v == "" {
			continue
		}
		k := bucketKey{policy: p.Name, key: x.Key, value: v}
		b, ok := l.buckets[k]
		if !ok {
			b = &bucket{tokens: capacity(x), last: now}
			l.buckets[k] = b
		}
		refill(b, x, now)
		hits = append(hits, hit{x, b})
	}
	l.calls++
	if l.calls%sweepEvery == 0 {
		l.sweep(now)
	}
	if len(hits) == 0 {
		return Decision{Allowed: true}
	}
	allowed := true
	var retry float64
	for _, h := range hits {
		if h.b.tokens < 1 {
			allowed = false
			retry = math.Max(retry, (1-h.b.tokens)/rate(h.x))
		}
	}
	if allowed {
		for _, h := range hits {
			h.b.tokens--
		}
	}
	// 残りの最も少ない上限を応答の残量にする。
	tight := hits[0]
	for _, h := range hits[1:] {
		if h.b.tokens < tight.b.tokens {
			tight = h
		}
	}
	full := (capacity(tight.x) - tight.b.tokens) / rate(tight.x)
	d := Decision{
		Allowed:   allowed,
		Remaining: int(math.Max(0, math.Floor(tight.b.tokens))),
		Limit:     tight.x.Limit,
		Window:    tight.x.Window,
		Reset:     now.Add(time.Duration(math.Ceil(full)) * time.Second),
	}
	if !allowed {
		d.RetryAfter = time.Duration(math.Ceil(retry)) * time.Second
	}
	return d
}

// sweep は満ちていて 2 × Window 使われていない bucket を消す（mu の中で呼ぶ）。
// 満ちた bucket は消しても作り直すと同じ値になるので、判定は変わらない。
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		p, ok := l.policies[k.policy]
		if !ok {
			delete(l.buckets, k)
			continue
		}
		for _, x := range p.Limits {
			if x.Key != k.key {
				continue
			}
			idle := now.Sub(b.last)
			refilled := math.Min(capacity(x), b.tokens+idle.Seconds()*rate(x))
			if refilled >= capacity(x) && idle >= 2*time.Duration(x.Window)*time.Second {
				delete(l.buckets, k)
			}
		}
	}
}

// Policies は今の表。
func (l *Limiter) Policies() []gen.Policy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]gen.Policy(nil), l.order...)
}

// Update は表を入れ替える（設定の「レート制限」が変わった時。Config.Reload の後）。
// 同じ policy と key の bucket は残し、表から消えた物は消す。
func (l *Limiter) Update(policies []gen.Policy) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.set(policies)
	for k := range l.buckets {
		p, ok := l.policies[k.policy]
		keep := false
		if ok {
			for _, x := range p.Limits {
				if x.Key == k.key {
					keep = true
				}
			}
		}
		if !keep {
			delete(l.buckets, k)
		}
	}
}
