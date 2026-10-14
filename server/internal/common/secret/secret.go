// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package secret は秘密のファイルを読み書きする（srv-common B-002、基本設計書 第1章 8.5、第12章 10）。
// 秘密は []byte で持ち、使い終えたら Zero で埋める。生成と交代の判断は srv-admin が行う。
package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/config"
)

// ファイルの名（Write と Exists の name）。
const (
	NameSessionKeys = "session-keys"
	NameVapid       = "vapid"
	NameSfu         = "sfu"
	NameBackup      = "backup"
	NameDbPassword  = "db_password"
	NameKek         = "kek"
	NameDnsToken    = "dns_token"
)

// 決まった値。
const (
	algHS256     = "HS256"
	keyBytes     = 32
	containerUID = 10001
	fileMode     = 0o400
)

// KeyWithId はセッションの署名鍵（HMAC-SHA256。alg は HS256 だけ）。
type KeyWithId struct {
	Id          string
	Alg         string
	Key         []byte
	CreatedAt   time.Time
	RetireAfter time.Time
}

// VapidKey は VAPID の鍵（P-256）。Private は JWK の d、Public は非圧縮の点（65 バイト）。
type VapidKey struct {
	Version int
	Private []byte
	Public  []byte
}

// SfuKey は SFU の API の鍵と秘密。
type SfuKey struct {
	Id     string
	Secret []byte
}

// KekWithId は KEK（256 ビット）。Id は UUIDv7。
type KekWithId struct {
	Id        uuid.UUID
	Key       []byte
	CreatedAt time.Time
}

// Dirs は秘密の置き場。
//   - Data：/data/secrets（生成する秘密。コンテナとホストの両方）
//   - Run：/run/secrets（コンテナの中の Compose の secret：kek、db_password、dns_token）。ホストでは空
//   - Etc：/etc/family-talk/secrets（ホストの ft だけ。kek と dns_token を書く）。コンテナでは空
type Dirs struct {
	Data string
	Run  string
	Etc  string
}

// Store は秘密のファイルの置き場。
type Store struct {
	dirs Dirs
}

// NewStore は置き場を受けて Store を作る。
func NewStore(dirs Dirs) *Store {
	return &Store{dirs: dirs}
}

// readPath は name の読みの置き場。kek と dns_token はホストなら /etc、コンテナなら /run/secrets。
func (s *Store) readPath(name string) string {
	switch name {
	case NameKek, NameDnsToken:
		if s.dirs.Etc != "" {
			return filepath.Join(s.dirs.Etc, name)
		}
		return filepath.Join(s.dirs.Run, name)
	case NameDbPassword:
		if s.dirs.Run != "" {
			return filepath.Join(s.dirs.Run, name)
		}
		return filepath.Join(s.dirs.Data, name)
	default:
		return filepath.Join(s.dirs.Data, name)
	}
}

// writeDir は name の書きの置き場。kek と dns_token は Etc だけ。
func (s *Store) writeDir(name string) (string, error) {
	switch name {
	case NameKek, NameDnsToken:
		if s.dirs.Etc == "" {
			return "", errors.New("secret: " + name + " はホストの ft だけが書く（コンテナから /etc に書かない）")
		}
		return s.dirs.Etc, nil
	default:
		return s.dirs.Data, nil
	}
}

// readJSON は name の JSON を v に読み、元のバイト列を Zero する。
func (s *Store) readJSON(name string, v any) error {
	b, err := config.ReadSecretFile(s.readPath(name))
	if err != nil {
		return err
	}
	defer Zero(b)
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("secret: " + name + " の形が違う")
	}
	return nil
}

// SessionKeys は新旧 2 本のセッションの署名鍵。
func (s *Store) SessionKeys() ([]KeyWithId, error) {
	var f struct {
		Keys []struct {
			Id          string    `json:"id"`
			Alg         string    `json:"alg"`
			Key         []byte    `json:"key"`
			CreatedAt   time.Time `json:"created_at"`
			RetireAfter time.Time `json:"retire_after"`
		} `json:"keys"`
	}
	if err := s.readJSON(NameSessionKeys, &f); err != nil {
		return nil, err
	}
	out := make([]KeyWithId, 0, len(f.Keys))
	for _, k := range f.Keys {
		if k.Alg != algHS256 || len(k.Key) < keyBytes || k.Id == "" {
			return nil, errors.New("secret: session-keys の鍵が HS256 の 32 バイト以上でない")
		}
		out = append(out, KeyWithId{Id: k.Id, Alg: k.Alg, Key: k.Key, CreatedAt: k.CreatedAt, RetireAfter: k.RetireAfter})
	}
	if len(out) == 0 {
		return nil, errors.New("secret: session-keys に鍵が無い")
	}
	return out, nil
}

// Vapid は VAPID の鍵。JWK の crv が P-256 で、x と y が public と一致することを確かめる。
func (s *Store) Vapid() (VapidKey, error) {
	var f struct {
		Version int `json:"version"`
		Private struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
			D   string `json:"d"`
		} `json:"private"`
		Public string `json:"public"`
	}
	if err := s.readJSON(NameVapid, &f); err != nil {
		return VapidKey{}, err
	}
	dec := base64.RawURLEncoding
	d, errD := dec.DecodeString(f.Private.D)
	x, errX := dec.DecodeString(f.Private.X)
	y, errY := dec.DecodeString(f.Private.Y)
	pub, errP := dec.DecodeString(f.Public)
	if errors.Join(errD, errX, errY, errP) != nil || f.Private.Kty != "EC" || f.Private.Crv != "P-256" ||
		len(d) != 32 || len(x) != 32 || len(y) != 32 {
		return VapidKey{}, errors.New("secret: vapid の JWK が P-256 の形でない")
	}
	point := append(append([]byte{0x04}, x...), y...)
	if !bytes.Equal(point, pub) {
		return VapidKey{}, errors.New("secret: vapid の public が JWK の x と y に一致しない")
	}
	return VapidKey{Version: f.Version, Private: d, Public: pub}, nil
}

// Sfu は SFU の鍵（2 本）。
func (s *Store) Sfu() ([]SfuKey, error) {
	var f struct {
		Keys []struct {
			Id     string `json:"id"`
			Secret []byte `json:"secret"`
		} `json:"keys"`
	}
	if err := s.readJSON(NameSfu, &f); err != nil {
		return nil, err
	}
	out := make([]SfuKey, 0, len(f.Keys))
	for _, k := range f.Keys {
		if k.Id == "" || len(k.Secret) < keyBytes {
			return nil, errors.New("secret: sfu の鍵が 32 バイト以上でない")
		}
		out = append(out, SfuKey{Id: k.Id, Secret: k.Secret})
	}
	if len(out) == 0 {
		return nil, errors.New("secret: sfu に鍵が無い")
	}
	return out, nil
}

// Backup は restic のパスワード。
func (s *Store) Backup() ([]byte, error) {
	return config.ReadSecretFile(s.readPath(NameBackup))
}

// Kek は現在と交代中の前の KEK。Etc があれば Etc から、無ければ Run から読む。
func (s *Store) Kek() ([]KekWithId, error) {
	var f struct {
		Keys []struct {
			Id        string    `json:"id"`
			Key       []byte    `json:"key"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"keys"`
	}
	if err := s.readJSON(NameKek, &f); err != nil {
		return nil, err
	}
	out := make([]KekWithId, 0, len(f.Keys))
	for _, k := range f.Keys {
		id, err := uuid.FromString(k.Id)
		if err != nil || id.Version() != uuid.V7 || len(k.Key) != keyBytes {
			return nil, errors.New("secret: kek の鍵が UUIDv7 の id と 32 バイトの鍵でない")
		}
		out = append(out, KekWithId{Id: id, Key: k.Key, CreatedAt: k.CreatedAt})
	}
	if len(out) == 0 {
		return nil, errors.New("secret: kek に鍵が無い")
	}
	return out, nil
}

// DbPassword は DB のパスワード。
func (s *Store) DbPassword() ([]byte, error) {
	return config.ReadSecretFile(s.readPath(NameDbPassword))
}

// DnsToken は DNS の提供者の API トークン。
func (s *Store) DnsToken() ([]byte, error) {
	return config.ReadSecretFile(s.readPath(NameDnsToken))
}

// Exists は name のファイルがあるか（第1章 7.1 の 2 で生成するかの判断）。書ける置き場が無い名は読みの置き場を見る。
func (s *Store) Exists(name string) bool {
	path := s.readPath(name)
	if dir, err := s.writeDir(name); err == nil {
		path = filepath.Join(dir, name)
	}
	_, err := os.Stat(path)
	return err == nil
}

// Write は一時の名前で書き、0400 にしてから改名する（途中で止まっても半端なファイルを残さない）。
// ホストの ft（root）が書く時は所有者を 10001 にする（第12章 10）。
// kek と dns_token は Etc に書く（Etc が空なら誤り。コンテナから /etc に書かない。第12章 6.2）。他は Data に書く。
func (s *Store) Write(name string, b []byte) (err error) {
	dir, err := s.writeDir(name)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	closed := false
	defer func() {
		if err != nil {
			if !closed {
				_ = f.Close()
			}
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(fileMode); err != nil {
		return err
	}
	if os.Getuid() == 0 {
		if err = f.Chown(containerUID, containerUID); err != nil {
			return err
		}
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	closed = true
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Zero は使い終えた秘密をゼロで埋める。完全な消去は言語の保証の外（第12章 H-12-12）。
func Zero(b []byte) {
	clear(b)
	runtime.KeepAlive(b)
}
