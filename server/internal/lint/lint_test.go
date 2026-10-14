// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

package lint

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// base は試験の材料の package の頭（本システムの module の internal の下）。
const base = "github.com/nrsd-ishikawasou/family-talk-pwa/server/internal"

// R: R-11-9-1
func TestAnalyzersReportExactlyTheMarkedCalls(t *testing.T) {
	dir := analysistest.TestData()
	analysistest.Run(t, dir, NoTimeNow, base+"/timenow", base+"/common/clock")
	analysistest.Run(t, dir, LabelConst, base+"/labelconst")
	analysistest.Run(t, dir, LogMsgConst, base+"/logmsg")
	analysistest.Run(t, dir, NoMathRand, base+"/mathrand", base+"/b")
}
