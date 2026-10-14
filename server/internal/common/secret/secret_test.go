// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package secret

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
)

func dirs(t *testing.T) Dirs {
	t.Helper()
	root := t.TempDir()
	d := Dirs{Data: filepath.Join(root, "data"), Run: filepath.Join(root, "run"), Etc: filepath.Join(root, "etc")}
	for _, p := range []string{d.Data, d.Run, d.Etc} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func key32() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

// R: R-12-7-1
func TestWriteIsAtomicAnd0400(t *testing.T) {
	d := dirs(t)
	s := NewStore(d)
	if err := s.Write(NameBackup, []byte("pw")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(d.Data, NameBackup))
	if err != nil || st.Mode().Perm() != 0o400 {
		t.Fatalf("0400 でない: %v %v", st, err)
	}
	entries, _ := os.ReadDir(d.Data)
	if len(entries) != 1 {
		t.Fatalf("一時のファイルが残る: %v", entries)
	}
	b, err := s.Backup()
	if err != nil || string(b) != "pw" {
		t.Fatalf("書いた秘密が読めない: %q %v", b, err)
	}
	if !s.Exists(NameBackup) || s.Exists(NameVapid) {
		t.Fatal("Exists が違う")
	}
}

// R: R-1-2-3
func TestKekAndDnsTokenAreWrittenOnlyOnTheHost(t *testing.T) {
	d := dirs(t)
	container := NewStore(Dirs{Data: d.Data, Run: d.Run})
	if err := container.Write(NameKek, []byte("x")); err == nil {
		t.Fatal("コンテナの Store が /etc に書けた")
	}
	if err := container.Write(NameDnsToken, []byte("x")); err == nil {
		t.Fatal("コンテナの Store が dns_token を書けた")
	}
	host := NewStore(Dirs{Data: d.Data, Etc: d.Etc})
	id := uuid.Must(uuid.NewV7())
	file := jsonOf(t, map[string]any{"keys": []map[string]any{{"id": id.String(), "key": key32(), "created_at": time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}}})
	if err := host.Write(NameKek, file); err != nil {
		t.Fatal(err)
	}
	keks, err := host.Kek()
	if err != nil || len(keks) != 1 || keks[0].Id != id || len(keks[0].Key) != 32 {
		t.Fatalf("KEK が読めない: %v %v", keks, err)
	}
	// コンテナは /run/secrets から読む。
	if err := os.WriteFile(filepath.Join(d.Run, NameKek), file, 0o400); err != nil {
		t.Fatal(err)
	}
	if got, err := container.Kek(); err != nil || got[0].Id != id {
		t.Fatalf("コンテナが /run/secrets の KEK を読めない: %v", err)
	}
}

func TestKekRejectsNonV7AndShortKeys(t *testing.T) {
	d := dirs(t)
	s := NewStore(Dirs{Data: d.Data, Etc: d.Etc})
	v4 := uuid.Must(uuid.NewV4())
	for _, k := range []map[string]any{
		{"id": v4.String(), "key": key32()},
		{"id": uuid.Must(uuid.NewV7()).String(), "key": []byte("short")},
	} {
		if err := s.Write(NameKek, jsonOf(t, map[string]any{"keys": []map[string]any{k}})); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Kek(); err == nil {
			t.Fatalf("不正な KEK を受けた: %v", k)
		}
	}
}

func TestSessionKeysRequireHS256(t *testing.T) {
	d := dirs(t)
	s := NewStore(d)
	ok := map[string]any{"keys": []map[string]any{{"id": "k1", "alg": "HS256", "key": key32()}}}
	if err := s.Write(NameSessionKeys, jsonOf(t, ok)); err != nil {
		t.Fatal(err)
	}
	if ks, err := s.SessionKeys(); err != nil || ks[0].Id != "k1" {
		t.Fatalf("session-keys が読めない: %v", err)
	}
	bad := map[string]any{"keys": []map[string]any{{"id": "k1", "alg": "EdDSA", "key": key32()}}}
	if err := s.Write(NameSessionKeys, jsonOf(t, bad)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionKeys(); err == nil {
		t.Fatal("HS256 でない鍵を受けた")
	}
}

func TestVapidChecksPublicMatchesJwk(t *testing.T) {
	d := dirs(t)
	s := NewStore(d)
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.PublicKey().Bytes()
	enc := base64.RawURLEncoding
	jwk := map[string]any{"kty": "EC", "crv": "P-256", "x": enc.EncodeToString(pub[1:33]), "y": enc.EncodeToString(pub[33:]), "d": enc.EncodeToString(priv.Bytes())}
	if err := s.Write(NameVapid, jsonOf(t, map[string]any{"version": 2, "private": jwk, "public": enc.EncodeToString(pub)})); err != nil {
		t.Fatal(err)
	}
	v, err := s.Vapid()
	if err != nil || v.Version != 2 || !bytes.Equal(v.Public, pub) || !bytes.Equal(v.Private, priv.Bytes()) {
		t.Fatalf("VAPID が読めない: %v", err)
	}
	other, _ := ecdh.P256().GenerateKey(rand.Reader)
	if err := s.Write(NameVapid, jsonOf(t, map[string]any{"version": 2, "private": jwk, "public": enc.EncodeToString(other.PublicKey().Bytes())})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Vapid(); err == nil {
		t.Fatal("public と JWK が違う VAPID を受けた")
	}
}

func TestDbPasswordPrefersRunSecrets(t *testing.T) {
	d := dirs(t)
	if err := os.WriteFile(filepath.Join(d.Run, NameDbPassword), []byte("from-run\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Data, NameDbPassword), []byte("from-data"), 0o400); err != nil {
		t.Fatal(err)
	}
	if b, _ := NewStore(d).DbPassword(); string(b) != "from-run" {
		t.Fatalf("コンテナで /run/secrets を読まない: %q", b)
	}
	if b, _ := NewStore(Dirs{Data: d.Data}).DbPassword(); string(b) != "from-data" {
		t.Fatalf("ホストで /data/secrets を読まない: %q", b)
	}
}

func TestZero(t *testing.T) {
	b := []byte("secret")
	Zero(b)
	if !bytes.Equal(b, make([]byte, 6)) {
		t.Fatalf("ゼロで埋まらない: %v", b)
	}
}
