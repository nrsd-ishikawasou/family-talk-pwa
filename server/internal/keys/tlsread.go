// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"encoding/binary"
)

// malformed は MLS の物の形の誤り（RFC 9420 の文法に合わない）。CommitReader の外へは KEY-009（CheckSender は ROOM-005）にして出す。
// reason は試験と記録のため（本文を含めない。第12章 11）。
type malformed struct {
	reason string
}

func (m *malformed) Error() string {
	return "keys: MLS の形の誤り: " + m.reason
}

// tlsReader は RFC 9420 の TLS 符号化（RFC 8446 の表記と 2.1.2 の可変長のベクタ）の読み手。最初の誤りを持ち、その後の読みは
// 全部ゼロの値を返す（呼ぶ側は最後に err を見る）。読んだバイト列は元の配列の切り出しで、写さない。
type tlsReader struct {
	b   []byte
	i   int
	err *malformed
}

// fail は最初の誤りを持つ。
func (r *tlsReader) fail(reason string) {
	if r.err == nil {
		r.err = &malformed{reason: reason}
	}
}

// take は n バイトを読む。足りなければ誤り。
func (r *tlsReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b)-r.i < n {
		r.fail("足りない")
		return nil
	}
	v := r.b[r.i : r.i+n : r.i+n]
	r.i += n
	return v
}

func (r *tlsReader) u8() uint8 {
	v := r.take(1)
	if v == nil {
		return 0
	}
	return v[0]
}

func (r *tlsReader) u16() uint16 {
	v := r.take(2)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint16(v)
}

func (r *tlsReader) u32() uint32 {
	v := r.take(4)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v)
}

func (r *tlsReader) u64() uint64 {
	v := r.take(8)
	if v == nil {
		return 0
	}
	return binary.BigEndian.Uint64(v)
}

// varint は可変長のベクタの長さ（RFC 9420 2.1.2）。先頭 2 ビットが 00 は 1 バイト、01 は 2 バイト、10 は 4 バイト、11 は誤り。
// 最小の符号化でなければ誤り（2 バイトで 64 未満、4 バイトで 16384 未満）。
func (r *tlsReader) varint() int {
	first := r.u8()
	if r.err != nil {
		return 0
	}
	switch first >> 6 {
	case 0:
		return int(first)
	case 1:
		v := uint32(first&0x3f)<<8 | uint32(r.u8())
		if r.err == nil && v < 64 {
			r.fail("長さが最小の符号化でない")
		}
		return int(v)
	case 2:
		rest := r.take(3)
		if rest == nil {
			return 0
		}
		v := uint32(first&0x3f)<<24 | uint32(rest[0])<<16 | uint32(rest[1])<<8 | uint32(rest[2])
		if v < 16384 {
			r.fail("長さが最小の符号化でない")
		}
		return int(v)
	default:
		r.fail("長さの先頭 2 ビットが 11")
		return 0
	}
}

// opaque は可変長のベクタ <V> のバイト列。
func (r *tlsReader) opaque() []byte {
	n := r.varint()
	return r.take(n)
}

// vector は可変長のベクタの中身を新しい読み手で読み、fn を中身が尽きるまで繰り返す。fn が中身の途中で止まる（要素の境で
// 終わらない）か、1 バイトも読まなければ誤り。
func (r *tlsReader) vector(fn func(*tlsReader)) {
	body := r.opaque()
	if r.err != nil {
		return
	}
	sub := &tlsReader{b: body}
	for sub.i < len(sub.b) && sub.err == nil {
		at := sub.i
		fn(sub)
		if sub.err == nil && sub.i == at {
			sub.fail("要素が空")
		}
	}
	if sub.err != nil {
		r.err = sub.err
	}
}

// optional は optional<T>（RFC 9420 2.1.1）。先頭 1 バイトが 0 なら無し、1 なら fn、他は誤り。戻りは有無。
func (r *tlsReader) optional(fn func(*tlsReader)) bool {
	switch r.u8() {
	case 0:
		return false
	case 1:
		fn(r)
		return r.err == nil
	default:
		r.fail("optional の先頭が 0 と 1 以外")
		return false
	}
}

// end は最後のバイトまで読んだかを確かめる（残りがあれば誤り）。
func (r *tlsReader) end() {
	if r.err == nil && r.i != len(r.b) {
		r.fail("最後のバイトの後に残りがある")
	}
}

// putVarint は可変長のベクタの長さを最小の符号化で足す（RFC 9420 2.1.2。KeyPackageRef と SignWithLabel の対象を組む）。
func putVarint(out []byte, n int) []byte {
	switch {
	case n < 1<<6:
		return append(out, byte(n))
	case n < 1<<14:
		return append(out, byte(0x40|n>>8), byte(n))
	default:
		return append(out, byte(0x80|n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
}

// putOpaque は可変長のベクタ <V> を足す。
func putOpaque(out, v []byte) []byte {
	return append(putVarint(out, len(v)), v...)
}
