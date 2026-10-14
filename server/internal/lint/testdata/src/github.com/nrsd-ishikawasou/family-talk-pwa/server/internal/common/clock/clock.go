// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package clock は試験の材料（time.Now を呼んでよい唯一の package）。
package clock

import "time"

func Now() time.Time { return time.Now() }
