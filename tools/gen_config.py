# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""config.yaml から api/schemas の ft.settings.* を作る（基本設計書 第2章 12、第11章 7.2、第14章 8）。

使い方：python -m tools.gen_config [--check]
  --check：書かずに、作った結果がコミットしてある物と同じかを確かめる（違えば 1）。
"""
import argparse
import json
import pathlib
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
API = ROOT / 'api'
BASE = 'https://family-talk.invalid/schemas/'
SJ = 'https://json-schema.org/draft/2020-12/schema'
TYPES = {'int': 'integer', 'duration': 'integer', 'bytes': 'integer', 'percent': 'integer', 'bool': 'boolean',
         'string': 'string', 'enum': 'string', 'list': 'array', 'object': 'object'}
# 形の名、選び方（置ける場所）、説明。1 つの分類だけの形は頭の分類名を外す（第7章 6 の書き方）
SETS = [
    ('ft.settings.user', lambda layers: 'user' in layers, '構成員ごとの設定（/me/settings）', True),
    ('ft.settings.room_me', lambda layers: 'room_me' in layers, 'トークごとの自分の設定（/rooms/{id}/me）', True),
    ('ft.settings.server', lambda layers: bool({'env', 'file', 'db'} & set(layers)),
     'サーバーの設定（/admin/settings。env と file 由来はロック表示）', False),
    ('ft.settings.guardian', lambda layers: 'guardian' in layers, '保護者が決める子の制限（/me/guardian/children/{id}/controls）', True),
]


def prop(item):
    d = {'type': TYPES[item['type']]}
    if item['default'] is None:
        d = {'type': [d['type'], 'null']}
    rng = item.get('range') or {}
    if 'enum' in rng:
        d['enum'] = rng['enum'] + ([None] if item['default'] is None else [])
    if 'min' in rng:
        d['minimum'] = rng['min']
    if 'max' in rng:
        d['maximum'] = rng['max']
    if item['type'] == 'percent':
        d.setdefault('minimum', 0)
        d.setdefault('maximum', 100)
    d['default'] = item['default']
    d['description'] = item['chapter'] + ('。' + item['note'] if item.get('note') else '')
    return d


def build():
    cfg = yaml.safe_load((API / 'config.yaml').read_text(encoding='utf-8'))
    out = {}
    for name, pick, desc, strip in SETS:
        items = [i for i in cfg['items'] if pick(i['layers'])]
        props = {(i['key'].split('.', 1)[1] if strip else i['key']): prop(i) for i in items}
        doc = {'$schema': SJ, '$id': f'{BASE}{name}/1', 'title': f'{name}/1',
               'description': f'第2章 12。{desc}。api/config.yaml から生成', 'type': 'object',
               'properties': props, 'additionalProperties': False}
        out[API / 'schemas' / name / '1.json'] = json.dumps(doc, ensure_ascii=False, indent=2) + '\n'
    return out


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--check', action='store_true', help='書かずに、コミットしてある物と同じかを確かめる')
    a = ap.parse_args(argv)
    diff = []
    for path, text in build().items():
        if a.check:
            if not path.exists() or path.read_text(encoding='utf-8') != text:
                diff.append(str(path.relative_to(ROOT)))
        else:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text, encoding='utf-8')
    if diff:
        print('作り直すと変わる：' + '、'.join(diff))
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
