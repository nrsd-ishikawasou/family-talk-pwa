// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package store は記録の読み書きの器（srv-store。B-020〜B-029）。
package store

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/fault"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/gen"
	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/secret"
)

// 接続先（第11章 3.1 の postgres の行。第12章 4 のコンテナの間の行）。
const (
	DbHost = "postgres"
	DbPort = 5432
	DbName = "family_talk"
	DbUser = "family_talk"
)

// 接続の決まった値。
const (
	connectTimeout    = 5 * time.Second
	healthCheckPeriod = 30 * time.Second
	maxConnsCap       = 16
)

// ConnInfo は接続先。本番は DefaultConnInfo、試験は testcontainers の接続先。
type ConnInfo struct {
	Host     string
	Port     uint16
	Database string
	User     string
	Password []byte
}

// DefaultConnInfo は第11章 3.1 の接続先（password は Secrets.DbPassword）。
func DefaultConnInfo(password []byte) ConnInfo {
	return ConnInfo{Host: DbHost, Port: DbPort, Database: DbName, User: DbUser, Password: password}
}

// pgxConfig は接続の設定を作る。TLS は使わない（第12章 4）。
func pgxConfig(info ConnInfo) (*pgx.ConnConfig, error) {
	cc, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cc.Host = info.Host
	cc.Port = info.Port
	cc.Database = info.Database
	cc.User = info.User
	cc.Password = string(info.Password)
	cc.TLSConfig = nil
	cc.Fallbacks = nil
	cc.ConnectTimeout = connectTimeout
	return cc, nil
}

// Pool は postgres への接続の pool（B-020）。
type Pool struct {
	pool *pgxpool.Pool
	info ConnInfo
}

// Open は接続の pool を作り、Ping で確かめる。接続の数は CPU×2（上限 16）。届かなければ SRV-004。
func Open(ctx context.Context, info ConnInfo) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fault.Wrap(err)
	}
	cc, err := pgxConfig(info)
	if err != nil {
		return nil, fault.Wrap(err)
	}
	cfg.ConnConfig = cc
	cfg.MaxConns = int32(min(2*runtime.NumCPU(), maxConnsCap))
	cfg.MinConns = 1
	cfg.HealthCheckPeriod = healthCheckPeriod
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, dbUnavailable()
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, dbUnavailable()
	}
	keep := info
	keep.Password = append([]byte(nil), info.Password...)
	return &Pool{pool: p, info: keep}, nil
}

// Begin はトランザクションを始める。呼んだ側は必ず Commit か Rollback を呼ぶ。
func (p *Pool) Begin(ctx context.Context) (*Tx, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	return &Tx{tx: tx}, nil
}

// Query はトランザクションの外の読み出し。
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

// QueryRow はトランザクションの外の 1 行の読み出し。
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

// Exec はトランザクションの外の書き込み。
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.pool.Exec(ctx, sql, args...)
}

// Ping は /health の確かめ。届かなければ SRV-004。
func (p *Pool) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return dbUnavailable()
	}
	return nil
}

// Close は pool を閉じ、持っていたパスワードを Zero する。
func (p *Pool) Close() {
	p.pool.Close()
	secret.Zero(p.info.Password)
}

// connect は pool と別の専用の接続を作る（LISTEN と移行）。
func (p *Pool) connect(ctx context.Context) (*pgx.Conn, error) {
	cc, err := pgxConfig(p.info)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cc)
}

// unavailable は DB に届かない誤りを SRV-004 に、他は Fault にする。
func unavailable(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fault.Wrap(err)
	}
	return dbUnavailable()
}

// Tx は 1 つのトランザクション。コミットの後に走る付け足し（AfterCommit）を持つ。
type Tx struct {
	tx    pgx.Tx
	after []func()
	done  bool
}

// Exec はトランザクションの中の書き込み。
func (t *Tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

// Query はトランザクションの中の読み出し。
func (t *Tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.tx.Query(ctx, sql, args...)
}

// QueryRow はトランザクションの中の 1 行の読み出し。
func (t *Tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

// Commit はコミットし、成功したら AfterCommit の関数を登録の順に呼ぶ。
func (t *Tx) Commit(ctx context.Context) error {
	if err := t.tx.Commit(ctx); err != nil {
		t.done = true
		t.after = nil
		return unavailable(err)
	}
	t.done = true
	after := t.after
	t.after = nil
	for _, fn := range after {
		fn()
	}
	return nil
}

// Rollback は取り消す（コミット済みなら何もしない。defer で呼んでよい）。AfterCommit の関数は捨てる。
func (t *Tx) Rollback(ctx context.Context) {
	if t.done {
		return
	}
	t.done = true
	t.after = nil
	_ = t.tx.Rollback(ctx)
}

// AfterCommit はコミットの後に走る付け足しを登録する（プロセスの中の channel への投入。NOTIFY には使わない）。
func (t *Tx) AfterCommit(fn func()) {
	t.after = append(t.after, fn)
}

type txKey struct{}

// WithTx は外側で開いたトランザクションを ctx に入れる（srv-boundary の冪等の middleware。第2章 11）。
func WithTx(ctx context.Context, tx *Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFrom は ctx のトランザクションを返す。無ければ nil。
func TxFrom(ctx context.Context) *Tx {
	tx, _ := ctx.Value(txKey{}).(*Tx)
	return tx
}

// InTx は fn をトランザクションの中で走らせる。ctx に外側のトランザクションがあればそれを使い、
// コミットと rollback は外側に任せる（冪等の記録と操作を 1 つのトランザクションにするため）。
// 無ければ自分で開き、fn が nil を返せばコミット、誤りなら rollback する。
func (p *Pool) InTx(ctx context.Context, fn func(tx *Tx) error) error {
	if tx := TxFrom(ctx); tx != nil {
		return fn(tx)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// dbUnavailable は DB に届かない時の SRV-004（params.service は postgres。第2章 9.2）。
func dbUnavailable() error {
	return fault.New(gen.SRV004, map[string]any{"service": "postgres"})
}
