// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package envelope はエンベロープ暗号化（srv-common B-007、基本設計書 第12章 6.2、12）。
// 構成員ごとの DEK で機微なメタデータを AES-256-GCM で暗号化し（fenv/1）、
// DEK は KEK で XChaCha20-Poly1305 により包む（fdek/1）。
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"

	"github.com/gofrs/uuid/v5"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
)

// 形式の版と長さ（第12章 12）。
const (
	Version = 1

	DekBytes = 32

	// FenvHeaderBytes は fenv/1 の頭：版 1、dek_id 16、nonce 12。
	FenvHeaderBytes = 1 + 16 + 12
	// FdekBytes は fdek/1 の全体：版 1、kek_id 16、nonce 24、包んだ DEK 48。
	FdekBytes = 1 + 16 + 24 + 48

	fdekHeaderBytes = 1 + 16 + 24
	gcmTagBytes     = 16
)

// 形の誤り（DB の値が壊れている。呼ぶ側は Fault にする）。
var (
	errVersion = errors.New("envelope: 版が 1 でない")
	errLength  = errors.New("envelope: 長さが合わない")
)

// Fenv は DEK で暗号化した値（DB の bytea）。KEK の id は持たない。
type Fenv struct {
	V          byte
	DekId      uuid.UUID
	Nonce      [12]byte
	Ciphertext []byte
}

// header は版、dek_id、nonce の 29 バイト。
func (f Fenv) header() []byte {
	h := make([]byte, 0, FenvHeaderBytes)
	h = append(h, f.V)
	h = append(h, f.DekId.Bytes()...)
	h = append(h, f.Nonce[:]...)
	return h
}

// Encode は版、dek_id、nonce、暗号文と認証タグの順のバイト列にする。
func (f Fenv) Encode() []byte {
	return append(f.header(), f.Ciphertext...)
}

// DecodeFenv はバイト列を読む。版が違う、または短ければ誤り。
func DecodeFenv(b []byte) (Fenv, error) {
	if len(b) < FenvHeaderBytes+gcmTagBytes {
		return Fenv{}, errLength
	}
	if b[0] != Version {
		return Fenv{}, errVersion
	}
	var f Fenv
	f.V = b[0]
	f.DekId = uuid.FromBytesOrNil(b[1:17])
	copy(f.Nonce[:], b[17:29])
	f.Ciphertext = append([]byte(nil), b[29:]...)
	return f, nil
}

// Fdek は KEK で包んだ DEK（DB の user_dek.wrapped）。
type Fdek struct {
	V       byte
	KekId   uuid.UUID
	Nonce   [24]byte
	Wrapped [48]byte
}

// header は版、kek_id、nonce の 41 バイト。
func (f Fdek) header() []byte {
	h := make([]byte, 0, fdekHeaderBytes)
	h = append(h, f.V)
	h = append(h, f.KekId.Bytes()...)
	h = append(h, f.Nonce[:]...)
	return h
}

// Encode は版、kek_id、nonce、包んだ DEK の順の 89 バイトにする。
func (f Fdek) Encode() []byte {
	return append(f.header(), f.Wrapped[:]...)
}

// DecodeFdek はバイト列を読む。版が違う、または長さが 89 でなければ誤り。
func DecodeFdek(b []byte) (Fdek, error) {
	if len(b) != FdekBytes {
		return Fdek{}, errLength
	}
	if b[0] != Version {
		return Fdek{}, errVersion
	}
	var f Fdek
	f.V = b[0]
	f.KekId = uuid.FromBytesOrNil(b[1:17])
	copy(f.Nonce[:], b[17:41])
	copy(f.Wrapped[:], b[41:])
	return f, nil
}

// Envelope は乱数の読み口を持つ暗号化の操作。
type Envelope struct {
	rand io.Reader
}

// New は rand（Deps.Rand）で nonce と DEK を作る Envelope を返す。
func New(rand io.Reader) *Envelope {
	return &Envelope{rand: rand}
}

// fill は b を乱数で埋める。乱数が読めないのは続けられない誤りなので panic する。
func (e *Envelope) fill(b []byte) {
	if _, err := io.ReadFull(e.rand, b); err != nil {
		panic("envelope: 乱数が読めない: " + err.Error())
	}
}

// NewDek は 256 ビットの DEK を作る。
func (e *Envelope) NewDek() []byte {
	b := make([]byte, DekBytes)
	e.fill(b)
	return b
}

// WrapDek は dek を kek で包む。認証の追加データは頭の 41 バイトと dekId。
func (e *Envelope) WrapDek(kek secret.KekWithId, dekId uuid.UUID, dek []byte) (Fdek, error) {
	if len(dek) != DekBytes {
		return Fdek{}, fault.Wrap(errLength)
	}
	aead, err := chacha20poly1305.NewX(kek.Key)
	if err != nil {
		return Fdek{}, fault.Wrap(err)
	}
	f := Fdek{V: Version, KekId: kek.Id}
	e.fill(f.Nonce[:])
	sealed := aead.Seal(nil, f.Nonce[:], dek, append(f.header(), dekId.Bytes()...))
	copy(f.Wrapped[:], sealed)
	return f, nil
}

// UnwrapDek は f.KekId に合う KEK を keks から選んで開く。
// 合う KEK が無い、または開けなければ Fault（SRV-003）。KEK の交代の不整合だけで起きる。
func (e *Envelope) UnwrapDek(keks []secret.KekWithId, dekId uuid.UUID, f Fdek) ([]byte, error) {
	if f.V != Version {
		return nil, fault.Wrap(errVersion)
	}
	for _, k := range keks {
		if k.Id != f.KekId {
			continue
		}
		aead, err := chacha20poly1305.NewX(k.Key)
		if err != nil {
			return nil, fault.Wrap(err)
		}
		dek, err := aead.Open(nil, f.Nonce[:], f.Wrapped[:], append(f.header(), dekId.Bytes()...))
		if err != nil {
			return nil, fault.Wrap(err)
		}
		return dek, nil
	}
	return nil, fault.Wrap(errors.New("envelope: kek_id に合う KEK が無い"))
}

// gcm は dek の AES-256-GCM を作る。
func gcm(dek []byte) (cipher.AEAD, error) {
	if len(dek) != DekBytes {
		return nil, errLength
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal は plaintext を dek で暗号化する。aad は呼ぶ側の文脈（表、列、行の識別子）。
// 認証の追加データは頭の 29 バイトと aad。
func (e *Envelope) Seal(dek []byte, dekId uuid.UUID, plaintext, aad []byte) (Fenv, error) {
	a, err := gcm(dek)
	if err != nil {
		return Fenv{}, fault.Wrap(err)
	}
	f := Fenv{V: Version, DekId: dekId}
	e.fill(f.Nonce[:])
	f.Ciphertext = a.Seal(nil, f.Nonce[:], plaintext, append(f.header(), aad...))
	return f, nil
}

// Open は f を dek で開く。dek の id が f.DekId と違う、または開けなければ Fault（SRV-003）。
func (e *Envelope) Open(dek []byte, dekId uuid.UUID, f Fenv, aad []byte) ([]byte, error) {
	if f.V != Version {
		return nil, fault.Wrap(errVersion)
	}
	if f.DekId != dekId {
		return nil, fault.Wrap(errors.New("envelope: dek_id が違う"))
	}
	a, err := gcm(dek)
	if err != nil {
		return nil, fault.Wrap(err)
	}
	pt, err := a.Open(nil, f.Nonce[:], f.Ciphertext, append(f.header(), aad...))
	if err != nil {
		return nil, fault.Wrap(err)
	}
	return pt, nil
}
