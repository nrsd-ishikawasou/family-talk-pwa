// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/tern/v2/migrate"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/store/migrations"
)

// versionTable は tern の版の表の名（第1章 8.3）。
const versionTable = "schema_migrations"

// migrationName は移行のファイルの名（NNN_<ブロック>_<名>.sql。tern は \d+_.+\.sql を移行として読む）。
var migrationName = regexp.MustCompile(`^(\d+)_.+\.sql$`)

// Migration は 1 つの移行のファイル。
type Migration struct {
	Version int
	Name    string
}

// MigrationReport は移行の結果。
type MigrationReport struct {
	From    int
	To      int
	Applied []string
	Failed  string
}

// Migrator は DB の移行（B-021。tern。1 つずつトランザクション、advisory lock の中）。
type Migrator struct {
	pool *Pool
	fsys fs.FS
}

// NewMigrator は埋め込みの移行の Migrator を作る。
func NewMigrator(pool *Pool) *Migrator {
	return &Migrator{pool: pool, fsys: migrations.FS}
}

// embedded は埋め込みの移行を版の順に返す。
func (m *Migrator) embedded() ([]Migration, error) {
	entries, err := fs.ReadDir(m.fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		sm := migrationName.FindStringSubmatch(e.Name())
		if e.IsDir() || sm == nil {
			continue
		}
		v, err := strconv.Atoi(sm[1])
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, mg := range out {
		if mg.Version != i+1 {
			return nil, fmt.Errorf("store: 移行の番号が 1 から続いていない: %s", mg.Name)
		}
	}
	return out, nil
}

// withMigrator は専用の接続で tern の Migrator を作って fn を呼ぶ。
func (m *Migrator) withMigrator(ctx context.Context, fn func(*migrate.Migrator) error) error {
	conn, err := m.pool.connect(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer conn.Close(ctx)
	tm, err := migrate.NewMigrator(ctx, conn, versionTable)
	if err != nil {
		return fault.Wrap(err)
	}
	if err := tm.LoadMigrations(m.fsys); err != nil {
		return fault.Wrap(err)
	}
	return fn(tm)
}

// Current は今の版（schema_migrations）。
func (m *Migrator) Current(ctx context.Context) (int, error) {
	var v int32
	err := m.withMigrator(ctx, func(tm *migrate.Migrator) error {
		var err error
		v, err = tm.GetCurrentVersion(ctx)
		return err
	})
	return int(v), err
}

// Pending はまだ当てていない移行。
func (m *Migrator) Pending(ctx context.Context) ([]Migration, error) {
	cur, err := m.Current(ctx)
	if err != nil {
		return nil, err
	}
	all, err := m.embedded()
	if err != nil {
		return nil, fault.Wrap(err)
	}
	var out []Migration
	for _, mg := range all {
		if mg.Version > cur {
			out = append(out, mg)
		}
	}
	return out, nil
}

// Apply は全部を当てる。失敗は Failed に名を入れて誤りを返す（srv-admin が読み取り専用にする。第1章 7.1 の 3）。
func (m *Migrator) Apply(ctx context.Context) (MigrationReport, error) {
	all, err := m.embedded()
	if err != nil {
		return MigrationReport{}, fault.Wrap(err)
	}
	var rep MigrationReport
	err = m.withMigrator(ctx, func(tm *migrate.Migrator) error {
		from, err := tm.GetCurrentVersion(ctx)
		if err != nil {
			return err
		}
		rep.From = int(from)
		migErr := tm.Migrate(ctx)
		to, err := tm.GetCurrentVersion(ctx)
		if err != nil {
			return err
		}
		rep.To = int(to)
		for _, mg := range all {
			if mg.Version > rep.From && mg.Version <= rep.To {
				rep.Applied = append(rep.Applied, mg.Name)
			}
			if migErr != nil && mg.Version == rep.To+1 {
				rep.Failed = mg.Name
			}
		}
		return migErr
	})
	if err != nil {
		return rep, fault.Wrap(err)
	}
	return rep, nil
}

// errDowngrade は記録の版がイメージより新しいこと。
var errDowngrade = errors.New("store: 記録の版がイメージより新しい（ダウングレードは支えない）")

// RequireVersion は記録の版がイメージの持つ最後の版より新しくないことを確かめる（新しければ起動しない）。
func (m *Migrator) RequireVersion(ctx context.Context) error {
	all, err := m.embedded()
	if err != nil {
		return fault.Wrap(err)
	}
	cur, err := m.Current(ctx)
	if err != nil {
		return err
	}
	if len(all) > 0 && cur > all[len(all)-1].Version {
		return errDowngrade
	}
	return nil
}
