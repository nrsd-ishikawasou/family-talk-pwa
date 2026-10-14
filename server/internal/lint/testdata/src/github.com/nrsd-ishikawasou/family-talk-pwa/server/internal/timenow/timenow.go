// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package timenow は試験の材料（notimenow）。
package timenow

import "time"

func f() {
	_ = time.Now()              // want "time.Now を直接呼ばない"
	_ = time.Since(time.Time{}) // want "time.Since を直接呼ばない"
	_ = time.Until(time.Time{}) // want "time.Until を直接呼ばない"
	_ = time.Unix(0, 0)
}
