// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package labelconst は試験の材料（labelconst）。
package labelconst

import "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"

const kind = "backup"

func f(s string) {
	_ = logx.LabelName(kind)
	_ = logx.LabelName("restore")
	_ = logx.LabelName(s) // want "logx.LabelName に定数でない値を変換しない"
}
