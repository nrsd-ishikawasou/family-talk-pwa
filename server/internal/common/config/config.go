// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package config は設定の 3 層を読む（srv-common B-001、基本設計書 第11章 7）。
// 層の優先：環境変数 > ファイル > DB > 既定値。項目ごとに置ける層は api/config.yaml の layers（生成ファイル gen.Items）。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// Layer は値がどの層から来たか（管理者の画面のロック表示）。
type Layer int

// 層。値が大きいほど優先が高い。
const (
	LayerDefault Layer = iota
	LayerDb
	LayerFile
	LayerEnv
)

// 層の名（config.yaml の layers の値）。
const (
	layerNameFile = "file"
	layerNameDb   = "db"
)

// secretFileMax は秘密のファイルの大きさの上限。
const secretFileMax = 64 << 10

// Value は 1 つの項目の値。既定値と同じ JSON の形で持つ（config.yaml の type：bool、bytes、duration、enum、int、list、object、percent、string）。
type Value struct {
	raw json.RawMessage
}

// valueOf は JSON の値から Value を作る。
func valueOf(raw []byte) Value {
	return Value{raw: json.RawMessage(bytes.TrimSpace(raw))}
}

// NewValue は Go の値を JSON にして Value を作る（DB の層の実装と試験が使う）。
func NewValue(v any) (Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Value{}, err
	}
	return valueOf(b), nil
}

// IsNull は値が無いこと（例：room.retract_window の null は無期限）。
func (v Value) IsNull() bool {
	return len(v.raw) == 0 || string(v.raw) == "null"
}

// Int は int、bytes、percent の値。整数でなければプログラムの誤り（型は config.yaml と生成ファイルで保証）。
func (v Value) Int() int64 {
	n, ok := asInt(v.raw)
	if !ok {
		panic("config: 整数でない値")
	}
	return n
}

// Bool は bool の値。
func (v Value) Bool() bool {
	var b bool
	if err := json.Unmarshal(v.raw, &b); err != nil {
		panic("config: bool でない値")
	}
	return b
}

// String は string と enum の値。
func (v Value) String() string {
	var s string
	if err := json.Unmarshal(v.raw, &s); err != nil {
		panic("config: 文字列でない値")
	}
	return s
}

// Duration は duration の値（config.yaml は秒）。
func (v Value) Duration() time.Duration {
	return time.Duration(v.Int()) * time.Second
}

// Raw は list と object の値の JSON。
func (v Value) Raw() json.RawMessage {
	return slices.Clone(v.raw)
}

// asInt は JSON の整数を読む（小数や文字列は偽）。
func asInt(raw []byte) (int64, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var n json.Number
	if err := d.Decode(&n); err != nil {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}

// DbLayer は DB の層（実装は srv-admin の settings_db 表）。
type DbLayer interface {
	Get(key string) (Value, bool)
}

// LoadProblem は起動を止める設定の誤りの 1 件。値は持たない（秘密を起動の記録に出さない）。
type LoadProblem struct {
	Key    string
	Reason string
}

// LoadError は起動を止める設定の誤りの一覧（第1章 7.1 の 1）。
type LoadError struct {
	Problems []LoadProblem
}

// Error は鍵と理由を並べる。
func (e *LoadError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.Key+": "+p.Reason)
	}
	return "config: " + strings.Join(parts, "; ")
}

// Config は読み終えた設定。よく使う項目は欄に持つ。
type Config struct {
	Domain    string
	HttpsPort int
	Mode      string
	DataDir   string
	Tz        string

	mu      sync.RWMutex
	catalog map[string]gen.Item
	env     map[string]Value
	file    map[string]Value
	db      map[string]Value
}

// Get は key の値を層の優先で返す。config.yaml に無い鍵はプログラムの誤りなので panic する。
func (c *Config) Get(key string) Value {
	v, _ := c.lookup(key)
	return v
}

// Source は key の値がどの層から来たか。
func (c *Config) Source(key string) Layer {
	_, l := c.lookup(key)
	return l
}

// lookup は環境変数、ファイル、DB、既定値の順に引く。
func (c *Config) lookup(key string) (Value, Layer) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	it, ok := c.catalog[key]
	if !ok {
		panic("config: config.yaml に無い鍵: " + key)
	}
	if v, ok := c.env[key]; ok {
		return v, LayerEnv
	}
	if v, ok := c.file[key]; ok {
		return v, LayerFile
	}
	if v, ok := c.db[key]; ok {
		return v, LayerDb
	}
	return valueOf([]byte(it.DefaultJSON)), LayerDefault
}

// Reload は DB の層だけを読み直す（管理者の画面の変更の後）。範囲の外の値があれば ADM-004 の Event を返し、
// DB の層を入れ替えない（全部か無しか）。
func (c *Config) Reload(db DbLayer) error {
	next := map[string]Value{}
	for _, key := range sortedKeys(c.catalog) {
		it := c.catalog[key]
		if !slices.Contains(it.Layers, layerNameDb) {
			continue
		}
		v, ok := db.Get(key)
		if !ok {
			continue
		}
		if reason := check(it, v.raw); reason != "" {
			return fault.New(gen.ADM004, map[string]any{"key": key})
		}
		next[key] = v
	}
	c.mu.Lock()
	c.db = next
	c.mu.Unlock()
	return nil
}

// Load は環境変数（env）、ファイル（/data/config/server.yaml の中身）、項目の一覧（catalog）から Config を作る。
// 必須の欠け、範囲の外、置けない層の値（例：ファイルに server.domain）、config.yaml に無い鍵は、
// 起動を止める誤りとして返す（第1章 7.1 の 1）。誤りは鍵と理由を持ち、値は持たない（秘密を起動の記録に出さない）。
func Load(env map[string]string, fileYaml []byte, catalog []gen.Item) (*Config, error) {
	c := &Config{catalog: map[string]gen.Item{}, env: map[string]Value{}, file: map[string]Value{}, db: map[string]Value{}}
	for _, it := range catalog {
		c.catalog[it.Key] = it
	}
	var problems []LoadProblem
	add := func(key, reason string) { problems = append(problems, LoadProblem{Key: key, Reason: reason}) }

	for _, key := range sortedKeys(c.catalog) {
		it := c.catalog[key]
		if it.Env == "" {
			continue
		}
		s, ok := env[it.Env]
		if !ok || s == "" {
			continue
		}
		raw, reason := parseEnv(it, s)
		if reason == "" {
			reason = check(it, raw)
		}
		if reason != "" {
			add(key, reason)
			continue
		}
		c.env[key] = valueOf(raw)
	}

	if len(bytes.TrimSpace(fileYaml)) > 0 {
		var m map[string]any
		if err := yaml.Unmarshal(fileYaml, &m); err != nil {
			add("server.yaml", "YAML として読めない")
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			it, ok := c.catalog[key]
			if !ok {
				add(key, "config.yaml に無い鍵")
				continue
			}
			if !slices.Contains(it.Layers, layerNameFile) {
				add(key, "ファイルの層に置けない")
				continue
			}
			raw, err := json.Marshal(m[key])
			if err != nil {
				add(key, "値を JSON にできない")
				continue
			}
			if reason := check(it, raw); reason != "" {
				add(key, reason)
				continue
			}
			c.file[key] = valueOf(raw)
		}
	}

	for _, key := range sortedKeys(c.catalog) {
		it := c.catalog[key]
		if !it.Required {
			continue
		}
		if _, ok := c.env[key]; !ok {
			add(key, "必須の値が無い（環境変数 "+it.Env+"）")
		}
	}
	if len(problems) > 0 {
		return nil, &LoadError{Problems: problems}
	}

	c.Domain = c.Get(gen.KeyServerDomain).String()
	c.HttpsPort = int(c.Get(gen.KeyServerHttpsPort).Int())
	c.Mode = c.Get(gen.KeyServerMode).String()
	c.DataDir = c.Get(gen.KeyServerDataDir).String()
	c.Tz = c.Get(gen.KeyServerTz).String()
	return c, nil
}

// parseEnv は環境変数の文字列を項目の型の JSON にする。誤りは理由を返す。
func parseEnv(it gen.Item, s string) ([]byte, string) {
	switch it.Type {
	case "int", "bytes", "duration", "percent":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, "10 進の整数でない"
		}
		return []byte(strconv.FormatInt(n, 10)), ""
	case "bool":
		if s != "true" && s != "false" {
			return nil, "true か false でない"
		}
		return []byte(s), ""
	case "string", "enum":
		b, _ := json.Marshal(s)
		return b, ""
	default:
		return nil, "環境変数で与えられない型（" + it.Type + "）"
	}
}

// check は値が項目の型と範囲に合うかを確かめる。合わなければ理由を返す（値は理由に入れない）。
func check(it gen.Item, raw []byte) string {
	if len(raw) == 0 || string(raw) == "null" {
		if it.Nullable {
			return ""
		}
		return "null は許されない"
	}
	switch it.Type {
	case "int", "bytes", "duration", "percent":
		n, ok := asInt(raw)
		if !ok {
			return "整数でない"
		}
		if it.Min != nil && n < *it.Min {
			return fmt.Sprintf("下限 %d より小さい", *it.Min)
		}
		if it.Max != nil && n > *it.Max {
			return fmt.Sprintf("上限 %d より大きい", *it.Max)
		}
	case "bool":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return "bool でない"
		}
	case "string":
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "文字列でない"
		}
	case "enum":
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "文字列でない"
		}
		if !slices.Contains(it.Enum, s) {
			return "選べる値（" + strings.Join(it.Enum, "、") + "）でない"
		}
	case "list":
		var a []any
		if json.Unmarshal(raw, &a) != nil {
			return "配列でない"
		}
	case "object":
		var o map[string]any
		if json.Unmarshal(raw, &o) != nil {
			return "オブジェクトでない"
		}
	default:
		return "知らない型（" + it.Type + "）"
	}
	return ""
}

// sortedKeys は鍵の昇順（誤りの並びを決まった順にする）。
func sortedKeys(m map[string]gen.Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReadSecretFile は秘密のファイル（*_FILE と /run/secrets）を読む。所有者が自分でない（ホストの root は可）、
// またはグループか他人に権限があれば誤り（第12章 10）。末尾の改行は 1 つだけ外す。
func ReadSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("config: 秘密のファイルにグループか他人の権限がある: " + path)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		uid := uint32(os.Getuid())
		if sys.Uid != uid && uid != 0 {
			return nil, errors.New("config: 秘密のファイルの所有者が違う: " + path)
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, secretFileMax+1))
	if err != nil {
		return nil, err
	}
	if len(b) > secretFileMax {
		return nil, errors.New("config: 秘密のファイルが大きすぎる: " + path)
	}
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b, nil
}
