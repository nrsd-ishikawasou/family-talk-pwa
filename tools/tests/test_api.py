# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""境界の正本（api/）と基本設計書の照合（試験設計書 第3章 T-L2-01 の一部）。

実行：python3 -m unittest tools.tests.test_api（依存は tools/requirements.txt）
"""
import glob
import json
import os
import pathlib
import re
import unittest

import yaml
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

ROOT = pathlib.Path(__file__).resolve().parents[2]
API = ROOT / 'api'
DESIGN = ROOT / 'docs' / 'jp' / 'design'
METHODS = ('get', 'put', 'post', 'delete', 'patch', 'head')


def chapter(name):
    return (DESIGN / name).read_text(encoding='utf-8')


def section(text, start, end):
    s = text.index(start)
    e = text.index(end, s + len(start))
    return text[s:e]


def table_rows(sec):
    """md の表の本文の行（見出しと区切りを除く）を欄の並びで返す。"""
    rows = []
    for line in sec.split('\n'):
        if line.startswith('|') and not re.match(r'^\|(\s*:?-{3,}:?\s*\|)+\s*$', line):
            rows.append([c.strip() for c in re.split(r'(?<!\\)\|', line)[1:-1]])
    return rows[1:] if rows else rows


def load_yaml(name):
    return yaml.safe_load((API / name).read_text(encoding='utf-8'))


OPENAPI = load_yaml('openapi.yaml')
ASYNCAPI = load_yaml('asyncapi.yaml')
ERRORS = load_yaml('errors.yaml')
RATES = load_yaml('ratelimits.yaml')
CONFIG = load_yaml('config.yaml')
CH2 = chapter('02_基本設計書_境界.md')


def operations():
    for path, item in OPENAPI['paths'].items():
        for m in METHODS:
            if m in item:
                yield m, path, item[m]


def _route_rows():
    """第2章 5 の表の行ごとに（動詞の並び、経路の並び、備考）。第2章の書き方を openapi の形に写す（/dl/ の 5 行は 4 つの operation。/api/v1/family/icon は /family/icon）。"""
    sec = section(CH2, '## 5. 資源の一覧（API）', '\n## 6. ')
    for line in sec.split('\n'):
        if not line.startswith('|`'):
            continue
        cells = [c.strip() for c in re.split(r'(?<!\\)\|', line)[1:-1]]
        paths = [p.split('（')[0].strip() for p in re.findall(r'`([^`]+)`', cells[0]) if p != '/api/v1']
        full = []
        for p in paths:
            if full and p in ('/verify', '/unsuspend'):
                p = full[0].rsplit('/', 1)[0] + p
            full.append(p)
        verbs = [v.strip().split('（')[0].lower() for v in cells[1].split('、')] if len(cells) >= 4 else ['get']
        verbs = ['get' if v == 'websocket' else v for v in verbs]
        out = []
        for p in full:
            p = p.replace('/api/v1/family/icon', '/family/icon').replace('/maps/<版>/{file}', '/maps/{version}/{file}')
            if p in ('/dl/app-<abi>.apk', '/dl/*.sha256'):
                p = '/dl/{file}'
            if p == '/dl/manual/':
                p = '/dl/manual/{path}'
            out.append(p)
        yield verbs, out, cells[-1]


def spec_routes():
    """第2章 5 の表の経路と動詞の組。"""
    return {(v, p) for verbs, paths, _ in _route_rows() for p in paths for v in verbs}


def query_names(item, op):
    """経路と操作の query の引数の名（components/parameters の参照を解く）。"""
    names = set()
    for prm in item.get('parameters', []) + op.get('parameters', []):
        if '$ref' in prm:
            prm = OPENAPI['components']['parameters'][prm['$ref'].rsplit('/', 1)[1]]
        if prm['in'] == 'query':
            names.add(prm['name'])
    return names


class TestRoutes(unittest.TestCase):
    # R: R-2-8-1
    def test_routes_match_chapter2_section5(self):
        ops = {(m, p) for m, p, _ in operations()}
        internal = {('get', '/internal/auth'), ('post', '/internal/sfu/webhook')}
        spec = spec_routes()
        self.assertEqual(spec - ops, set(), '第2章 5 にあって openapi.yaml に無い')
        self.assertEqual(ops - spec - internal, set(), 'openapi.yaml にあって第2章 5 に無い')

    # R: R-2-2-4
    def test_query_parameters_match_chapter2_section5(self):
        """第2章 5 の備考の ?名前= は、その行のどれかの動詞の操作の query にある。"""
        checked = 0
        for verbs, paths, note in _route_rows():
            names = set(re.findall(r'[?&]([a-z_]+)=', note))
            if not names:
                continue
            for p in paths:
                item = OPENAPI['paths'][p]
                have = set().union(*(query_names(item, item[v]) for v in verbs if v in item))
                checked += 1
                with self.subTest(path=p):
                    self.assertLessEqual(names, have)
        self.assertGreaterEqual(checked, 8)

    # R: R-2-2-4
    def test_paged_lists_have_cursor_parameters(self):
        """応答が has_more を持つ一覧は limit と、before/after か since か since_epoch を持つ（第2章 3）。"""
        schemas = OPENAPI['components']['schemas']
        for m, p, o in operations():
            body = o['responses'].get('200', {}).get('content', {}).get('application/json', {}).get('schema', {})
            if '$ref' in body and body['$ref'].startswith('#/'):
                body = schemas[body['$ref'].rsplit('/', 1)[1]]
            if 'has_more' not in body.get('properties', {}):
                continue
            names = query_names(OPENAPI['paths'][p], o)
            with self.subTest(op=o['operationId']):
                self.assertIn('limit', names)
                self.assertTrue({'before', 'after'} <= names or names & {'since', 'since_epoch'}, names)
        limit = OPENAPI['components']['parameters']['Limit']['schema']
        rule = re.search(r'`limit` は (\d+)〜(\d+)、既定 (\d+)', section(CH2, '## 3. やり取りの分類と規則', '\n## 4. '))
        self.assertEqual((limit['minimum'], limit['maximum'], limit['default']), tuple(int(x) for x in rule.groups()))

    def test_implied_errors_are_listed(self):
        """x- の属性から暗に返る番号が x-errors にある（第2章 10）。"""
        on_exceed = {p['name']: p.get('on_exceed') for p in RATES['policies']}
        session = {'session', 'approved', 'device_self', 'room_member', 'admin', 'shell'}
        approved = {'approved', 'room_member', 'admin'}
        for m, p, o in operations():
            a, errs, want = set(o['x-authz']), set(o.get('x-errors', [])), set()
            if a <= session:
                want |= {'AUTH-001', 'AUTH-002'}
                if a <= approved:
                    want.add('KEY-004')
                if 'device_self' in a:
                    want.add('KEY-001')
                if 'room_member' in a:
                    want.add('ROOM-002')
                if 'admin' in a:
                    want.add('ADM-001')
            if o.get('x-idempotency') == 'key':
                want |= {'NET-007', 'NET-008'}
            if o.get('x-reauth'):
                want.add('AUTH-006')
            want |= {on_exceed[r] for r in o.get('x-ratelimit', []) if on_exceed.get(r) and on_exceed[r] != 'NET-004'}
            with self.subTest(op=o['operationId']):
                self.assertLessEqual(want, errs)

    def test_readonly_allow_is_documented(self):
        """x-readonly は allow だけで、書き込みの動詞に付き、第2章 10 が対象を挙げる（第1章 DD-1-15）。"""
        rule = section(CH2, '- 読み取り専用（第1章 DD-1-15）の時', '\n')
        allowed = [o for m, p, o in operations() if 'x-readonly' in o]
        self.assertTrue(allowed)
        for m, p, o in operations():
            if 'x-readonly' not in o:
                continue
            with self.subTest(op=o['operationId']):
                self.assertEqual(o['x-readonly'], 'allow')
                self.assertIn(m, ('post', 'put', 'patch', 'delete'))
        self.assertIn('x-readonly: allow', rule)

    def test_deprecations_are_well_formed(self):
        """x-deprecation は deprecated: true と組で、sunset は at より前にしない（第2章 10。RFC 9745 の 2.1）。"""
        for m, p, o in operations():
            if 'x-deprecation' not in o and not o.get('deprecated'):
                continue
            with self.subTest(op=o['operationId']):
                self.assertTrue(o.get('deprecated'))
                d = o['x-deprecation']
                self.assertEqual(set(d), {'at', 'sunset'})
                self.assertLessEqual(str(d['at']), str(d['sunset']))

    # R: R-2-1-1
    def test_every_operation_has_valid_authz(self):
        sec = section(CH2, '|`x-authz` の値|通す要求|', '\n\n')
        values = set()
        for row in table_rows(sec):
            values.update(re.findall(r'`([a-z_]+)`', row[0]))
        rsec = section(CH2, '|`x-authz-rule` の名前|確かめ|', '\n\n')
        rules = {re.findall(r'`([a-z_]+)`', r[0])[0] for r in table_rows(rsec)}
        for m, p, o in operations():
            with self.subTest(op=o['operationId']):
                self.assertTrue(o.get('x-authz'), 'x-authz が無い')
                self.assertLessEqual(set(o['x-authz']), values)
                if 'x-authz-rule' in o:
                    self.assertIn(o['x-authz-rule'], rules)
                if 'x-reauth' in o:
                    self.assertIn(o['x-reauth'], (True, 'conditional'))
                self.assertIn(o['x-idempotency'], ('key', 'natural', 'none'))
                if o['x-idempotency'] == 'natural':
                    self.assertIn('x-idempotency-natural', o)

    # R: R-2-8-1
    def test_operation_ids_unique_and_named(self):
        ids = [o['operationId'] for _, _, o in operations()]
        self.assertEqual(len(ids), len(set(ids)))
        for i in ids:
            self.assertRegex(i, r'^[a-z][A-Za-z0-9]*$')


class TestErrors(unittest.TestCase):
    # R: R-2-2-2
    def test_ledger_matches_chapter2_section9_2(self):
        sec = section(CH2, '### 9.2 エラーコード表の初期の項目', '### 9.3')
        spec = re.findall(r'^\|([A-Z]{3,5}-\d{3})\|', sec, re.M)
        codes = [r['code'] for r in ERRORS['rows']]
        self.assertEqual(codes, spec)
        self.assertEqual(len(codes), len(set(codes)))

    # R: R-2-2-2
    def test_rows_have_required_fields(self):
        cats = re.search(r'分類は 11：([^|]+)\|', chapter('01_基本設計書_全体構成.md')).group(1)
        allowed = set(re.findall(r'([A-Z]{3,5})（', cats))
        self.assertEqual(len(allowed), 11)
        for r in ERRORS['rows']:
            with self.subTest(code=r['code']):
                self.assertRegex(r['code'], r'^[A-Z]{3,5}-\d{3}$')
                self.assertIn(r['code'].split('-')[0], allowed)
                self.assertTrue(r['title'])
                self.assertEqual(r['user_text_key'], 'err.' + r['code'])
                self.assertEqual(r['test'], 'TestErr_' + r['code'].replace('-', '_'))
                self.assertTrue('retryable' in r or r.get('warning') is True)
                Draft202012Validator.check_schema(r['params'])

    # R: R-2-2-2
    def test_x_errors_are_in_ledger(self):
        codes = {r['code'] for r in ERRORS['rows']}
        glob_ = {r['code'] for r in ERRORS['rows'] if r['scope'] == 'global'}
        for _, _, o in operations():
            for c in o.get('x-errors', []):
                with self.subTest(op=o['operationId'], code=c):
                    self.assertIn(c, codes)
                    self.assertNotIn(c, glob_, 'global の番号は x-errors に書かない')


class TestRateLimits(unittest.TestCase):
    # R: R-11-4-2
    def test_policies_match_chapter2_section9_3(self):
        sec = section(CH2, '### 9.3 レート制限', '## 10.')
        rows = [r for r in table_rows(sec) if r]
        self.assertEqual(len(RATES['policies']), len(rows))
        names = {p['name'] for p in RATES['policies']}
        codes = {r['code'] for r in ERRORS['rows']}
        for _, _, o in operations():
            for n in o.get('x-ratelimit', []):
                self.assertIn(n, names)
        for p in RATES['policies']:
            self.assertIn(p['on_exceed'], codes)
            for lim in p['limits']:
                self.assertIn(lim['key'], ('ip', 'user', 'device', 'room', 'connection'))


class TestConfig(unittest.TestCase):
    # R: R-11-4-2
    def test_categories_and_keys(self):
        cats = {c['name'] for c in CONFIG['categories']}
        keys = [i['key'] for i in CONFIG['items']]
        self.assertEqual(len(keys), len(set(keys)))
        self.assertEqual({k.split('.')[0] for k in keys}, cats)
        for i in CONFIG['items']:
            self.assertLessEqual(set(i['layers']), {'env', 'file', 'db', 'user', 'room_me', 'device', 'guardian'})
            self.assertEqual(i['locked_when_env_or_file'], bool({'env', 'file'} & set(i['layers'])))

    # R: R-11-4-2
    def test_env_matches_chapter11_section3_2(self):
        sec = section(chapter('11_基本設計書_運用.md'), '### 3.2 env.example', '### 3.3')
        envs = {e for e in re.findall(r'`(FT_[A-Z0-9_]+)`', sec) if 'DIGEST' not in e}
        self.assertEqual({i['env'] for i in CONFIG['items'] if i.get('env')}, envs)


class TestSchemas(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        reg = Registry()
        cls.files = sorted(glob.glob(str(API / 'schemas' / '**' / '*.json'), recursive=True))
        for f in cls.files:
            reg = reg.with_resource(pathlib.Path(f).as_uri(), Resource.from_contents(json.loads(pathlib.Path(f).read_text(encoding='utf-8'))))
        for y, doc in (('openapi.yaml', OPENAPI), ('asyncapi.yaml', ASYNCAPI)):
            reg = reg.with_resource((API / y).as_uri(), Resource.from_contents(doc, default_specification=DRAFT202012))
        cls.reg = reg

    def resolve_all(self, uri):
        list(Draft202012Validator({'$ref': uri}, registry=self.reg).iter_errors({}))

    # R: R-2-2-2
    def test_shared_schemas_valid_and_resolvable(self):
        for f in self.files:
            with self.subTest(f=os.path.relpath(f, API)):
                d = json.loads(pathlib.Path(f).read_text(encoding='utf-8'))
                Draft202012Validator.check_schema(d)
                self.assertTrue(d['$id'].startswith('https://family-talk.invalid/schemas/'))
                self.resolve_all(pathlib.Path(f).as_uri())

    # R: R-2-2-2
    def test_openapi_and_asyncapi_schemas_valid_and_resolvable(self):
        for n, s in OPENAPI['components']['schemas'].items():
            with self.subTest(schema=n):
                Draft202012Validator.check_schema(s)
                self.resolve_all((API / 'openapi.yaml').as_uri() + '#/components/schemas/' + n)
        for n, m in ASYNCAPI['components']['messages'].items():
            with self.subTest(message=n):
                Draft202012Validator.check_schema(m['payload'])
                self.resolve_all((API / 'asyncapi.yaml').as_uri() + '#/components/messages/' + n + '/payload')

    # R: R-2-3-1
    def test_asyncapi_messages_match_chapter2(self):
        sec = section(CH2, '### 6.2 イベントの一覧', '### 6.3')
        types = []
        for row in table_rows(sec):
            types += re.findall(r'`([a-z.]+)`', row[0])
        ws = [m['name'] for k, m in ASYNCAPI['components']['messages'].items() if k.startswith('ws')]
        self.assertEqual(sorted(ws), sorted(types))
        sec64 = section(CH2, '### 6.4 配布元の常時接続', '## 7.')
        up_types = []
        for row in table_rows(sec64):
            up_types += re.findall(r'`([a-z]+)`', row[0])
        up = [m['name'] for k, m in ASYNCAPI['components']['messages'].items() if k.startswith('up')]
        self.assertEqual(sorted(up), sorted(up_types))

    # R: R-9-1-1
    def test_bridge_matches_chapter9_section4_2(self):
        ch9 = chapter('09_基本設計書_Androidのシェル.md')
        sec = section(ch9, '### 4.2 JS ブリッジの命令', '\n## 5.')
        cmd_part, ev_part = sec.split('|event（シェルからウェブアプリへ）|')
        cmds = [n for r in table_rows(cmd_part) for n in re.findall(r'`([A-Za-z]+)`', r[0])]
        evs = [n for r in table_rows('|e|d|\n|---|---|\n' + ev_part) for n in re.findall(r'`([A-Za-z]+)`', r[0])]
        bridge = json.loads((API / 'schemas' / 'ft.bridge' / '1.json').read_text(encoding='utf-8'))
        self.assertEqual(sorted(k[:-5] for k in bridge['$defs'] if k.endswith('_args')), sorted(cmds))
        self.assertEqual(sorted(k[:-5] for k in bridge['$defs'] if k.endswith('_data')), sorted(evs))


if __name__ == '__main__':
    unittest.main()
