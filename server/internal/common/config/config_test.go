// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// baseEnv は必須の 2 項目を持つ環境変数。
func baseEnv() map[string]string {
	return map[string]string{"FT_DOMAIN": "family.example", "FT_DNS_PROVIDER": "testdns"}
}

type dbMap map[string]Value

func (m dbMap) Get(key string) (Value, bool) {
	v, ok := m[key]
	return v, ok
}

func mustValue(t *testing.T, v any) Value {
	t.Helper()
	x, err := NewValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func problemKeys(t *testing.T, err error) []string {
	t.Helper()
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("LoadError でない: %v", err)
	}
	var keys []string
	for _, p := range le.Problems {
		keys = append(keys, p.Key)
	}
	return keys
}

// R: R-11-4-1
func TestDefaultsAndFrequentFields(t *testing.T) {
	c, err := Load(baseEnv(), nil, gen.Items)
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "family.example" || c.HttpsPort != 443 || c.Mode != "A" || c.DataDir != "/data" || c.Tz != "Asia/Tokyo" {
		t.Fatalf("よく使う欄が違う: %+v", c)
	}
	if c.Source(gen.KeyServerHttpsPort) != LayerDefault || c.Source(gen.KeyServerDomain) != LayerEnv {
		t.Fatal("層の判定が違う")
	}
	if got := c.Get(gen.KeyRoomEditWindow).Duration(); got != 900*time.Second {
		t.Fatalf("既定の duration が違う: %v", got)
	}
}

func TestRequiredMissingStopsStartup(t *testing.T) {
	_, err := Load(map[string]string{}, nil, gen.Items)
	keys := problemKeys(t, err)
	if strings.Join(keys, ",") != "server.dns_provider,server.domain" {
		t.Fatalf("必須の欠けの鍵が違う: %v", keys)
	}
	if strings.Contains(err.Error(), "family") {
		t.Fatal("誤りの文に値が入っている")
	}
}

func TestEnvParsingAndRange(t *testing.T) {
	env := baseEnv()
	env["FT_HTTPS_PORT"] = "70000"
	env["FT_DDNS"] = "yes"
	env["FT_MODE"] = "Z"
	env["FT_HTTP_PORT"] = "8o"
	_, err := Load(env, nil, gen.Items)
	keys := strings.Join(problemKeys(t, err), ",")
	for _, k := range []string{"server.ddns", "server.http_port", "server.https_port", "server.mode"} {
		if !strings.Contains(keys, k) {
			t.Fatalf("%s の誤りを拾わない: %s", k, keys)
		}
	}
	if strings.Contains(err.Error(), "70000") || strings.Contains(err.Error(), "8o") {
		t.Fatal("誤りの文に値が入っている")
	}
}

func TestFileLayerRules(t *testing.T) {
	file := []byte("backup.local_path: /mnt/usb\nserver.domain: evil.example\nno.such.key: 1\n")
	_, err := Load(baseEnv(), file, gen.Items)
	keys := strings.Join(problemKeys(t, err), ",")
	if keys != "no.such.key,server.domain" {
		t.Fatalf("ファイルの層の誤りが違う: %s", keys)
	}
	c, err := Load(baseEnv(), []byte("backup.local_path: /mnt/usb\n"), gen.Items)
	if err != nil {
		t.Fatal(err)
	}
	if c.Get(gen.KeyBackupLocalPath).String() != "/mnt/usb" || c.Source(gen.KeyBackupLocalPath) != LayerFile {
		t.Fatal("ファイルの値が読めない")
	}
	if _, err := Load(baseEnv(), []byte(":\n- ["), gen.Items); err == nil {
		t.Fatal("壊れた YAML を受けた")
	}
}

func TestPrecedenceEnvFileDbDefault(t *testing.T) {
	c, err := Load(baseEnv(), []byte("backup.local_path: /from/file\n"), gen.Items)
	if err != nil {
		t.Fatal(err)
	}
	db := dbMap{
		gen.KeyBackupLocalPath: mustValue(t, "/from/db"),
		gen.KeyRoomEditWindow:  mustValue(t, 1800),
	}
	if err := c.Reload(db); err != nil {
		t.Fatal(err)
	}
	if c.Get(gen.KeyBackupLocalPath).String() != "/from/file" {
		t.Fatal("ファイルが DB より優先されない")
	}
	if c.Get(gen.KeyRoomEditWindow).Int() != 1800 || c.Source(gen.KeyRoomEditWindow) != LayerDb {
		t.Fatal("DB が既定値より優先されない")
	}
}

func TestReloadRejectsOutOfRangeAtomically(t *testing.T) {
	c, _ := Load(baseEnv(), nil, gen.Items)
	_ = c.Reload(dbMap{gen.KeyRoomEditWindow: mustValue(t, 1800)})
	err := c.Reload(dbMap{gen.KeyRoomEditWindow: mustValue(t, 3600), gen.KeyMediaPhotoOriginalMax: mustValue(t, 1)})
	var ev *fault.Event
	if !errors.As(err, &ev) || ev.Code != gen.ADM004 || ev.Params["key"] != gen.KeyMediaPhotoOriginalMax {
		t.Fatalf("ADM-004 でない: %v", err)
	}
	if c.Get(gen.KeyRoomEditWindow).Int() != 1800 {
		t.Fatal("誤りの時に DB の層が入れ替わった")
	}
}

func TestNullableItems(t *testing.T) {
	c, _ := Load(baseEnv(), nil, gen.Items)
	if err := c.Reload(dbMap{gen.KeyRoomRetractWindow: mustValue(t, nil)}); err != nil {
		t.Fatalf("null を許す項目の null を拒んだ: %v", err)
	}
	if !c.Get(gen.KeyRoomRetractWindow).IsNull() {
		t.Fatal("null が読めない")
	}
	if err := c.Reload(dbMap{gen.KeyRoomEditWindow: mustValue(t, nil)}); err == nil {
		t.Fatal("null を許さない項目の null を受けた")
	}
}

func TestUnknownKeyPanics(t *testing.T) {
	c, _ := Load(baseEnv(), nil, gen.Items)
	defer func() {
		if recover() == nil {
			t.Fatal("config.yaml に無い鍵で panic しない")
		}
	}()
	c.Get("no.such.key")
}

// R: R-1-2-3
func TestReadSecretFilePermissionsAndTrim(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s")
	if err := os.WriteFile(p, []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := ReadSecretFile(p)
	if err != nil || string(b) != "pw" {
		t.Fatalf("0600 の秘密が読めない、または改行が残る: %q %v", b, err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(p); err != nil {
		t.Fatalf("0400 の秘密が読めない: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(p); err == nil {
		t.Fatal("他人が読める秘密を受けた")
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, secretFileMax+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(big); err == nil {
		t.Fatal("大きすぎる秘密を受けた")
	}
}
