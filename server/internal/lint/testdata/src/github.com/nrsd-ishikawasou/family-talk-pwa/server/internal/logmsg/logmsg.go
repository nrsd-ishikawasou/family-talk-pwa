// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package logmsg は試験の材料（logmsgconst）。
package logmsg

import "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal/common/logx"

func f(l *logx.Logger, s string) {
	l.Info("定数の msg")
	l.Error("a" + "b")
	l.Warn(s)        // want "logx の msg に定数でない文字列を渡さない"
	l.Debug(s + "x") // want "logx の msg に定数でない文字列を渡さない"
}
