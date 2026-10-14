// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package logx はログ（srv-common B-006、基本設計書 第1章 10.4、第12章 11）。
// 標準出力に JSON Lines。値は Redactable だけを受け、本文・添付・トークン・鍵・Cookie・生の IP を型で出させない。
// msg は定数の文字列だけを渡す（変数を渡す呼び出しは lint で止める）。
package logx

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
)

// Level は水準。既定は info、管理者が debug に上げられる（設定 log.level）。
type Level = slog.Level

// 水準の値。
const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// codeKey は ErrorCounter に数える欄の鍵（エラーの番号）。
const codeKey = "code"

// Redactable はログに出してよい値。Redact の関数だけが作る。
type Redactable interface {
	slog.LogValuer
}

// Field はログの 1 つの欄。
type Field struct {
	Key   string
	Value Redactable
}

// ErrorCounter はエラーの番号ごとの件数を数える側（実装は srv-admin の LogStats。依存の逆転）。
type ErrorCounter interface {
	Add(component, code string)
}

// Logger は 1 つの部品の記録の口。With で欄を足した子を作る。
type Logger struct {
	shared    *shared
	component string
	fields    []Field
}

// shared は親と子で共有する水準と数える側。
type shared struct {
	mu      sync.RWMutex
	handler slog.Handler
	level   *slog.LevelVar
	counter ErrorCounter
	now     func() time.Time
}

// New は w に JSON Lines を書く Logger を作る。component は部品の名（ログの component）。
// now は Deps.Clock の Now（ログの ts）。slog の Logger を通さないので、時刻は now からだけ入る。
func New(w io.Writer, component string, level Level, now func() time.Time) *Logger {
	lv := new(slog.LevelVar)
	lv.Set(level)
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv, ReplaceAttr: rename})
	return &Logger{shared: &shared{handler: h, level: lv, now: now}, component: component}
}

// rename は組み込みの鍵を整える。time は ts（UTC）、level の値は小文字。
func rename(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "ts"
		a.Value = slog.StringValue(a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	case slog.LevelKey:
		a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
	}
	return a
}

// Debug は debug の記録。
func (l *Logger) Debug(msg string, fields ...Field) {
	l.log(LevelDebug, msg, fields)
}

// Info は info の記録。
func (l *Logger) Info(msg string, fields ...Field) {
	l.log(LevelInfo, msg, fields)
}

// Warn は warn の記録。code の欄があれば ErrorCounter.Add を呼ぶ。
func (l *Logger) Warn(msg string, fields ...Field) {
	l.log(LevelWarn, msg, fields)
}

// Error は error の記録。code の欄があれば ErrorCounter.Add を呼ぶ。
func (l *Logger) Error(msg string, fields ...Field) {
	l.log(LevelError, msg, fields)
}

// log は 1 件を書く。書き出しの失敗は捨てる（ログの失敗で業務を止めない）。
func (l *Logger) log(level Level, msg string, fields []Field) {
	s := l.shared
	ctx := context.Background()
	if !s.handler.Enabled(ctx, level) {
		return
	}
	r := slog.NewRecord(s.now(), level, msg, 0)
	r.AddAttrs(slog.String("component", l.component))
	code := ""
	for _, set := range [][]Field{l.fields, fields} {
		for _, f := range set {
			if f.Value == nil {
				continue
			}
			v := f.Value.LogValue()
			r.AddAttrs(slog.Attr{Key: f.Key, Value: v})
			if f.Key == codeKey {
				code = v.String()
			}
		}
	}
	_ = s.handler.Handle(ctx, r)
	if level >= LevelWarn && code != "" {
		s.mu.RLock()
		c := s.counter
		s.mu.RUnlock()
		if c != nil {
			c.Add(l.component, code)
		}
	}
}

// With は fields を全ての記録に足した子を返す（要求の識別子、構成員、端末、部品）。
func (l *Logger) With(fields ...Field) *Logger {
	merged := make([]Field, 0, len(l.fields)+len(fields))
	merged = append(merged, l.fields...)
	merged = append(merged, fields...)
	return &Logger{shared: l.shared, component: l.component, fields: merged}
}

// SetLevel は水準を変える（設定の変更の後。親と子の全部に効く）。
func (l *Logger) SetLevel(level Level) {
	l.shared.level.Set(level)
}

// SetCounter は数える側を登録する（第11章 12 の「ログ」の要約）。
func (l *Logger) SetCounter(c ErrorCounter) {
	l.shared.mu.Lock()
	defer l.shared.mu.Unlock()
	l.shared.counter = c
}

// redacted は LogValue を持つ値の型。Redact の関数だけが作る。
type redacted struct {
	v slog.Value
}

// LogValue はログに出す値。
func (r redacted) LogValue() slog.Value {
	return r.v
}

// Id は識別子（UUID）。そのまま出す。
func Id(v uuid.UUID) Redactable {
	return redacted{slog.StringValue(v.String())}
}

// Code はエラーの番号。
func Code(code string) Redactable {
	return redacted{slog.StringValue(code)}
}

// Count は件数。
func Count(n int64) Redactable {
	return redacted{slog.Int64Value(n)}
}

// Duration は所要（ミリ秒で出す。欄 dur_ms）。
func Duration(d time.Duration) Redactable {
	return redacted{slog.Int64Value(d.Milliseconds())}
}

// Path は経路の雛形（/rooms/{id}/events）。実際の id を含む経路は渡さない。
func Path(route string) Redactable {
	return redacted{slog.StringValue(route)}
}

// Trace はコードの場所（fault.Fault の Stack と Error。関数、ファイル、行、型の名だけで、値は入らない）。
// 他の文字列を渡さない（srv-boundary の Recover と誤りの口だけが使う）。
func Trace(s string) Redactable {
	return redacted{slog.StringValue(s)}
}

// Secret は常に "<redacted>" を出す。
func Secret(any) Redactable {
	return redacted{slog.StringValue("<redacted>")}
}

// Ip は常に "<ip>" を出す（生の IP は ip_log へ。第12章 11）。
func Ip(netip.Addr) Redactable {
	return redacted{slog.StringValue("<ip>")}
}

// LabelName は定数の名（仕事の種類、結果の分類、秘密の種類）。定数だけを渡す。
// 変数の string は型が合わずに渡せない。LabelName(x) の明示の変換は lint で止める。
type LabelName string

// Label は定数の名を出す（第12章 11 の「書く」：認証の成否、認可の失敗、レート制限、裏の処理の結果、秘密の生成と交代）。
func Label(name LabelName) Redactable {
	return redacted{slog.StringValue(string(name))}
}
