// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package clock は時刻の読みを 1 か所に集める（srv-common B-003、基本設計書 第1章 10.7）。
// 他の package は time.Now を直接呼ばず、Deps.Clock を使う。
package clock

import (
	"sync"
	"time"
)

// Clock は時刻の読み。Now は UTC でミリ秒に丸めた値を返す。
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// WallClock は本物の時計。time.Now を呼ぶのはここだけ。
type WallClock struct{}

// Now は今の時刻（UTC、ミリ秒）。記録の時刻なので壁の時計を使う。
func (WallClock) Now() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

// After は d の後に 1 度だけ時刻を送る。
func (WallClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// TestClock は試験の時計。Set と Advance でだけ進む。
type TestClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []testWaiter
}

type testWaiter struct {
	at time.Time
	ch chan time.Time
}

// NewTestClock は t（UTC のミリ秒に丸める）から始まる試験の時計を作る。
func NewTestClock(t time.Time) *TestClock {
	return &TestClock{now: t.UTC().Truncate(time.Millisecond)}
}

// Now は試験の時計の今。
func (c *TestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After は時計が d 進んだ時に 1 度だけ時刻を送る。d が 0 以下なら即座に送る。
func (c *TestClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, testWaiter{at: at, ch: ch})
	return ch
}

// Set は時計を t に合わせ、期限を過ぎた After に送る。
func (c *TestClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC().Truncate(time.Millisecond)
	c.fire()
}

// Advance は時計を d 進める。
func (c *TestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fire()
}

// fire は期限を過ぎた待ちに今の時刻を送って外す（mu の中で呼ぶ）。ch は 1 の大きさなので送りで止まらない。
func (c *TestClock) fire() {
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}
