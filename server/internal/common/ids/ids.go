// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package ids は識別子と token を作る（srv-common B-004、基本設計書 第1章 8.2）。
package ids

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strings"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/clock"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// Uuid は UUIDv7（RFC 9562）。
type Uuid = uuid.UUID

// TokenBytes は token の長さ（256 ビット）。
const TokenBytes = 32

// userCodeAlphabet は端末の追加のコードの文字（I、O、0、1 を除く 32 文字。第3章 4.4）。
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// Ids は時計と乱数を受けて識別子を作る。試験は決まった時計と乱数で同じ列を再現する。
type Ids struct {
	gen  *uuid.Gen
	rand io.Reader
}

// New は clk の時刻と rand の乱数で作る Ids を返す。
// gofrs の NewV7 は RFC 9562 6.2 の方法 1（12 ビットの単調な計数）で、時計が戻っても順序を保つ。
func New(clk clock.Clock, rand io.Reader) *Ids {
	g := uuid.NewGenWithOptions(uuid.WithEpochFunc(clk.Now), uuid.WithRandomReader(rand))
	return &Ids{gen: g, rand: rand}
}

// NewV7 は UUIDv7 を作る。乱数が読めないのは続けられない誤りなので panic する（crypto/rand と同じ扱い）。
func (g *Ids) NewV7() Uuid {
	u, err := g.gen.NewV7()
	if err != nil {
		panic("ids: 乱数が読めない: " + err.Error())
	}
	return u
}

// Parse は文字列の UUID を読む。形が違う、または版が 7 でなければ NET-001（要求の形の誤り）の Event を返す。
// params はエラーコード表の NET-001 の形（errors の要素は path と message。path は誤りの場所の名）。
func (g *Ids) Parse(s string) (Uuid, *fault.Event) {
	u, err := uuid.FromString(strings.ToLower(s))
	if err != nil || u.Version() != uuid.V7 {
		return uuid.Nil, fault.New(gen.NET001, map[string]any{"errors": []map[string]any{{"path": "id", "message": "UUIDv7 でない"}}})
	}
	return u, nil
}

// NewToken は 256 ビットの乱数（招待、端末の追加のコード、セッション）。
func (g *Ids) NewToken() []byte {
	b := make([]byte, TokenBytes)
	if _, err := io.ReadFull(g.rand, b); err != nil {
		panic("ids: 乱数が読めない: " + err.Error())
	}
	return b
}

// EncodeToken は URL-safe の Base64（埋めの = なし、43 文字）。
func EncodeToken(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken は DB に置く側の SHA-256。
func HashToken(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// UserCode は端末の表示用の 8 文字（I、O、0、1 を除く Base32。4 文字ずつ - で区切る。第3章 4.4）。
// 32 は 256 を割り切るので、各バイトの下位 5 ビットで表を引いても偏らない。
func (g *Ids) UserCode() string {
	var b [8]byte
	if _, err := io.ReadFull(g.rand, b[:]); err != nil {
		panic("ids: 乱数が読めない: " + err.Error())
	}
	out := make([]byte, 0, 9)
	for i, x := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeAlphabet[x&31])
	}
	return string(out)
}
