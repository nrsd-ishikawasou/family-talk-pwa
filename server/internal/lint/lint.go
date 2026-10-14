// SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
// SPDX-License-Identifier: Apache-2.0

// Package lint は自前の検査（ci-tools の ftlint）。go/analysis の Analyzer で、
// srv-common の約束（時刻は Deps.Clock だけ、ログは定数の msg と Redact の値だけ、乱数は crypto/rand）を守らせる。
package lint

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// 本システムの package の道のり。
const (
	modulePath  = "github.com/nrsd-ishikawasou/family-talk-pwa/server"
	clockPkg    = modulePath + "/internal/common/clock"
	logxPkg     = modulePath + "/internal/common/logx"
	labelName   = "LabelName"
	loggerName  = "Logger"
	timePkgPath = "time"
)

// Analyzers は ftlint が束ねる全部。
var Analyzers = []*analysis.Analyzer{NoTimeNow, LabelConst, LogMsgConst, NoMathRand}

// isTest は試験のファイルか。
func isTest(pass *analysis.Pass, n ast.Node) bool {
	return strings.HasSuffix(pass.Fset.Position(n.Pos()).Filename, "_test.go")
}

// NoTimeNow は time.Now、time.Since、time.Until の直接の呼び出しを止める（srv-common B-003。clock の中を除く）。
var NoTimeNow = &analysis.Analyzer{
	Name:     "notimenow",
	Doc:      "時刻は Deps.Clock から読む。time.Now、time.Since、time.Until を直接呼ばない（clock の中を除く）",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run: func(pass *analysis.Pass) (any, error) {
		if pass.Pkg.Path() == clockPkg {
			return nil, nil
		}
		ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
		ins.Preorder([]ast.Node{(*ast.SelectorExpr)(nil)}, func(n ast.Node) {
			sel := n.(*ast.SelectorExpr)
			fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != timePkgPath {
				return
			}
			switch fn.Name() {
			case "Now", "Since", "Until":
				pass.Reportf(sel.Pos(), "time.%s を直接呼ばない。Deps.Clock の Now を使う", fn.Name())
			}
		})
		return nil, nil
	},
}

// isNamed は t が pkgPath の name の型か。
func isNamed(t types.Type, pkgPath, name string) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkgPath && n.Obj().Name() == name
}

// LabelConst は logx.LabelName への定数でない値の変換を止める（第12章 11。変数の文字列をログに出さない）。
var LabelConst = &analysis.Analyzer{
	Name:     "labelconst",
	Doc:      "logx.LabelName には定数だけを渡す",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run: func(pass *analysis.Pass) (any, error) {
		ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
		ins.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
			call := n.(*ast.CallExpr)
			tv, ok := pass.TypesInfo.Types[call.Fun]
			if !ok || !tv.IsType() || !isNamed(tv.Type, logxPkg, labelName) || len(call.Args) != 1 {
				return
			}
			if arg := pass.TypesInfo.Types[call.Args[0]]; arg.Value == nil {
				pass.Reportf(call.Pos(), "logx.LabelName に定数でない値を変換しない")
			}
		})
		return nil, nil
	},
}

// LogMsgConst は logx の記録（Debug、Info、Warn、Error）の msg に定数でない文字列を渡すことを止める。
var LogMsgConst = &analysis.Analyzer{
	Name:     "logmsgconst",
	Doc:      "logx の記録の msg は定数の文字列だけ",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run: func(pass *analysis.Pass) (any, error) {
		ins := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
		ins.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
			call := n.(*ast.CallExpr)
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || len(call.Args) == 0 {
				return
			}
			switch sel.Sel.Name {
			case "Debug", "Info", "Warn", "Error":
			default:
				return
			}
			s, ok := pass.TypesInfo.Selections[sel]
			if !ok || !isNamed(s.Recv(), logxPkg, loggerName) {
				return
			}
			if arg := pass.TypesInfo.Types[call.Args[0]]; arg.Value == nil {
				pass.Reportf(call.Args[0].Pos(), "logx の msg に定数でない文字列を渡さない（値は Field と Redact で）")
			}
		})
		return nil, nil
	},
}

// NoMathRand は math/rand と math/rand/v2 の import を止める（試験を除く。乱数は Deps.Rand）。
var NoMathRand = &analysis.Analyzer{
	Name: "nomathrand",
	Doc:  "乱数は Deps.Rand（crypto/rand）。math/rand を import しない（試験を除く）",
	Run: func(pass *analysis.Pass) (any, error) {
		for _, f := range pass.Files {
			if isTest(pass, f) {
				continue
			}
			for _, imp := range f.Imports {
				switch strings.Trim(imp.Path.Value, `"`) {
				case "math/rand", "math/rand/v2":
					pass.Reportf(imp.Pos(), "math/rand を import しない。乱数は Deps.Rand")
				}
			}
		}
		return nil, nil
	},
}
