# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""他社の名称と痕跡の一覧（Git の外）で、リポジトリの追跡しているファイルを走査する（基本設計書 第14章 14 の 1、試験設計書 T-L5-08）。

一覧：tools/forbidden_words.txt（1 行 1 語。# から行末は注記。Git の外。CI は secret から書き出す）。
見つけた語そのものは出さない（CI の記録は公開されるため）。場所と一覧の何番目かだけを出す。
使い方：python -m tools.forbidden_words [--list PATH]
終了コード：0 無し、1 見つかった、2 一覧が無い。
"""
import argparse
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
DEFAULT_LIST = ROOT / 'tools' / 'forbidden_words.txt'
SKIP = {'tools/forbidden_words.txt', 'tools/plaintext_markers.txt'}


def load(path):
    words = []
    for line in path.read_text(encoding='utf-8').split('\n'):
        w = line.split('#', 1)[0].strip()
        if w:
            words.append(w)
    return words


def pattern(word):
    # 英数字の語は語の境で、大小を区別せずに探す。それ以外（日本語）は部分一致
    if re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9 .&\'-]*', word):
        return re.compile(r'(?<![A-Za-z0-9])' + re.escape(word) + r'(?![A-Za-z0-9])', re.IGNORECASE)
    return re.compile(re.escape(word))


def tracked_files():
    out = subprocess.run(['git', 'ls-files', '-z'], cwd=ROOT, capture_output=True, check=True).stdout
    return [p for p in out.decode('utf-8').split('\0') if p and p not in SKIP]


def scan(words, files):
    pats = [pattern(w) for w in words]
    hits = []
    for rel in files:
        path = ROOT / rel
        try:
            text = path.read_text(encoding='utf-8')
        except (UnicodeDecodeError, OSError):
            continue   # 画像その他のバイナリ
        for n, line in enumerate(text.split('\n'), 1):
            for idx, p in enumerate(pats, 1):
                if p.search(line):
                    hits.append((rel, n, idx))
    return hits


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--list', type=pathlib.Path, default=DEFAULT_LIST, help='一覧のファイル')
    a = ap.parse_args(argv)
    if not a.list.exists():
        print(f'一覧が無い：{a.list}')
        return 2
    words = load(a.list)
    if not words:
        print('一覧が空')
        return 2
    hits = scan(words, tracked_files())
    for rel, n, idx in hits:
        print(f'{rel}:{n}: 一覧の {idx} 番目の語')
    print(f'語 {len(words)}、見つかった所 {len(hits)}')
    return 1 if hits else 0


if __name__ == '__main__':
    sys.exit(main())
