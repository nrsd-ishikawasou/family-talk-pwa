# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""設計書の表から文の生成ファイルを作る（基本設計書 第14章 8、全体実装設計書 5.1）。

1. エラーの文言の表：第10章 9.2 の表（番号、1 文目、2 文目）から web/src/ui/i18n/errors.json を作る。鍵は err.<番号>
   （エラーコード表の user_text_key）、値は {"what": 1 文目, "next": 2 文目}。生成した物はコミットし、CI は --check で
   差分 0 を確かめる。エラーコード表の番号と表の番号が 1 対 1 でなければ誤り。
2. 知らせの文言の表：第10章 9.4 の表（識別子、1 文目、2 文目）から web/src/ui/i18n/notices.json を作る。鍵は
   notice.<識別子>（notice のイベントの text_key）、値は {"what": 1 文目, "next": 2 文目}。第7章 9 の表の識別子と 1 対 1 で
   なければ誤り。
3. 明示の文：第12章 7.1 の引用と 8 の表の行から <置き場>/disclosure.json を作る（--disclosure-out。make gen が
   server/internal/identity/gen に作る。コミットしない）。「（VPS の場合）」で始まる行は mode D の印を付け、行頭の括弧を除く。
   {admin}、{provider}、{card_holders} の埋め込みと版の計算はサーバーが行う（第12章 7.1）。

使い方：
  python -m tools.gen_texts [--check]          エラーと知らせの文言の表を作る（--check は差分を見るだけ）
  python -m tools.gen_texts --disclosure-out DIR 明示の文だけを DIR に作る
"""
import argparse
import json
import pathlib
import re
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
DESIGN = ROOT / 'docs' / 'jp' / 'design'
CHAPTER = DESIGN / '10_基本設計書_画面.md'
DATA_CHAPTER = DESIGN / '12_基本設計書_データと暗号化.md'
NOTICE_CHAPTER = DESIGN / '07_基本設計書_通知.md'
ERRORS = ROOT / 'api' / 'errors.yaml'
OUT = ROOT / 'web' / 'src' / 'ui' / 'i18n' / 'errors.json'
NOTICE_OUT = ROOT / 'web' / 'src' / 'ui' / 'i18n' / 'notices.json'
ROW = re.compile(r'^\|([A-Z]+-\d{3})\|([^|]+)\|([^|]+)\|$')
NOTICE_ROW = re.compile(r'^\|([a-z][a-z0-9_]*)\|([^|]+)\|([^|]*)\|$')
KIND_CELL = re.compile(r'^`([a-z][a-z0-9_]*)`$')
VPS = '（VPS の場合）'


def section(text, head, level='###'):
    """見出し head で始まる節の本文（次の同じ深さか浅い見出しの前まで）。"""
    start = text.index(f'{level} {head}')
    m = re.compile(r'^#{1,%d} ' % len(level), re.M).search(text, start + 1)
    return text[start:m.start() if m else len(text)]


def table(text):
    """第10章 9.2 の節の表の行（番号、1 文目、2 文目）。"""
    rows = []
    for line in section(text, '9.2 ').splitlines():
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


def notice_table(text):
    """第10章 9.4 の節の表の行（識別子、1 文目、2 文目。2 文目は空でもよい）。"""
    rows = []
    for line in section(text, '9.4 ').splitlines():
        m = NOTICE_ROW.match(line.strip())
        if m:
            rows.append((m.group(1), m.group(2).strip(), m.group(3).strip()))
    return rows


def notice_kinds(text):
    """第7章 9 の表の識別子の列（2 列目。`—` の行はトークだけの知らせで識別子を持たない）。"""
    kinds = []
    for line in section(text, '9. ', level='##').splitlines():
        if not line.startswith('|'):
            continue
        cells = [c.strip() for c in line.strip().strip('|').split('|')]
        if len(cells) < 2:
            continue
        m = KIND_CELL.match(cells[1])
        if m:
            kinds.append(m.group(1))
    return kinds


def build_notices(chapter_text, notice_chapter_text):
    rows = notice_table(chapter_text)
    ids = [r[0] for r in rows]
    if len(ids) != len(set(ids)):
        raise ValueError('第10章 9.4 の表に同じ識別子が 2 つある')
    kinds = notice_kinds(notice_chapter_text)
    if len(kinds) != len(set(kinds)):
        raise ValueError('第7章 9 の表に同じ識別子が 2 つある')
    if not kinds:
        raise ValueError('第7章 9 の表に識別子が無い')
    missing, extra = sorted(set(kinds) - set(ids)), sorted(set(ids) - set(kinds))
    if missing or extra:
        raise ValueError(f'第7章 9 の識別子と第10章 9.4 の表が合わない（無い {missing}、余る {extra}）')
    out = {f'notice.{k}': {'what': what, 'next': nxt} for k, what, nxt in rows}
    return json.dumps(dict(sorted(out.items())), ensure_ascii=False, indent=2) + '\n'


def conditional(text):
    """「（VPS の場合）」で始まる文は形 D の時だけ示す。印を外した文と mode の組。"""
    if text.startswith(VPS):
        return text[len(VPS):].strip(), 'D'
    return text, None


def build_disclosure(text):
    """第12章 7.1 の引用（題、前書き、箇条）と 8 の表の行から明示の文の生成ファイルを作る。"""
    quote = [line[1:].strip() for line in section(text, '7.1 ').splitlines() if line.startswith('>')]
    quote = [q for q in quote if q]
    if len(quote) < 3 or not (quote[0].startswith('**') and quote[0].endswith('**')):
        raise ValueError('第12章 7.1 の引用の形が違う（題、前書き、箇条の順）')
    title, lead = quote[0].strip('*'), quote[1]
    if lead.startswith('- '):
        raise ValueError('第12章 7.1 の引用に前書きが無い')
    bullets = [q[2:].strip() for q in quote[2:]]
    if any(not q.startswith('- ') for q in quote[2:]):
        raise ValueError('第12章 7.1 の引用の前書きの後は箇条だけにする')
    outbound = []
    lines = section(text, '8. ', level='##').splitlines()
    for line in lines:
        cells = [c.strip() for c in line.strip().strip('|').split('|')] if line.startswith('|') else []
        if len(cells) != 3 or cells[0] in ('送られる情報の内容', '---'):
            continue
        content, mode = conditional(cells[0])
        row = {'content': content, 'destination': cells[1], 'purpose': cells[2]}
        if mode:
            row['mode'] = mode
        outbound.append(row)
    if not outbound:
        raise ValueError('第12章 8 の表に行が無い')
    for s in [title, lead, *bullets, *(v for r in outbound for v in r.values())]:
        if '`' in s or '**' in s:
            raise ValueError(f'利用者に見せる文に書式の記号がある：{s}')
    items = []
    for b in bullets:
        t, mode = conditional(b)
        items.append({'text': t, 'mode': mode} if mode else {'text': t})
    out = {'title': title, 'lead': lead, 'items': items, 'outbound': outbound}
    return json.dumps(out, ensure_ascii=False, indent=2) + '\n'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--check', action='store_true', help='作り直した結果とコミットした物の差分が 0 かを見る')
    ap.add_argument('--disclosure-out', help='明示の文の生成ファイル（disclosure.json）を置くディレクトリ')
    a = ap.parse_args(argv)
    chapter = CHAPTER.read_text(encoding='utf-8')
    text = build(chapter, yaml.safe_load(ERRORS.read_text(encoding='utf-8'))['rows'])
    notices = build_notices(chapter, NOTICE_CHAPTER.read_text(encoding='utf-8'))
    if a.disclosure_out:
        out = pathlib.Path(a.disclosure_out)
        out.mkdir(parents=True, exist_ok=True)
        (out / 'disclosure.json').write_text(build_disclosure(DATA_CHAPTER.read_text(encoding='utf-8')), encoding='utf-8')
        print(f'書いた：{out / "disclosure.json"}')
    if a.check:
        bad = 0
        for path, want, sec in ((OUT, text, '9.2'), (NOTICE_OUT, notices, '9.4')):
            if not path.exists() or path.read_text(encoding='utf-8') != want:
                print(f'{path.relative_to(ROOT)} が第10章 {sec} の表と違う（python -m tools.gen_texts で作り直す）')
                bad += 1
            else:
                print(f'{path.relative_to(ROOT)} は第10章 {sec} の表と同じ')
        return 1 if bad else 0
    if not a.disclosure_out:
        OUT.parent.mkdir(parents=True, exist_ok=True)
        OUT.write_text(text, encoding='utf-8')
        print(f'書いた：{OUT.relative_to(ROOT)}')
        NOTICE_OUT.write_text(notices, encoding='utf-8')
        print(f'書いた：{NOTICE_OUT.relative_to(ROOT)}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
