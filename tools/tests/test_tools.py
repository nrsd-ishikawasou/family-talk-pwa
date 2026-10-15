# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""道具の試験（L0。全体実装設計書 6.1）。正しい入力で緑、壊した入力で赤になることを確かめる。"""
import contextlib
import io
import json
import pathlib
import re
import tempfile
import unittest

import yaml

from tools import bundle_openapi, config_check, errors_check, forbidden_words, gen_config, gen_errors, gen_texts, no_plaintext, trace_requirements

ROOT = pathlib.Path(__file__).resolve().parents[2]


class TestGenConfig(unittest.TestCase):
    # R: R-2-8-1
    def test_committed_schemas_are_reproduced(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(gen_config.main(['--check']), 0)

    def test_strips_category_for_single_category_sets(self):
        built = {p.parent.name: p for p in gen_config.build()}
        self.assertIn('ft.settings.room_me', built)


class TestGenErrors(unittest.TestCase):
    """api/ の errors.yaml、ratelimits.yaml、config.yaml から Go の定数を作る道具（全体実装設計書 5.1）。"""

    def load(self, name):
        return yaml.safe_load((ROOT / 'api' / name).read_text(encoding='utf-8'))

    # R: R-2-8-1
    def test_output_is_reproducible(self):
        a = gen_errors.config_go(self.load('config.yaml')) + gen_errors.errors_go(self.load('errors.yaml'))
        b = gen_errors.config_go(self.load('config.yaml')) + gen_errors.errors_go(self.load('errors.yaml'))
        self.assertEqual(a, b)

    def test_required_and_nullable_follow_config_yaml(self):
        doc = self.load('config.yaml')
        out = gen_errors.config_go(doc)
        required = sorted(it['key'] for it in doc['items'] if gen_errors.required(it))
        self.assertEqual(required, ['server.dns_provider', 'server.domain'])
        self.assertEqual(out.count('Required: true'), len(required))
        nullable = [it for it in doc['items'] if 'null_means' in (it.get('range') or {})]
        self.assertEqual(out.count('Nullable: true'), len(nullable))
        for it in nullable:
            self.assertIn(gen_errors.q(it['range']['null_means']), out)

    def test_every_error_code_has_a_constant_and_shell_rows_have_no_status(self):
        doc = self.load('errors.yaml')
        out = gen_errors.errors_go(doc)
        for r in doc['rows']:
            self.assertIn(f'\t{gen_errors.ident(r["code"])} = "{r["code"]}"\n', out)
            if r['code'].startswith('SHELL-'):
                self.assertIn(f'{{Code: {gen_errors.ident(r["code"])}, Status: 0,', out)

    def test_broken_input_is_an_error(self):
        doc = self.load('config.yaml')
        doc['items'][0] = dict(doc['items'][0])
        del doc['items'][0]['type']
        with self.assertRaises(KeyError):
            gen_errors.config_go(doc)


class TestBundleOpenapi(unittest.TestCase):
    """oapi-codegen の前に openapi を束ねる道具（基本設計書 第14章 8）。"""

    @classmethod
    def setUpClass(cls):
        cls.text = (ROOT / 'api' / 'openapi.yaml').read_text(encoding='utf-8')
        cls.spec = bundle_openapi.bundle(cls.text)

    def refs(self, node):
        if isinstance(node, dict):
            if isinstance(node.get('$ref'), str):
                yield node['$ref']
            for v in node.values():
                yield from self.refs(v)
        elif isinstance(node, list):
            for v in node:
                yield from self.refs(v)

    def operations(self, spec):
        return sorted(op['operationId'] for item in spec['paths'].values()
                      for m, op in item.items() if m in bundle_openapi.METHODS)

    # R: R-2-8-1
    def test_output_is_reproducible(self):
        self.assertEqual(bundle_openapi.bundle(self.text), self.spec)

    def test_no_external_refs_remain_and_all_refs_resolve(self):
        refs = list(self.refs(self.spec))
        self.assertTrue(refs)
        for ref in refs:
            self.assertTrue(ref.startswith('#/'), ref)
            node = self.spec
            for part in ref[2:].split('/'):
                node = node[part.replace('~1', '/').replace('~0', '~')]

    def test_schema_names(self):
        p = ROOT / 'api' / 'schemas' / 'ft.event.mls' / '1.json'
        self.assertEqual(bundle_openapi.schema_name(p), 'FtEventMls1')
        self.assertIn('FtEventMls1', self.spec['components']['schemas'])

    def test_paths_are_absolute_and_operations_are_kept(self):
        original = yaml.safe_load(self.text)
        self.assertEqual(self.operations(self.spec), self.operations(original))
        self.assertEqual(self.spec['servers'], [{'url': '/'}])
        for path, item in self.spec['paths'].items():
            self.assertNotIn('servers', item, path)
        self.assertIn('/health', self.spec['paths'])
        self.assertIn('/internal/auth', self.spec['paths'])
        self.assertIn('/api/v1/sync', self.spec['paths'])
        self.assertNotIn('/sync', self.spec['paths'])

    def test_generator_config(self):
        conf = bundle_openapi.oapi_config(self.spec)
        self.assertEqual(conf['output-options']['exclude-operation-ids'], ['connectUpWs', 'connectWs'])
        self.assertEqual(conf['output-options']['exclude-tags'], ['static'])
        self.assertNotIn('embedded-spec', conf['generate'])

    def test_broken_servers_are_errors(self):
        def spec(paths, servers=None):
            return {'servers': servers or [{'url': '/api/v1'}], 'paths': paths}
        op = {'get': {'operationId': 'x'}}
        with self.assertRaises(ValueError):
            bundle_openapi.absolute_paths(spec({'/a': dict(op), '/api/v1/a': {**op, 'servers': [{'url': '/'}]}}))
        with self.assertRaises(ValueError):
            bundle_openapi.absolute_paths(spec({'/a': {'get': {'operationId': 'x', 'servers': [{'url': '/'}]}}}))
        with self.assertRaises(ValueError):
            bundle_openapi.absolute_paths(spec({'/a': dict(op)}, [{'url': '/{v}'}]))
        with self.assertRaises(ValueError):
            bundle_openapi.absolute_paths(spec({'/a': dict(op)}, [{'url': '/a'}, {'url': '/b'}]))


class TestErrorsCheck(unittest.TestCase):
    """エラーコード表と L1 の試験の名と文言の照合（全体実装設計書 6.3）。"""

    # R: R-2-8-1
    def test_repository_rows_for_finished_blocks_have_l1_tests(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(errors_check.main(['--tests', 'NET,SRV-001,SRV-003,SRV-004']), 0)

    def test_detects_missing_test_key_and_text(self):
        rows = [{'code': 'NET-001', 'test': 'TestErr_NET_001', 'user_text_key': 'err.NET-001'},
                {'code': 'NET-002', 'test': 'TestNet2', 'user_text_key': 'err.NET-002'},
                {'code': 'SRV-002', 'test': 'TestErr_SRV_002', 'user_text_key': 'x'}]
        problems = errors_check.check(rows, ['NET', 'SRV-002', 'KEY'], {'TestErr_NET_001': None}, {'err.NET-001': 'a'})
        text = '\n'.join(problems)
        self.assertIn('NET-002：test の欄', text)
        self.assertIn('NET-002：L1 に試験 TestErr_NET_002 が無い', text)
        self.assertIn('SRV-002：user_text_key の欄', text)
        self.assertIn('SRV-002：L1 に試験', text)
        self.assertIn('NET-002：文言の表に err.NET-002 が無い', text)
        self.assertIn('--tests の KEY に当たる行が無い', text)
        self.assertNotIn('NET-001', text)

    def test_only_integration_files_count(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / 'a_test.go').write_text('package a\nfunc TestErr_NET_001(t *testing.T) {}\n', encoding='utf-8')
            (p / 'b_test.go').write_text('//go:build integration\n\npackage a\nfunc TestErr_NET_002(t *testing.T) {}\n', encoding='utf-8')
            self.assertEqual(set(errors_check.l1_tests(p)), {'TestErr_NET_002'})


class TestGenTexts(unittest.TestCase):
    """エラーの文言の表（第10章 9.2 が正本）。"""

    # R: R-10-7-1
    def test_committed_texts_are_reproduced(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(gen_texts.main(['--check']), 0)

    def test_table_and_errors_must_match(self):
        doc = '### 9.2 文言\n\n|番号|1 文目|2 文目|\n|---|---|---|\n|NET-001|a。|b。|\n\n### 9.3 次\n'
        out = json.loads(gen_texts.build(doc, [{'code': 'NET-001', 'user_text_key': 'err.NET-001'}]))
        self.assertEqual(out, {'err.NET-001': {'what': 'a。', 'next': 'b。'}})
        with self.assertRaises(ValueError):
            gen_texts.build(doc, [{'code': 'NET-001', 'user_text_key': 'err.NET-001'},
                                  {'code': 'NET-002', 'user_text_key': 'err.NET-002'}])
        with self.assertRaises(ValueError):
            gen_texts.build(doc.replace('|NET-001|a。|b。|', '|NET-001|a。|b。|\n|NET-001|c。|d。|'),
                            [{'code': 'NET-001', 'user_text_key': 'err.NET-001'}])

    # R: R-7-6-1
    def test_notice_table_and_chapter_7_must_match(self):
        doc10 = '### 9.4 知らせ\n\n|識別子|1 文目|2 文目|\n|---|---|---|\n|disk_usage|a。|b。|\n|paper_check|c。||\n\n## 10. 次\n'
        doc7 = '## 9. 知らせ\n\n|知らせ|識別子|level|\n|---|---|---|\n|容量|`disk_usage`|warning|\n|誕生日|—|—|\n|紙|`paper_check`|info|\n\n## 10. 次\n'
        out = json.loads(gen_texts.build_notices(doc10, doc7))
        self.assertEqual(out, {'notice.disk_usage': {'what': 'a。', 'next': 'b。'}, 'notice.paper_check': {'what': 'c。', 'next': ''}})
        with self.assertRaises(ValueError):
            gen_texts.build_notices(doc10, doc7.replace('|紙|`paper_check`|info|', '|紙|`paper_check`|info|\n|地図|`maps_update`|info|'))
        with self.assertRaises(ValueError):
            gen_texts.build_notices(doc10.replace('|paper_check|c。||', ''), doc7)

    # R: R-12-5-1
    def test_disclosure_from_chapter_12(self):
        out = json.loads(gen_texts.build_disclosure(gen_texts.DATA_CHAPTER.read_text(encoding='utf-8')))
        self.assertIn('{admin}', out['lead'])
        self.assertTrue(out['items'] and out['outbound'])
        vps = [i for i in out['items'] if i.get('mode') == 'D']
        self.assertEqual(len(vps), 1)
        self.assertIn('{provider}', vps[0]['text'])
        self.assertFalse(vps[0]['text'].startswith(gen_texts.VPS))
        self.assertEqual([r['mode'] for r in out['outbound'] if 'mode' in r], ['D'])
        texts = [out['title'], out['lead'], *(i['text'] for i in out['items'])]
        texts += [v for r in out['outbound'] for k, v in r.items() if k != 'mode']
        for t in texts:
            self.assertEqual(set(re.findall(r'\{(\w+)\}', t)) - {'admin', 'provider', 'card_holders'}, set(), t)

    def test_disclosure_shape_errors(self):
        doc = ('### 7.1 文\n\n> **題**\n>\n> 前書き{admin}。\n>\n> - 項目。\n\n## 8. 表\n\n'
               '|送られる情報の内容|送信先|利用の目的|\n|---|---|---|\n|（VPS の場合）全部|事業者|稼働|\n\n## 9. 次\n')
        out = json.loads(gen_texts.build_disclosure(doc))
        self.assertEqual(out['outbound'], [{'content': '全部', 'destination': '事業者', 'purpose': '稼働', 'mode': 'D'}])
        with self.assertRaises(ValueError):
            gen_texts.build_disclosure(doc.replace('> **題**', '> 題'))
        with self.assertRaises(ValueError):
            gen_texts.build_disclosure(doc.replace('項目。', '`項目`。'))
        with self.assertRaises(ValueError):
            gen_texts.build_disclosure(doc.replace('|（VPS の場合）全部|事業者|稼働|\n', ''))


class TestNoPlaintext(unittest.TestCase):
    """平文の走査（全体実装設計書 6.4。T-L1-16）。"""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.dir.name)
        self.markers = self.root / 'markers.txt'
        self.markers.write_text('aaaabbbbcccc\nzzzzyyyyxxxx\n', encoding='utf-8')
        self.env = no_plaintext.os.environ.pop(no_plaintext.ENV, None)

    def tearDown(self):
        if self.env is not None:
            no_plaintext.os.environ[no_plaintext.ENV] = self.env
        self.dir.cleanup()

    def run_tool(self, *paths):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            rc = no_plaintext.main(['--markers', str(self.markers), *map(str, paths)])
        return rc, out.getvalue()

    # R: R-12-1-1
    def test_finds_marker_without_printing_it(self):
        data = self.root / 'data'
        data.mkdir()
        (data / 'dump.sql').write_bytes(b'xx zzzzyyyyxxxx yy')
        (data / 'clean.bin').write_bytes(b'nothing here')
        rc, out = self.run_tool(data, self.markers)
        self.assertEqual(rc, 1)
        self.assertIn('語 2 番、位置 3', out)
        self.assertNotIn('zzzzyyyyxxxx', out)
        self.assertIn('ファイル 2、見つけた 1', out)  # 語の一覧そのものは走査しない

    def test_clean_and_chunk_boundary(self):
        f = self.root / 'big.bin'
        f.write_bytes(b'.' * 10 + b'aaaabbbbcccc' + b'.' * 10)
        old = no_plaintext.CHUNK
        no_plaintext.CHUNK = 16  # 語が区切りの境目をまたぐ
        try:
            self.assertEqual(no_plaintext.scan_file(f, [b'aaaabbbbcccc']), [(1, 10)])
        finally:
            no_plaintext.CHUNK = old
        clean = self.root / 'clean.txt'
        clean.write_bytes(b'aaaabbbbccc')
        self.assertEqual(self.run_tool(clean)[0], 0)

    def test_missing_or_weak_markers_stop(self):
        with self.assertRaises(SystemExit):
            no_plaintext.load_markers(self.root / 'none.txt')
        weak = self.root / 'weak.txt'
        weak.write_text('short\n', encoding='utf-8')
        with self.assertRaises(SystemExit):
            no_plaintext.load_markers(weak)
        made = self.root / 'made.txt'
        self.assertTrue(no_plaintext.generate(made, 3))
        self.assertEqual(len(no_plaintext.load_markers(made)), 3)
        self.assertFalse(no_plaintext.generate(made, 3))


class TestConfigCheck(unittest.TestCase):
    # R: R-11-4-2
    def test_repository_config_is_consistent(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(config_check.main([]), 0)

    def test_detects_default_out_of_range_and_bad_layer(self):
        items = [{'key': 'x.a', 'type': 'int', 'default': 5, 'range': {'min': 10}, 'layers': ['db'], 'chapter': '第1章'},
                 {'key': 'x.b', 'type': 'bool', 'default': True, 'layers': ['nowhere'], 'chapter': '第1章'},
                 {'key': 'x.b', 'type': 'bool', 'default': True, 'layers': ['db'], 'chapter': ''}]
        bad = config_check.check_items(items)
        self.assertTrue(any('下限' in b for b in bad))
        self.assertTrue(any('置ける場所' in b for b in bad))
        self.assertTrue(any('重複' in b for b in bad))
        self.assertTrue(any('出典' in b for b in bad))

    def test_reads_units_and_per_unit_qualifiers(self):
        self.assertEqual([n for n, _ in config_check.nums('15 分')], [900])
        self.assertEqual([n for n, _ in config_check.nums('100、1 パック 40')], [100, 40])
        self.assertEqual([n for n, _ in config_check.nums('1 億')], [1e8])


class TestForbiddenWords(unittest.TestCase):
    def test_missing_list_is_an_error(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(forbidden_words.main(['--list', '/nonexistent/list.txt']), 2)

    def test_word_boundaries_and_japanese_substrings(self):
        ascii_pat = forbidden_words.pattern('line')
        self.assertTrue(ascii_pat.search('use LINE here'))
        self.assertFalse(ascii_pat.search('online offline'))
        self.assertTrue(forbidden_words.pattern('例の語').search('この例の語を含む'))

    def test_report_does_not_reveal_the_word(self):
        # 語は実行時に組み立てる（追跡しているこのファイル自身が走査で当たらないように）
        word = 'zz-secret-' + 'word-qq'
        with tempfile.TemporaryDirectory() as d:
            lst = pathlib.Path(d) / 'list.txt'
            lst.write_text('# 注記\n' + word + '\n', encoding='utf-8')
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                code = forbidden_words.main(['--list', str(lst)])
            self.assertEqual(code, 0)
            self.assertNotIn(word, out.getvalue())


class TestTraceRequirements(unittest.TestCase):
    # R: R-2-8-1
    def test_repository_trace_is_consistent(self):
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(trace_requirements.main([]), 0)

    def test_note_forms(self):
        lines = ['# R: R-2-8-1', '// R: R-4-2-1、R-4-2-2', '/* R: R-5-1-1 */', '@Requirement("R-9-1-1")']
        found = []
        for line in lines:
            for m in trace_requirements.NOTE.finditer(line):
                found += __import__('re').findall(r'R-\d+-\d+-\d+', m.group(1) or m.group(2))
        self.assertEqual(found, ['R-2-8-1', 'R-4-2-1', 'R-4-2-2', 'R-5-1-1', 'R-9-1-1'])


if __name__ == '__main__':
    unittest.main()
