// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package boundary

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"
)

// recorder は応答を包み、状態と大きさを記録する。capture が真なら本文も写す（冪等の記録と応答の検証）。
type recorder struct {
	http.ResponseWriter
	status  int
	n       int64
	capture bool
	body    bytes.Buffer
}

// wrapWriter は w を recorder にする。既に recorder ならそれを返す。
func wrapWriter(w http.ResponseWriter) *recorder {
	if r, ok := w.(*recorder); ok {
		return r
	}
	return &recorder{ResponseWriter: w}
}

// WriteHeader は状態を記録する。2 回目以降は標準と同じく無視される。
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write は本文を書く。WriteHeader の前なら 200。
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.capture {
		r.body.Write(b)
	}
	n, err := r.ResponseWriter.Write(b)
	r.n += int64(n)
	return n, err
}

// Unwrap は http.ResponseController と WebSocket の部品が元の ResponseWriter（Hijacker、Flusher）を辿るため。
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Flush はロングポールの応答のため。
func (r *recorder) Flush() {
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

// written は応答を書き始めたか。
func (r *recorder) written() bool {
	return r.status != 0
}

// Problem は Event を application/problem+json で書く（第2章 9.1）。instance は要求の識別子。
// エラーコード表で log が真の番号は Warn で記録する（本文と秘密は書かない）。
func Problem(w http.ResponseWriter, r *http.Request, e *fault.Event) {
	ev := *e
	ev.Instance = RequestIdFrom(r.Context())
	origin := ""
	if ri := reqFrom(r.Context()); ri != nil {
		origin = ri.origin
		if gen.Errors[ev.Code].Log {
			ri.log.Warn("要求の誤り", logx.Field{Key: "code", Value: logx.Code(ev.Code)},
				logx.Field{Key: "op", Value: logx.Path(OperationFrom(r.Context()))})
		}
	}
	body, err := json.Marshal(ev.Problem(origin))
	if err != nil {
		body = []byte(`{"code":"SRV-003","status":500}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(ev.Status)
	_, _ = w.Write(body)
}

// writeError は handler と middleware の誤りを書く。Event はそのまま、他は Fault（SRV-003）として記録して書く。
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ev *fault.Event
	if errors.As(err, &ev) {
		Problem(w, r, ev)
		return
	}
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		Problem(w, r, tooLarge(big.Limit))
		return
	}
	var f *fault.Fault
	if !errors.As(err, &f) {
		f = fault.Wrap(err)
	}
	f.Instance = RequestIdFrom(r.Context())
	if ri := reqFrom(r.Context()); ri != nil {
		ri.log.Error("内部の誤り", logx.Field{Key: "op", Value: logx.Path(OperationFrom(r.Context()))},
			logx.Field{Key: "cause", Value: logx.Trace(f.Error())}, logx.Field{Key: "stack", Value: logx.Trace(f.Stack)})
	}
	Problem(w, r, f.AsEvent())
}
