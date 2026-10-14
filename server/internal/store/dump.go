// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"strconv"
	"strings"
)

// Cmd は外の命令（引数と、足す環境変数）。
type Cmd struct {
	Args []string
	Env  []string
}

// Dump は DB の書き出しと戻しの命令（B-028。第11章 9.1、9.2）。
type Dump struct{}

// connArgs は libpq の道具の接続の引数（パスワードは passfile。--no-password で尋ねない）。
func connArgs() []string {
	return []string{"--host", DbHost, "--port", strconv.Itoa(DbPort), "--username", DbUser, "--no-password"}
}

// passEnv は PGPASSFILE の環境変数（PostgreSQL の文書は PGPASSWORD を勧めない）。
func passEnv(passfile string) []string {
	return []string{"PGPASSFILE=" + passfile}
}

// pgpassEscape はパスワードのファイルの欄の `:` と `\` を逃がす（PostgreSQL の文書の形）。
func pgpassEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(s)
}

// WritePassFile は dir（tmpfs の /tmp）に 0600 のパスワードのファイルを作る。cleanup で消す。
func (Dump) WritePassFile(dir string, password []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp(dir, ".pgpass-*")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	line := DbHost + ":" + strconv.Itoa(DbPort) + ":*:" + DbUser + ":" + pgpassEscape(string(password)) + "\n"
	if err = f.Chmod(0o600); err == nil {
		_, err = f.WriteString(line)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

// DumpCommands は pg_dumpall --globals-only、pg_dump -Fc の順の 2 つの命令。passfile は 0600 のパスワードのファイル。
func (Dump) DumpCommands(passfile string) []Cmd {
	globals := append([]string{"pg_dumpall", "--globals-only"}, connArgs()...)
	dump := append(append([]string{"pg_dump", "--format=custom"}, connArgs()...), "--dbname", DbName)
	return []Cmd{{Args: globals, Env: passEnv(passfile)}, {Args: dump, Env: passEnv(passfile)}}
}

// RestoreCommands は空の postgres に globals、dump の順で戻す 2 つの命令（第11章 9.2 の 3）。
func (Dump) RestoreCommands(passfile, globalsFile, dumpFile string) []Cmd {
	globals := append(append([]string{"psql"}, connArgs()...), "--dbname", "postgres", "--set", "ON_ERROR_STOP=1", "--file", globalsFile)
	dump := append(append([]string{"pg_restore"}, connArgs()...), "--dbname", DbName, "--exit-on-error", dumpFile)
	return []Cmd{{Args: globals, Env: passEnv(passfile)}, {Args: dump, Env: passEnv(passfile)}}
}
