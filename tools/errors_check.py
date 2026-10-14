# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""エラーコード表と試験と文言の照合（全体実装設計書 6.3、試験設計書 T-L2-05、T-L1-13）。

1. api/errors.yaml の各行が test の欄（TestErr_<番号の - を _ に>）と user_text_key の欄（err.<番号>）を持つ。
2. --tests に挙げた番号（接頭の NET や、番号そのもの SRV-001）の行は、その名の試験が L1 にある。
   L1 は server/ の下の `//go:build integration` の付いた *_test.go。番号ごとの試験はブロックを作る段で足すので、
   照らす範囲は段階ごとに広げる（実装計画の WP ごとの閉じる条件）。
3. --texts に挙げた文言の表（JSON。鍵から文）が、全部の行の user_text_key を持つ。

使い方：python -m tools.errors_check [--tests NET,SRV-001] [--texts web/src/i18n/ja/errors.json]
"""
import argparse
import json
import pathlib
import re
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
ERRORS = ROOT / 'api' / 'errors.yaml'
SERVER = ROOT / 'server'
TEST_FUNC = re.compile(r'^func (Test\w+)\(t \*testing\.T\)', re.M)
BUILD_INTEGRATION = re.compile(r'^//go:build .*\bintegration\b', re.M)


def expected_test(code):
    return 'TestErr_' + code.replace('-', '_')


def l1_tests(root=SERVER):
    """integration の印の付いた試験のファイルにある試験の関数の名と、そのファイル。"""
    out = {}
    for f in sorted(root.rglob('*_test.go')):
        text = f.read_text(encoding='utf-8')
        if not BUILD_INTEGRATION.search(text):
            continue
        for name in TEST_FUNC.findall(text):
            out[name] = f
    return out


def selected(code, patterns):
    """code が --tests の接頭（NET）か番号（SRV-001）に当たるか。"""
    return any(code == p or code.startswith(p + '-') for p in patterns)


def check(rows, patterns, tests, texts):
    problems = []
    for r in rows:
        code = r.get('code', '')
        want = expected_test(code)
        if r.get('test') != want:
            problems.append(f'{code}：test の欄が {want} でない（{r.get("test")!r}）')
        if r.get('user_text_key') != f'err.{code}':
            problems.append(f'{code}：user_text_key の欄が err.{code} でない（{r.get("user_text_key")!r}）')
        if patterns and selected(code, patterns) and want not in tests:
            problems.append(f'{code}：L1 に試験 {want} が無い')
        if texts is not None and r.get('user_text_key') not in texts:
            problems.append(f'{code}：文言の表に {r.get("user_text_key")} が無い')
    unknown = [p for p in patterns if not any(selected(r.get('code', ''), [p]) for r in rows)]
    for p in unknown:
        problems.append(f'--tests の {p} に当たる行が無い')
    return problems


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--tests', default='', help='試験を求める番号の接頭か番号（カンマ区切り）')
    ap.add_argument('--texts', help='文言の表（JSON）')
    a = ap.parse_args(argv)
    rows = yaml.safe_load(ERRORS.read_text(encoding='utf-8'))['rows']
    patterns = [p.strip() for p in a.tests.split(',') if p.strip()]
    texts = None
    if a.texts:
        texts = json.loads((ROOT / a.texts).read_text(encoding='utf-8'))
    problems = check(rows, patterns, l1_tests(), texts)
    for p in problems:
        print(p)
    n = sum(1 for r in rows if patterns and selected(r['code'], patterns))
    print(f'行 {len(rows)}、試験を求めた行 {n}、問題 {len(problems)}')
    return 1 if problems else 0


if __name__ == '__main__':
    sys.exit(main())
