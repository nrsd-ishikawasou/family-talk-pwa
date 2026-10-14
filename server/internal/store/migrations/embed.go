// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package migrations は DB の移行の SQL を持つ（全体実装設計書 4.5）。ファイルは持ち主のブロックが書き、
// NNN_<ブロック>_<名>.sql の名で並べる。tern の up だけ（ダウングレード非対応。第1章 8.3）。
package migrations

import "embed"

// FS は移行の SQL の全部。
//
//go:embed *.sql
var FS embed.FS
