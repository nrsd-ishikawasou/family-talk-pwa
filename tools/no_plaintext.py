# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""平文の走査（全体実装設計書 6.3、6.4。試験設計書 T-L1-16）。

試験のデータの本文と添付に埋めた固有の語が、渡した置き場（DB のダンプ、/data/media、ログ、バックアップから戻した物）に
現れないことを確かめる。語の一覧は Git の外（tools/plaintext_markers.txt。1 行 1 語）か、環境変数 FT_PLAINTEXT_MARKERS
（改行区切り。CI の secret）。見つけた時も語そのものは出さず、語の番号と置き場と位置だけを出す。

使い方：
  python -m tools.no_plaintext [--markers PATH] PATH...     置き場を走査する（ディレクトリは下を全部）
  python -m tools.no_plaintext --generate N [--markers PATH] 乱数の語を N 個作って一覧に書く（無い時）
"""
import argparse
import os
import pathlib
import secrets
import string
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
DEFAULT_MARKERS = ROOT / 'tools' / 'plaintext_markers.txt'
ENV = 'FT_PLAINTEXT_MARKERS'
MARKER_LEN = 12
CHUNK = 1 << 20


def load_markers(path):
    """語の一覧。環境変数が先、無ければファイル。空の行と短すぎる語は誤り（偶然の一致を避ける）。"""
    text = os.environ.get(ENV)
    if text is None:
        if not path.exists():
            raise SystemExit(f'語の一覧が無い：{path}（--generate で作るか、{ENV} を渡す）')
        text = path.read_text(encoding='utf-8')
    markers = [m.strip() for m in text.splitlines() if m.strip()]
    if not markers:
        raise SystemExit('語の一覧が空')
    short = [i for i, m in enumerate(markers, 1) if len(m) < MARKER_LEN]
    if short:
        raise SystemExit(f'{MARKER_LEN} 文字より短い語がある（{short} 番目）')
    return [m.encode('utf-8') for m in markers]


def generate(path, n):
    """英小文字と数字の 12 文字の語を n 個作る（約 62 ビット。偶然の一致は無い）。既にあれば作らない。"""
    if path.exists():
        return False
    alphabet = string.ascii_lowercase + string.digits
    words = [''.join(secrets.choice(alphabet) for _ in range(MARKER_LEN)) for _ in range(n)]
    path.write_text('\n'.join(words) + '\n', encoding='utf-8')
    path.chmod(0o600)
    return True


def files_under(paths):
    for p in paths:
        p = pathlib.Path(p)
        if p.is_dir():
            yield from (f for f in sorted(p.rglob('*')) if f.is_file())
        elif p.is_file():
            yield p
        else:
            raise SystemExit(f'置き場が無い：{p}')


def scan_file(path, markers):
    """ファイルを区切りながら読み、語が現れた所（語の番号、位置）を返す。区切りの境目は語の長さ − 1 だけ重ねて読み、
    同じ位置は 1 回だけ数える。"""
    found = set()
    keep = max(len(m) for m in markers) - 1
    with open(path, 'rb') as f:
        base, buf = 0, b''
        while True:
            chunk = f.read(CHUNK)
            if not chunk:
                break
            buf += chunk
            for i, m in enumerate(markers, 1):
                start = 0
                while (k := buf.find(m, start)) != -1:
                    found.add((i, base + k))
                    start = k + 1
            cut = max(0, len(buf) - keep)
            base += cut
            buf = buf[cut:]
    return sorted(found)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('paths', nargs='*', help='走査する置き場（ファイルかディレクトリ）')
    ap.add_argument('--markers', default=str(DEFAULT_MARKERS), help='語の一覧（1 行 1 語）')
    ap.add_argument('--generate', type=int, help='乱数の語を N 個作る（一覧が無い時）')
    a = ap.parse_args(argv)
    path = pathlib.Path(a.markers)
    if a.generate:
        made = generate(path, a.generate)
        print(f'語の一覧を{"作った" if made else "作らなかった（既にある）"}：{path}')
        if not a.paths:
            return 0
    markers = load_markers(path)
    hits, n = 0, 0
    marker_file = path.resolve()
    for f in files_under(a.paths):
        if f.resolve() == marker_file:
            continue  # 語の一覧そのものは走査しない
        n += 1
        for i, pos in scan_file(f, markers):
            hits += 1
            print(f'見つけた：{f}（語 {i} 番、位置 {pos}）')
    print(f'語 {len(markers)}、ファイル {n}、見つけた {hits}')
    return 1 if hits else 0


if __name__ == '__main__':
    sys.exit(main())
