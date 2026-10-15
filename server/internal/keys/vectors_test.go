// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 公式の相互運用の検証ベクトル（mlswg/mls-implementations の固定の版）。再配布の許諾の表示が無いので Git に入れず、試験の時に
// 取得して SHA-256 を照らす（試験設計書 04 の D-2）。置き場は利用者のキャッシュの下。環境の変数 FT_MLS_VECTORS で手元の
// ディレクトリを指せる。取得できないかハッシュが違えば試験は失敗（飛ばさない）。
const vectorsCommit = "2034fe72e0c805a683ba76165660c834f2a8142a"

var vectorsSha256 = map[string]string{
	"messages.json":                       "b194abe1561995223482dbad51c180146920dc2f637e74d01e07a388308791fb",
	"passive-client-welcome.json":         "92ebb04b67b1aca4290965ae363650921842ceebe275bd79a2733414078d849c",
	"passive-client-handling-commit.json": "b24949fdf857bf79d511f57504a8bdc7ce3247eb199684745e439c11b1a1da21",
	"crypto-basics.json":                  "17cfcf89af9f51d0f2aa7af77f6f9ec99376a039214b6d42a6f11646b83e8c29",
	"tree-operations.json":                "9a25f8720714d1256ba2dad8d83a660ba0bae0d33ed15c682b9d74a5d67aad0a",
}

// vector は名前のベクトルのバイト列（ハッシュを照らした物）。
func vector(t *testing.T, name string) []byte {
	t.Helper()
	want, ok := vectorsSha256[name]
	if !ok {
		t.Fatalf("固定していないベクトル: %s", name)
	}
	check := func(b []byte) bool {
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:]) == want
	}
	if dir := os.Getenv("FT_MLS_VECTORS"); dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !check(b) {
			t.Fatalf("FT_MLS_VECTORS の %s が読めないかハッシュが違う: %v", name, err)
		}
		return b
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "family-talk", "mls-vectors", vectorsCommit)
	path := filepath.Join(dir, name)
	if b, err := os.ReadFile(path); err == nil && check(b) {
		return b
	}
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Get("https://raw.githubusercontent.com/mlswg/mls-implementations/" + vectorsCommit + "/test-vectors/" + name)
	if err != nil {
		t.Fatalf("%s を取得できない: %v", name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !check(b) {
		t.Fatalf("%s を取得できないかハッシュが違う: %d %v", name, resp.StatusCode, err)
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		_ = os.WriteFile(path, b, 0o644)
	}
	return b
}

// vectorJSON はベクトルを v に読む。
func vectorJSON(t *testing.T, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(vector(t, name), v); err != nil {
		t.Fatal(err)
	}
}

// unhex は 16 進の文字列（ベクトルの値）。
func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
