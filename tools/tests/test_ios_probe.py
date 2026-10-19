# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""iPhone の Safari の確かめの道具の試験（L0。全体実装設計書 6.3、基本設計書 第14章 11.6、試験設計書 T-L3-12、T-L6-09）。

macOS と Simulator は要らない。判定、版と名前の選び方、再試行の数え方、送る中身の形、ページと道具の項目の名前の一致、
証明書と HTTPS の配信、Web Push の暗号化（RFC 8291 の例）を開発機で確かめる。
"""
import contextlib
import glob
import io
import json
import pathlib
import re
import shutil
import ssl
import subprocess
import tempfile
import unittest
import urllib.request
from types import SimpleNamespace

from jsonschema import Draft202012Validator
from referencing import Registry, Resource

from tools import ios_probe as core
from tools.ios_probe import __main__ as cli
from tools.ios_probe import push, sim

ROOT = pathlib.Path(__file__).resolve().parents[2]
PAGE = ROOT / 'web' / 'probe'
SCHEMAS = ROOT / 'api' / 'schemas'


def probe(**over):
    """全部 ok のページの JSON（Safari のタブ）。over で項目の結果を差し替える。"""
    items = {i: {'result': 'ok', 'detail': ''} for ids in core.ITEMS.values() for i in ids}
    items['L3-4-display-mode']['detail'] = 'browser'
    for k, v in over.items():
        items[k.replace('_', '-')] = v
    return {'format': core.FORMAT, 'done': True, 'env': {'ua_os_version': '18.6', 'origin': 'https://localhost:1'}, 'items': items}


class TestJudge(unittest.TestCase):
    # R: R-12-3-2
    def test_all_ok_passes(self):
        v = core.judge(probe())
        self.assertTrue(v['pass'])
        self.assertEqual({g: x['verdict'] for g, x in v['groups'].items()}, {'1': 'ok', '2': 'ok', '3': 'ok', '4': 'ok'})

    def test_one_crypto_item_fails_the_whole(self):
        v = core.judge(probe(**{'L3-1-import-raw-compressed-rejected': {'result': 'ng', 'detail': '圧縮の点の raw を受けた'}}))
        self.assertFalse(v['pass'])
        self.assertEqual(v['groups']['1']['verdict'], 'ng')
        self.assertIn('圧縮', v['groups']['1']['reasons'][0])

    # R: R-3-3-1
    def test_passkey_not_run_is_not_red_but_ng_is(self):
        v = core.judge(probe(**{'L3-3-create': {'result': 'not_run', 'detail': 'NotAllowedError'}, 'L3-3-get': {'result': 'not_run', 'detail': ''}}))
        self.assertTrue(v['pass'])
        self.assertEqual(v['groups']['3']['verdict'], 'not_run')
        v = core.judge(probe(**{'L3-3-create': {'result': 'ok', 'detail': ''}, 'L3-3-get': {'result': 'ng', 'detail': 'SecurityError'}}))
        self.assertFalse(v['pass'])

    # R: R-8-2-1
    def test_not_run_outside_passkey_is_red(self):
        # 開き直しを見ていない（1 回しか開いていない）JSON は不可
        v = core.judge(probe(**{'L3-2-reopen': {'result': 'not_run', 'detail': 'まだ開き直していない'}}))
        self.assertFalse(v['pass'])

    def test_missing_item_done_and_format(self):
        p = probe()
        del p['items']['L3-1-hkdf-sha256']
        self.assertFalse(core.judge(p)['pass'])
        p = probe()
        p['done'] = False
        self.assertIn('done', core.judge(p)['error'])
        self.assertFalse(core.judge({'format': 'x'})['pass'])
        self.assertFalse(core.judge(None)['pass'])

    def test_display_mode_must_be_browser(self):
        v = core.judge(probe(**{'L3-4-display-mode': {'result': 'ok', 'detail': 'standalone'}}))
        self.assertFalse(v['pass'])
        self.assertEqual(v['groups']['4']['verdict'], 'ng')

    def test_judge_command_reads_sim_record_and_page_json(self):
        with tempfile.TemporaryDirectory() as d:
            for name, doc, code in (('rec.json', {'probe': probe()}, 0), ('page.json', probe(L3_2_get={'result': 'ng', 'detail': 'x'}), 1)):
                f = pathlib.Path(d) / name
                f.write_text(json.dumps(doc), encoding='utf-8')
                with contextlib.redirect_stdout(io.StringIO()) as out:
                    self.assertEqual(cli.main(['judge', str(f)]), code)
                self.assertIn('判定', out.getvalue())


class TestPageAndTool(unittest.TestCase):
    """ページ（web/probe/）と道具が同じ項目の名前と要素を使う。"""

    @classmethod
    def setUpClass(cls):
        cls.js = (PAGE / 'probe.js').read_text(encoding='utf-8')
        cls.html = (PAGE / 'index.html').read_text(encoding='utf-8')

    def test_every_judged_item_is_written_by_the_page(self):
        # AES-GCM の 2 項目はページが鍵の長さ 128 と 256 から名前を作る
        generated = {'L3-1-aes128gcm', 'L3-1-aes256gcm'}
        self.assertIn('`L3-1-aes${bits}gcm`', self.js)
        self.assertIn('[[128, ', self.js)
        self.assertIn('[256, ', self.js)
        for ids in core.ITEMS.values():
            for i in set(ids) - generated:
                self.assertIn(f"'{i}'", self.js, i)
        self.assertIn(f"format: '{core.FORMAT}'", self.js)
        self.assertIn(f"const PRESSED = '{core.PRESSED}'", self.js)

    def test_page_elements_exist(self):
        ids = set(re.findall(r'id="([^"]+)"', self.html))
        used = set(re.findall(r"\$\('([^'$]+)'\)", self.js)) | set(re.findall(r"bind\('([^']+)'", self.js))
        self.assertEqual(used - ids, set())
        for css in ('#b-pk-create', '#b-pk-get'):  # 道具が WebDriver で押す
            self.assertIn(css[1:], ids)
        for g in ('L3-1', 'L3-2', 'L3-3', 'L3-4'):
            self.assertIn(f't-{g}', ids)

    def test_manifest_scope_is_relative(self):
        m = json.loads((PAGE / 'manifest.webmanifest').read_text(encoding='utf-8'))
        self.assertEqual((m['start_url'], m['scope'], m['display']), ('./', './', 'standalone'))
        for icon in m['icons']:
            self.assertTrue((PAGE / icon['src']).is_file(), icon['src'])

    def test_page_does_not_carry_the_system_key_vectors(self):
        # 本システムの鍵の形は web-crypto の手順 9 まで入れない（ページ web-crypto の `web/probe/` の行）。第12章 5.3 の指紋と user_sig の値
        for s in ('11811 22211 31855 63267', 'ft.device_fingerprint/1', 'ft.device_cert/1', 'NQmuB28fr3taQAYp'):
            self.assertNotIn(s, self.js)


class TestRuntimeAndHost(unittest.TestCase):
    LISTING = {'runtimes': [
        {'name': 'iOS 26.2', 'platform': 'iOS', 'version': '26.2', 'identifier': 'rt.iOS-26-2', 'isAvailable': True, 'supportedDeviceTypes': []},
        {'name': 'iOS 18.6', 'platform': 'iOS', 'version': '18.6', 'identifier': 'rt.iOS-18-6', 'isAvailable': True, 'supportedDeviceTypes': [
            {'name': 'iPhone SE (3rd generation)', 'identifier': 'se3', 'productFamily': 'iPhone'},
            {'name': 'iPhone 16 Pro', 'identifier': '16pro', 'productFamily': 'iPhone'},
            {'name': 'iPhone 16', 'identifier': '16', 'productFamily': 'iPhone'},
            {'name': 'iPad (A16)', 'identifier': 'ipad', 'productFamily': 'iPad'}]},
        {'name': 'iOS 18.5', 'version': '18.5', 'identifier': 'rt.iOS-18-5', 'isAvailable': True},
        {'name': 'iOS 26.5', 'platform': 'iOS', 'version': '26.5', 'identifier': 'rt.iOS-26-5', 'isAvailable': False},
        {'name': 'tvOS 26.2', 'platform': 'tvOS', 'version': '26.2', 'identifier': 'rt.tvOS', 'isAvailable': True},
    ]}

    def test_pick_runtime(self):
        self.assertEqual(core.pick_runtime(self.LISTING, 'latest')[0], '26.2')  # 使えない 26.5 と tvOS は選ばない
        self.assertEqual(core.pick_runtime(self.LISTING, '18-latest')[0], '18.6')
        self.assertEqual(core.pick_runtime(self.LISTING, '18.5')[1], 'rt.iOS-18-5')
        with self.assertRaisesRegex(LookupError, '18.0'):
            core.pick_runtime(self.LISTING, '18.0')
        listing = {'runtimes': [{'name': 'iOS 18.0', 'version': '18.0.1', 'identifier': 'x', 'isAvailable': True}]}
        self.assertEqual(core.pick_runtime(listing, '18.0')[0], '18.0.1')

    def test_pick_device_type(self):
        types = core.pick_runtime(self.LISTING, '18-latest')[2]
        self.assertEqual(core.pick_device_type(types)['identifier'], '16')
        self.assertEqual(core.pick_device_type([{'name': 'iPhone SE (3rd generation)', 'productFamily': 'iPhone', 'identifier': 'se'}])['identifier'], 'se')
        with self.assertRaises(LookupError):
            core.pick_device_type([{'name': 'iPad', 'productFamily': 'iPad'}])

    def test_hosts_to_try(self):
        self.assertEqual(core.hosts_to_try(lambda n: '127.0.0.1'), [core.PROBE_HOST, 'localhost'])
        self.assertEqual(core.hosts_to_try(lambda n: '192.0.2.1'), ['localhost'])

        def unresolved(n):
            raise OSError('名前が無い')
        self.assertEqual(core.hosts_to_try(unresolved), ['localhost'])


class TestRetries(unittest.TestCase):
    def test_counts_the_first_try_and_the_retries(self):
        calls, sleeps = [], []

        def flaky():
            calls.append(1)
            if len(calls) < 3:
                raise RuntimeError('Unable to connect to simulator.')
            return 'ok'
        self.assertEqual(core.with_retries(flaky, core.DOWNLOAD_RETRIES, wait=30, sleep=sleeps.append), ('ok', 3))
        self.assertEqual(sleeps, [30, 30])

    def test_gives_up_after_the_last_retry(self):
        calls = []

        def always():
            calls.append(1)
            raise RuntimeError('つながらない')
        with self.assertRaises(RuntimeError) as cm:
            core.with_retries(always, core.DOWNLOAD_RETRIES, sleep=lambda s: None)
        self.assertEqual(len(calls), 4)  # 最初の 1 回と 3 回の再試行（第14章 11.6）
        self.assertEqual(cm.exception.tries, 4)
        calls.clear()
        with self.assertRaises(RuntimeError):
            core.with_retries(always, core.SESSION_RETRIES, sleep=lambda s: None)
        self.assertEqual(len(calls), 3)  # セッションの張り直しは 2 回まで


class TestPushPayloads(unittest.TestCase):
    """送る中身は本システムの通知の形（第2章 7、第7章 5.1、7）。api/schemas で検証する。"""

    @classmethod
    def setUpClass(cls):
        reg = Registry()
        for f in glob.glob(str(SCHEMAS / '**' / '*.json'), recursive=True):
            reg = reg.with_resource(pathlib.Path(f).as_uri(), Resource.from_contents(json.loads(pathlib.Path(f).read_text(encoding='utf-8'))))
        cls.reg = reg

    def valid(self, name, doc):
        v = Draft202012Validator({'$ref': (SCHEMAS / name / '1.json').as_uri()}, registry=self.reg)
        return [e.message for e in v.iter_errors(doc)]

    # R: R-7-1-1
    def test_messages_follow_the_schemas(self):
        site = 'https://example.invalid/family-talk-pwa/'
        for item in ('b', 'd'):
            body, ttl, urgency, top = core.message(item, 3, 10, 'legacy', site)
            self.assertEqual(self.valid('ft.push', body['push']), [])
            self.assertEqual(body['ft_probe']['seq'], 3)
            self.assertEqual((ttl, urgency), (86400, 'normal'))
            body, _, _, _ = core.message(item, 3, 10, 'declarative', site)
            self.assertEqual(self.valid('ft.push.declarative', body), [])
            self.assertEqual(body['notification']['app_badge'], '3')
            self.assertTrue(body['mutable'] and body['notification']['mutable'])
            self.assertRegex(body['notification']['title'], r'確かめ \w \d+/\d+')  # sw.js が数を読む形
        self.assertIsNone(core.message('b', 1, 10, 'legacy', site)[3])
        self.assertEqual(core.message('d', 1, 10, 'legacy', site)[3], core.topic(f'room:{core.ROOM_ID}'))

    # R: R-7-4-1
    def test_calls_follow_chapter_7(self):
        site = 'https://example.invalid/'
        plan = core.schedule('e', 10, 6, 'both', site, call_id=core.uuid7())
        self.assertEqual([t for t, _, _ in plan], [0.0, 20.0, 40.0, 60.0])
        kinds = [p[0]['push']['kind'] for _, _, p in plan]
        self.assertEqual(kinds, ['call', 'call', 'call', 'call_end'])
        self.assertEqual(plan[-1][2][0]['push']['reason'], 'missed')
        for _, form, (body, ttl, urgency, top) in plan:
            self.assertEqual(self.valid('ft.push', body['push']), [])
            self.assertEqual((form, ttl, urgency), ('legacy', 60, 'high'))
            self.assertEqual(top, core.topic(f'call:{body["push"]["call_id"]}'))
        for _, _, (body, _, _, _) in core.schedule('e', 10, 6, 'declarative', site):
            self.assertEqual(self.valid('ft.push.declarative', body), [])

    def test_schedule_and_topic(self):
        plan = core.schedule('d', 10, 6, 'both', 'https://example.invalid/')
        self.assertEqual(len(plan), 20)
        self.assertEqual([f for _, f, _ in plan], ['legacy'] * 10 + ['declarative'] * 10)
        self.assertEqual(plan[-1][0], 114.0)
        t = core.topic('room:x')
        self.assertEqual(len(t), 32)
        self.assertRegex(t, r'^[A-Za-z0-9_-]{32}$')  # RFC 8030 5.4
        self.assertRegex(core.uuid7(), r'^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')
        with self.assertRaises(ValueError):
            core.schedule('x', 1, 1, 'both', 'https://example.invalid/')

    def test_send_dry_run_waits_by_the_schedule(self):
        with tempfile.TemporaryDirectory() as d:
            sub = pathlib.Path(d) / 'sub.json'
            sub.write_text(json.dumps({'subscription': {'endpoint': 'https://push.example.invalid/x', 'keys': {'p256dh': 'BA', 'auth': 'AA'}}, 'page': 'https://example.invalid/p/'}), encoding='utf-8')
            keys = pathlib.Path(d) / 'k.json'
            keys.write_text('{"public": "x", "jwk": {}}', encoding='utf-8')
            now = [0.0]
            sleeps = []

            def sleep(s):
                sleeps.append(round(s, 3))
                now[0] += s
            args = SimpleNamespace(sub=str(sub), keys=str(keys), item='e', count=10, interval=6, form='both', delay=5, dry_run=True)
            recs = push.send(args, lambda s: None, sleep=sleep, clock=lambda: now[0])
            self.assertEqual([r['kind'] for r in recs], ['call', 'call', 'call', 'call_end'])
            self.assertEqual(sleeps, [5, 20.0, 20.0, 20.0])
            self.assertTrue(all(r['status'] == 'dry-run' for r in recs))
            sub.write_text(json.dumps({'subscription': {'endpoint': 'x'}, 'page': 'http://x/'}), encoding='utf-8')
            with self.assertRaises(SystemExit):
                push.load_subscription(sub)


@unittest.skipUnless(shutil.which('node'), 'node が無い')
class TestWebPushSender(unittest.TestCase):
    # R: R-7-1-1
    def test_rfc8291_example(self):
        r = subprocess.run(['node', str(push.SENDER), 'self-test'], capture_output=True, text=True, check=False)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(json.loads(r.stdout), {'ok': True})

    def test_keys(self):
        with tempfile.TemporaryDirectory() as d:
            pub = push.keys(pathlib.Path(d) / 'v.json')
            self.assertEqual(len(pub), 87)
            self.assertEqual(push.keys(pathlib.Path(d) / 'v.json'), pub)  # 2 回目は作り直さない


@unittest.skipUnless(shutil.which('openssl'), 'openssl が無い')
class TestCertsAndServe(unittest.TestCase):
    def test_page_is_served_over_tls_with_the_fresh_ca(self):
        with tempfile.TemporaryDirectory() as d:
            ca, crt, key = sim.make_certs(d, [core.PROBE_HOST, 'localhost', '127.0.0.1'])
            text = subprocess.run(['openssl', 'x509', '-in', str(crt), '-noout', '-text'], capture_output=True, text=True, check=True).stdout
            self.assertIn(f'DNS:{core.PROBE_HOST}', text)
            self.assertIn('IP Address:127.0.0.1', text)
            self.assertIn('TLS Web Server Authentication', text)
            ctx = ssl.create_default_context(cafile=str(ca))
            with sim.serve(crt, key) as port:
                with urllib.request.urlopen(f'https://localhost:{port}/manifest.webmanifest', context=ctx, timeout=10) as r:
                    self.assertEqual(r.headers['Content-Type'], 'application/manifest+json')
                    self.assertEqual(r.headers['Cache-Control'], 'no-store')
                with urllib.request.urlopen(f'https://localhost:{port}/', context=ctx, timeout=10) as r:
                    self.assertIn(b'probe.js', r.read())


class TestCli(unittest.TestCase):
    def test_help(self):
        for argv in (['--help'], ['sim', '--help'], ['push', 'send', '--help']):
            with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit) as cm:
                cli.main(argv)
            self.assertEqual(cm.exception.code, 0)
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as cm:
            cli.main(['sim'])
        self.assertEqual(cm.exception.code, 2)


if __name__ == '__main__':
    unittest.main()
