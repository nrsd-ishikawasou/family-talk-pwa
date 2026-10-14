// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
)

// 置き場の決まった値。
const (
	partsDir   = "parts"
	partSuffix = ".part"
	dirMode    = 0o700
	fileMode   = 0o600
)

// FileStore は添付の暗号文の置き場（B-025。/data/media/<2 文字>/<sha256>）。
type FileStore struct {
	Root string
}

// NewFileStore は root（/data/media）の FileStore を作る。
func NewFileStore(root string) *FileStore {
	return &FileStore{Root: root}
}

// PathFor は sha256 の置き場の道のり（先頭 2 文字のディレクトリ）。
func (s *FileStore) PathFor(sum [32]byte) string {
	h := hex.EncodeToString(sum[:])
	return filepath.Join(s.Root, h[:2], h)
}

// partPath は id の .part の道のり。
func (s *FileStore) partPath(id uuid.UUID) string {
	return filepath.Join(s.Root, partsDir, id.String()+partSuffix)
}

// syncDir はディレクトリを fsync する（改名と削除を確かにする。第1章 10.2）。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// OpenPart は /data/media/parts/<id>.part を追記で開く（無ければ作る）。
func (s *FileStore) OpenPart(id uuid.UUID) (*PartWriter, error) {
	if err := os.MkdirAll(filepath.Join(s.Root, partsDir), dirMode); err != nil {
		return nil, fault.Wrap(err)
	}
	f, err := os.OpenFile(s.partPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return nil, fault.Wrap(err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fault.Wrap(err)
	}
	return &PartWriter{f: f, offset: st.Size()}, nil
}

// Finalize はハッシュの照合、fsync、改名、ディレクトリの fsync の順で完了にする。違えば .part を消して MEDIA-004。
// 同じ中身の実体が既にあれば .part を消して成功。
func (s *FileStore) Finalize(id uuid.UUID, sum [32]byte) error {
	part := s.partPath(id)
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fault.New(gen.MEDIA002, nil)
		}
		return fault.Wrap(err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return fault.Wrap(err)
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != sum {
		_ = f.Close()
		_ = os.Remove(part)
		return fault.New(gen.MEDIA004, nil)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fault.Wrap(err)
	}
	if err := f.Close(); err != nil {
		return fault.Wrap(err)
	}
	dest := s.PathFor(sum)
	if _, err := os.Stat(dest); err == nil {
		_ = os.Remove(part)
		return syncDir(filepath.Dir(part))
	}
	if err := os.MkdirAll(filepath.Dir(dest), dirMode); err != nil {
		return fault.Wrap(err)
	}
	if err := os.Rename(part, dest); err != nil {
		return fault.Wrap(err)
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return fault.Wrap(err)
	}
	if err := syncDir(filepath.Dir(part)); err != nil {
		return fault.Wrap(err)
	}
	return nil
}

// Open は Range の配信のために開く。無ければ MEDIA-006。
func (s *FileStore) Open(sum [32]byte) (*os.File, error) {
	f, err := os.Open(s.PathFor(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fault.New(gen.MEDIA006, nil)
	}
	if err != nil {
		return nil, fault.Wrap(err)
	}
	return f, nil
}

// Exists は実体があるか。
func (s *FileStore) Exists(sum [32]byte) bool {
	_, err := os.Stat(s.PathFor(sum))
	return err == nil
}

// Remove は実体を消す（GC。参照が消えて猶予の後）。無いのは成功。
func (s *FileStore) Remove(sum [32]byte) error {
	p := s.PathFor(sum)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fault.Wrap(err)
	}
	if err := syncDir(filepath.Dir(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fault.Wrap(err)
	}
	return nil
}

// SweepParts は olderThan より古い .part を消し、消した道のりを返す（期限切れのスロット。24 時間）。
func (s *FileStore) SweepParts(olderThan time.Time) ([]string, error) {
	dir := filepath.Join(s.Root, partsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fault.Wrap(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), partSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(olderThan) {
			p := filepath.Join(dir, e.Name())
			if err := os.Remove(p); err == nil {
				out = append(out, p)
			}
		}
	}
	if len(out) > 0 {
		if err := syncDir(dir); err != nil {
			return out, fault.Wrap(err)
		}
	}
	return out, nil
}

// Usage は置き場の大きさの合計（parts を含む。容量の監視）。
func (s *FileStore) Usage() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.Root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fault.Wrap(err)
	}
	return total, nil
}

// PartWriter は 1 つのスロットの .part への追記。同じスロットへの書き手は 1 つ（srv-media が offset で守る）。
type PartWriter struct {
	f      *os.File
	offset int64
}

// Offset は今の大きさ（再開の位置）。
func (w *PartWriter) Offset() int64 {
	return w.offset
}

// Append は r を maxBytes まで流し書きする（記憶に載せない）。超えれば書いた分を戻して MEDIA-001。
func (w *PartWriter) Append(r io.Reader, maxBytes int64) (int64, error) {
	room := maxBytes - w.offset
	if room < 0 {
		room = 0
	}
	n, err := io.Copy(w.f, io.LimitReader(r, room+1))
	if n > room {
		if terr := w.f.Truncate(w.offset); terr != nil {
			return 0, fault.Wrap(terr)
		}
		return 0, fault.New(gen.MEDIA001, nil)
	}
	if err != nil {
		if terr := w.f.Truncate(w.offset); terr != nil {
			return 0, fault.Wrap(terr)
		}
		return 0, fault.Wrap(err)
	}
	w.offset += n
	return n, nil
}

// Close は閉じる。
func (w *PartWriter) Close() error {
	return w.f.Close()
}
