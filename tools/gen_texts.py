# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""エラーの文言の表を作る（基本設計書 第10章 9.2 の表が正本）。

第10章 9.2 の表（番号、1 文目、2 文目）から web/src/i18n/ja/errors.json を作る。鍵は err.<番号>（エラーコード表の
user_text_key）、値は {"what": 1 文目, "next": 2 文目}。生成した物はコミットし、CI は --check で差分 0 を確かめる。
エラーコード表の番号と表の番号が 1 対 1 でなければ誤り。

使い方：python -m tools.gen_texts [--check]
"""
import argparse
import json
import pathlib
import re
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
CHAPTER = ROOT / 'docs' / 'jp' / 'design' / '10_基本設計書_画面.md'
ERRORS = ROOT / 'api' / 'errors.yaml'
OUT = ROOT / 'web' / 'src' / 'i18n' / 'ja' / 'errors.json'
ROW = re.compile(r'^\|([A-Z]+-\d{3})\|([^|]+)\|([^|]+)\|$')


def table(text):
    """第10章 9.2 の節の表の行（番号、1 文目、2 文目）。"""
    start = text.index('### 9.2 ')
    end = text.index('\n### ', start + 1)
    rows = []
    for line in text[start:end].splitlines():
        m = ROW.match(line.strip())
        if m:
            rows.append((m.group(1), m.group(2).strip(), m.group(3).strip()))
    return rows


def build(chapter_text, errors_rows):
    rows = table(chapter_text)
    codes = [r[0] for r in rows]
    if len(codes) != len(set(codes)):
        raise ValueError('第10章 9.2 の表に同じ番号が 2 つある')
    want = {r['code']: r['user_text_key'] for r in errors_rows}
    missing, extra = sorted(set(want) - set(codes)), sorted(set(codes) - set(want))
    if missing or extra:
        raise ValueError(f'エラーコード表と第10章 9.2 の表が合わない（無い {missing}、余る {extra}）')
    out = {want[c]: {'what': what, 'next': nxt} for c, what, nxt in rows}
    return json.dumps(dict(sorted(out.items())), ensure_ascii=False, indent=2) + '\n'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--check', action='store_true', help='作り直した結果とコミットした物の差分が 0 かを見る')
    a = ap.parse_args(argv)
    text = build(CHAPTER.read_text(encoding='utf-8'), yaml.safe_load(ERRORS.read_text(encoding='utf-8'))['rows'])
    if a.check:
        if not OUT.exists() or OUT.read_text(encoding='utf-8') != text:
            print(f'{OUT.relative_to(ROOT)} が第10章 9.2 の表と違う（python -m tools.gen_texts で作り直す）')
            return 1
        print(f'{OUT.relative_to(ROOT)} は第10章 9.2 の表と同じ')
        return 0
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(text, encoding='utf-8')
    print(f'書いた：{OUT.relative_to(ROOT)}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
