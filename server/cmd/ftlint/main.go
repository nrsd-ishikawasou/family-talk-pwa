// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Command ftlint は自前の検査を束ねる（ci-tools）。使い方：go run ./cmd/ftlint ./...
package main

import (
	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/lint"
)

func main() {
	multichecker.Main(lint.Analyzers...)
}
