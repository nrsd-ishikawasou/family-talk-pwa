// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package mathrand は試験の材料（nomathrand）。
package mathrand

import "math/rand" // want "math/rand を import しない"

func f() int { return rand.Int() }
