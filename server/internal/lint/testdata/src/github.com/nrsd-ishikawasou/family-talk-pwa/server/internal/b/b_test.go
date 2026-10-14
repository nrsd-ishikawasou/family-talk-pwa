// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package b

import (
	"math/rand"
	"testing"
)

// 試験のファイルの math/rand は止めない。
func TestX(t *testing.T) { _ = rand.Int() }
