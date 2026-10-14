// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
)

func kek(t *testing.T) secret.KekWithId {
	t.Helper()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return secret.KekWithId{Id: uuid.Must(uuid.NewV7()), Key: k}
}

func isFault(err error) bool {
	_, ok := err.(*fault.Fault)
	return ok
}

// R: R-12-7-1
func TestSealOpenRoundTripAndBinding(t *testing.T) {
	e := New(rand.Reader)
	dek := e.NewDek()
	dekId := uuid.Must(uuid.NewV7())
	aad := []byte("user_private.birthday:" + dekId.String())
	f, err := e.Seal(dek, dekId, []byte("1990-01-02"), aad)
	if err != nil {
		t.Fatal(err)
	}
	enc := f.Encode()
	if len(enc) != FenvHeaderBytes+len("1990-01-02")+16 || enc[0] != 1 {
		t.Fatalf("fenv/1 の並びが違う: %d", len(enc))
	}
	back, err := DecodeFenv(enc)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := e.Open(dek, dekId, back, aad)
	if err != nil || string(pt) != "1990-01-02" {
		t.Fatalf("開けない: %v", err)
	}
	// 文脈が違う（別の行へ移した）
	if _, err := e.Open(dek, dekId, back, []byte("other row")); !isFault(err) {
		t.Fatal("別の文脈で開けた")
	}
	// DEK の id が違う
	if _, err := e.Open(dek, uuid.Must(uuid.NewV7()), back, aad); !isFault(err) {
		t.Fatal("違う dek_id で開いた")
	}
	// 頭の改竄（dek_id を書き換え、渡す id も合わせる）
	tampered := bytes.Clone(enc)
	tampered[5] ^= 1
	tf, _ := DecodeFenv(tampered)
	if _, err := e.Open(dek, tf.DekId, tf, aad); !isFault(err) {
		t.Fatal("頭を改竄した暗号文が開けた")
	}
	// 同じ平文でも nonce が違う
	f2, _ := e.Seal(dek, dekId, []byte("1990-01-02"), aad)
	if f2.Nonce == f.Nonce || bytes.Equal(f2.Ciphertext, f.Ciphertext) {
		t.Fatal("nonce が使い回された")
	}
}

func TestWrapUnwrapSelectsKekById(t *testing.T) {
	e := New(rand.Reader)
	oldKek, newKek := kek(t), kek(t)
	dek := e.NewDek()
	dekId := uuid.Must(uuid.NewV7())
	f, err := e.WrapDek(oldKek, dekId, dek)
	if err != nil {
		t.Fatal(err)
	}
	enc := f.Encode()
	if len(enc) != FdekBytes || enc[0] != 1 {
		t.Fatalf("fdek/1 が 89 バイトでない: %d", len(enc))
	}
	back, err := DecodeFdek(enc)
	if err != nil || back.KekId != oldKek.Id {
		t.Fatalf("fdek/1 が読めない: %v", err)
	}
	got, err := e.UnwrapDek([]secret.KekWithId{newKek, oldKek}, dekId, back)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("交代中の前の KEK で開けない: %v", err)
	}
	if _, err := e.UnwrapDek([]secret.KekWithId{newKek}, dekId, back); !isFault(err) {
		t.Fatal("合う KEK が無いのに開いた")
	}
	if _, err := e.UnwrapDek([]secret.KekWithId{oldKek}, uuid.Must(uuid.NewV7()), back); !isFault(err) {
		t.Fatal("別の構成員の dek_id で開いた（包みを移した）")
	}
}

func TestDecodeRejectsBadVersionAndLength(t *testing.T) {
	if _, err := DecodeFdek(make([]byte, 88)); err == nil {
		t.Fatal("88 バイトの fdek を受けた")
	}
	b := make([]byte, FdekBytes)
	b[0] = 2
	if _, err := DecodeFdek(b); err == nil {
		t.Fatal("版 2 の fdek を受けた")
	}
	if _, err := DecodeFenv(make([]byte, FenvHeaderBytes+15)); err == nil {
		t.Fatal("短い fenv を受けた")
	}
	c := make([]byte, FenvHeaderBytes+16)
	c[0] = 0
	if _, err := DecodeFenv(c); err == nil {
		t.Fatal("版 0 の fenv を受けた")
	}
}
