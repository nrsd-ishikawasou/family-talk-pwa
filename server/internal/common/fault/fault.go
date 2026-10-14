// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package fault は失敗の型を持つ（srv-common B-005、全体実装設計書 2.7）。
// Event はエラーコード表にある予期した失敗、Fault は予期しない失敗（応答は SRV-003）。
package fault

import (
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// Event はエラーコード表の 1 つの番号の発生（基本設計書 第1章 10.3）。
type Event struct {
	Code      string
	Status    int
	Retryable bool
	Params    map[string]any
	// Instance は要求の識別子（ログの req_id と同じ値）。srv-boundary が応答の時に入れる（第2章 9.1）。
	Instance uuid.UUID
}

// Problem は RFC 9457 の problem に code、retryable、params を加えた形（api/schemas/problem/1.json）。
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	Instance  string         `json:"instance,omitempty"`
	Code      string         `json:"code"`
	Retryable bool           `json:"retryable"`
	Params    map[string]any `json:"params,omitempty"`
}

// Error は番号を返す（本文や引数の値は含めない）。
func (e *Event) Error() string {
	return "fault: " + e.Code
}

// Problem は応答の形にする。origin は家族のサーバーの「https://名前」で、type の URL の頭になる。
// instance は `urn:uuid:` と要求の識別子（第2章 9.1）。detail は使わない（利用者に見せる文言は端末が code で引く）。
func (e *Event) Problem(origin string) Problem {
	def := gen.Errors[e.Code]
	p := Problem{
		Type:      origin + "/errors/" + e.Code,
		Title:     def.Title,
		Status:    e.Status,
		Code:      e.Code,
		Retryable: e.Retryable,
	}
	if len(e.Params) > 0 {
		p.Params = e.Params
	}
	if e.Instance != uuid.Nil {
		p.Instance = "urn:uuid:" + e.Instance.String()
	}
	return p
}

// Fault は予期しない失敗。中身（Cause と Stack）はログにだけ書き、応答には出さない。
type Fault struct {
	Instance uuid.UUID
	Cause    error
	Stack    string
}

// stackDepth は Wrap が取る呼び出しの深さ。
const stackDepth = 32

// Wrap は cause を包み、呼んだ場所の stack（関数、ファイル、行。引数の値は入らない）を取る。
func Wrap(cause error) *Fault {
	var pcs [stackDepth]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		f, more := frames.Next()
		b.WriteString(f.Function)
		b.WriteString(" ")
		b.WriteString(f.File)
		b.WriteString(":")
		b.WriteString(strconv.Itoa(f.Line))
		b.WriteString("\n")
		if !more {
			break
		}
	}
	return &Fault{Cause: cause, Stack: b.String()}
}

// Error は Fault の印と cause の型の名を返す（cause の文は返さない。秘密が混ざりうる）。
func (f *Fault) Error() string {
	if f.Cause == nil {
		return "fault: internal"
	}
	return "fault: internal (" + reflect.TypeOf(f.Cause).String() + ")"
}

// Unwrap は errors.Is と errors.As のために cause を返す。
func (f *Fault) Unwrap() error {
	return f.Cause
}

// AsEvent は SRV-003（500）の Event にする。Instance は同じ値。
func (f *Fault) AsEvent() *Event {
	e := New(gen.SRV003, nil)
	e.Instance = f.Instance
	return e
}

// Lookup はエラーコード表の 1 行を返す（生成ファイル gen.Errors）。
func Lookup(code string) (gen.ErrorDef, bool) {
	def, ok := gen.Errors[code]
	return def, ok
}

// New は code の Event を作る。status と retryable は表から入れる。
// 表で「状態で決まる」（gen.RetryByState）の番号は、呼ぶ側が Retryable を入れる。
// 呼ぶ側は gen の番号の定数を渡す。表に無い code、HTTP の応答に現れない番号（status が 0）、
// 警告の行（再試行の欄が無い）はプログラムの誤りなので panic する（全番号を試験が引く）。
func New(code string, params map[string]any) *Event {
	def, ok := gen.Errors[code]
	if !ok {
		panic("fault: エラーコード表に無い番号: " + code)
	}
	if def.Status == 0 {
		panic("fault: HTTP の応答に現れない番号: " + code)
	}
	if def.Retry == gen.RetryNotApplicable {
		panic("fault: 警告の行は応答の problem に使わない: " + code)
	}
	return &Event{Code: code, Status: def.Status, Retryable: def.Retry == gen.RetryYes, Params: params}
}

// Codes は表の全番号（昇順。試験が全番号を引くため）。
func Codes() []string {
	out := make([]string, 0, len(gen.Errors))
	for c := range gen.Errors {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
