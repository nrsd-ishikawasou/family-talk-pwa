# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""試験のコードの要件の注記と、試験設計書 第2章（要件と試験の対応）の表を突き合わせる（全体実装設計書 6.3）。

注記の形：Python は `# R: R-2-8-1`、Go は `// R: R-2-8-1`、TypeScript は `/* R: R-2-8-1 */`、Kotlin は `@Requirement("R-2-8-1")`。
赤にするもの：表に無い要件を引く注記、満たし方（試験の番号 T-、または「文書（…）」「NRSD」）の無い表の行。
使い方：python -m tools.trace_requirements
終了コード：0 一致、1 食い違いあり、2 表が読めない。
"""
import argparse
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
TABLE = ROOT / 'docs' / 'jp' / 'test' / '02_試験設計書_要件と試験の対応.md'
NOTE = re.compile(r'(?:#|//|/\*)\s*R:\s*((?:R-\d+-\d+-\d+[、,\s]*)+)|@Requirement\("(R-\d+-\d+-\d+)"\)')
CODE_SUFFIX = ('.py', '.go', '.ts', '.tsx', '.kt', '.kts')


def table_rows():
    rows = {}
    for line in TABLE.read_text(encoding='utf-8').split('\n'):
        m = re.match(r'^\|(R-\d+-\d+-\d+)\|', line)
        if m:
            cells = [c.strip() for c in line.strip('|').split('|')]
            how = cells[-1]
            # 満たし方：試験の番号、または文書（設計書の節）か NRSD の判断（表が明示する）
            tests = re.findall(r'T-[A-Z0-9]+(?:-[A-Z0-9]+)*', how)
            rows[m.group(1)] = tests if tests else (['文書'] if how.startswith(('文書', 'NRSD')) else [])
    return rows


def notes():
    out = subprocess.run(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'], cwd=ROOT,
                         capture_output=True, check=True).stdout.decode('utf-8').split('\0')
    found = {}
    for rel in out:
        if not rel.endswith(CODE_SUFFIX) or rel.startswith('tools/node/'):
            continue
        text = (ROOT / rel).read_text(encoding='utf-8')
        for n, line in enumerate(text.split('\n'), 1):
            for m in NOTE.finditer(line):
                for r in re.findall(r'R-\d+-\d+-\d+', m.group(1) or m.group(2)):
                    found.setdefault(r, []).append(f'{rel}:{n}')
    return found


def main(argv=None):
    argparse.ArgumentParser(description=__doc__.split('\n')[0]).parse_args(argv)
    try:
        rows = table_rows()
    except OSError as e:
        print(f'表が読めない：{e}')
        return 2
    if not rows:
        print('表に要件の行が無い')
        return 2
    found = notes()
    bad = []
    for r, places in sorted(found.items()):
        if r not in rows:
            bad.append(f'表に無い要件を引く注記 {r}（{"、".join(places)}）')
    for r, tests in sorted(rows.items()):
        if not tests:
            bad.append(f'試験の番号の無い表の行 {r}')
    covered = len(set(found) & set(rows))
    print(f'表の要件 {len(rows)}、注記のある要件 {covered}、注記 {sum(len(v) for v in found.values())}')
    for b in bad:
        print('NG ' + b)
    return 1 if bad else 0


if __name__ == '__main__':
    sys.exit(main())
