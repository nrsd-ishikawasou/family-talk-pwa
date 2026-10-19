# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""副命令 sim の実行の部分（基本設計書 第14章 11.6 の手順 1〜5）。

自前の認証局と HTTPS の配信は開発機（Linux）でも動く（--serve-only）。Simulator と safaridriver は macOS の実行環境だけ。
"""
import contextlib
import functools
import http.server
import json
import os
import pathlib
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

from tools import ios_probe as core

ROOT = pathlib.Path(__file__).resolve().parents[2]
PAGE = ROOT / 'web' / 'probe'


# ---------- 自前の認証局とサーバーの証明書（走るたびに作る。秘密鍵は一時のディレクトリの外へ出さない） ----------

def make_certs(work, names):
    """認証局（PEM）と、names を SAN に持つサーバーの証明書を openssl で作る。

    iOS が TLS のサーバーの証明書に求める形に合わせる：SAN に名前、拡張鍵用途 serverAuth、有効期間 825 日以下。
    """
    work = pathlib.Path(work)
    ca_key, ca_crt = work / 'ca.key', work / 'ca.pem'
    key, csr, crt, ext = work / 'server.key', work / 'server.csr', work / 'server.pem', work / 'server.ext'
    run = functools.partial(subprocess.run, check=True, capture_output=True)
    ca_cnf = work / 'ca.cnf'
    # -addext は使わない（macOS の openssl の版で受けるかを確かめていない）。設定ファイルと -extfile はどちらの版も受ける
    ca_cnf.write_text('[req]\ndistinguished_name=dn\nx509_extensions=v3_ca\nprompt=no\n[dn]\nCN=ft-probe-ca\n'
                      '[v3_ca]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\n')
    run(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(ca_key)])
    run(['openssl', 'req', '-x509', '-new', '-key', str(ca_key), '-sha256', '-days', '2', '-config', str(ca_cnf), '-out', str(ca_crt)])
    run(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(key)])
    run(['openssl', 'req', '-new', '-key', str(key), '-subj', f'/CN={names[0]}', '-out', str(csr)])
    san = ','.join(f'IP:{n}' if n.replace('.', '').isdigit() else f'DNS:{n}' for n in names)
    ext.write_text(f'basicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName={san}\n')
    run(['openssl', 'x509', '-req', '-in', str(csr), '-CA', str(ca_crt), '-CAkey', str(ca_key), '-CAcreateserial',
         '-days', '2', '-sha256', '-extfile', str(ext), '-out', str(crt)])
    for p in (ca_key, key):
        p.chmod(0o600)
    return ca_crt, crt, key


# ---------- web/probe/ の HTTPS の配信 ----------

class _Handler(http.server.SimpleHTTPRequestHandler):
    extensions_map = {**http.server.SimpleHTTPRequestHandler.extensions_map, '.webmanifest': 'application/manifest+json', '.mjs': 'text/javascript', '.js': 'text/javascript'}

    def end_headers(self):
        self.send_header('Cache-Control', 'no-store')
        super().end_headers()

    def log_message(self, fmt, *args):
        pass


@contextlib.contextmanager
def serve(crt, key, port=0, directory=PAGE):
    """web/probe/ を HTTPS で配る。戻すのは実際の port。"""
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(str(crt), str(key))
    httpd = http.server.ThreadingHTTPServer(('127.0.0.1', port), functools.partial(_Handler, directory=str(directory)))
    httpd.socket = ctx.wrap_socket(httpd.socket, server_side=True)
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    try:
        yield httpd.server_address[1]
    finally:
        httpd.shutdown()
        httpd.server_close()


# ---------- Simulator（xcrun simctl） ----------

def xcrun(*args, env=None, check=True, timeout=600):
    return subprocess.run(['xcrun', *args], check=check, capture_output=True, text=True, env=env, timeout=timeout)


def download_runtime(version, env, log):
    """`xcodebuild -downloadPlatform iOS -buildVersion <版>`。先に simctl を 1 回呼ぶ（呼ばないと時々つながらない。調査資料 準拠の調査 4）。"""
    def once():
        xcrun('simctl', 'list', env=env, check=False)
        r = subprocess.run(['xcodebuild', '-downloadPlatform', 'iOS', '-buildVersion', version], capture_output=True, text=True, env=env, timeout=3600)
        if r.returncode != 0:
            raise RuntimeError((r.stderr or r.stdout).strip().splitlines()[-1] if (r.stderr or r.stdout).strip() else f'終了コード {r.returncode}')
        return True
    return core.with_retries(once, core.DOWNLOAD_RETRIES, wait=30, log=log)


def create_and_boot(runtime_spec, env, log):
    listing = json.loads(xcrun('simctl', 'list', 'runtimes', '-j', env=env).stdout)
    version, runtime_id, types = core.pick_runtime(listing, runtime_spec)
    dtype = core.pick_device_type(types)
    udid = xcrun('simctl', 'create', f'ft-probe-{version}', dtype['identifier'], runtime_id, env=env).stdout.strip()
    log(f'Simulator：iOS {version}、{dtype["name"]}、{udid}')
    xcrun('simctl', 'boot', udid, env=env)
    xcrun('simctl', 'bootstatus', udid, '-b', env=env, timeout=900)
    return {'version': version, 'runtime': runtime_id, 'device_type': dtype['name'], 'udid': udid}


def launch_safari(udid, env):
    """セッションの前に Simulator の Safari を起こしておく（起こした直後の Simulator では、safaridriver が Safari を見つけるまでに時間切れになることがある）。"""
    xcrun('simctl', 'launch', udid, 'com.apple.mobilesafari', env=env, check=False)
    time.sleep(15)


def enroll_biometrics(udid, env):
    """生体認証を登録済みにする。Apple の公開の文書に無い通知の名前で、効くかは未確認（全体実装設計書 16）。失敗しても続ける。"""
    r = xcrun('simctl', 'spawn', udid, 'notifyutil', '-s', 'com.apple.BiometricKit.enrollmentChanged', '1',
              '-p', 'com.apple.BiometricKit.enrollmentChanged', env=env, check=False)
    return r.returncode == 0


def biometric_match(udid, env):
    """顔と指の一致の通知を出す（未確認。効かなければパスキーは NotAllowedError で not_run になる）。"""
    for name in ('com.apple.BiometricKit_Sim.pearl.match', 'com.apple.BiometricKit_Sim.fingerTouch.match'):
        xcrun('simctl', 'spawn', udid, 'notifyutil', '-p', name, env=env, check=False)


# ---------- W3C WebDriver（safaridriver の HTTP） ----------

class WebDriverError(RuntimeError):
    pass


class WebDriver:
    def __init__(self, base):
        self.base = base.rstrip('/')
        self.session = None

    def call(self, method, path, body=None, timeout=300):
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, method=method, headers={'Content-Type': 'application/json'})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return json.loads(r.read() or b'{}').get('value')
        except urllib.error.HTTPError as e:
            v = json.loads(e.read() or b'{}').get('value') or {}
            raise WebDriverError(f'{v.get("error")}: {v.get("message")}') from None

    def new_session(self, udid):
        caps = {'capabilities': {'alwaysMatch': {'browserName': 'Safari', 'platformName': 'iOS', 'safari:useSimulator': True, 'safari:deviceUDID': udid}}}
        v = self.call('POST', '/session', caps, timeout=900)
        self.session = v['sessionId']
        return v.get('capabilities', {})

    def s(self, method, path, body=None, timeout=300):
        return self.call(method, f'/session/{self.session}{path}', body, timeout)

    def navigate(self, url):
        self.s('POST', '/url', {'url': url})

    def script(self, js):
        return self.s('POST', '/execute/sync', {'script': js, 'args': []})

    def click(self, css):
        el = self.s('POST', '/element', {'using': 'css selector', 'value': css})
        eid = next(iter(el.values()))
        self.s('POST', f'/element/{eid}/click', {})

    def quit(self):
        if self.session:
            with contextlib.suppress(Exception):
                self.call('DELETE', f'/session/{self.session}', timeout=60)
            self.session = None


def wait_for(fn, timeout, every=1.0):
    end = time.monotonic() + timeout
    while True:
        v = fn()
        if v:
            return v
        if time.monotonic() > end:
            return None
        time.sleep(every)


def _detail(wd, item):
    return ((read_probe(wd) or {}).get('items', {}).get(item) or {}).get('detail')


def read_probe(wd):
    return wd.script('return window.__ft_probe ? JSON.parse(JSON.stringify(window.__ft_probe)) : null')


@contextlib.contextmanager
def safaridriver(port):
    p = subprocess.Popen(['safaridriver', '--port', str(port)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        wait_for(lambda: _port_open(port), 30, 0.5)
        yield WebDriver(f'http://127.0.0.1:{port}')
    finally:
        p.terminate()
        with contextlib.suppress(subprocess.TimeoutExpired):
            p.wait(10)


def _port_open(port):
    with socket.socket() as s:
        s.settimeout(0.5)
        return s.connect_ex(('127.0.0.1', port)) == 0


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def run_page(wd, url, udid, env, pk_timeout_ms, log):
    """ページを 2 回開き（2 回目で IndexedDB の開き直しを見る）、パスキーのボタンを押して、2 回目の JSON を返す。"""
    sep = '&' if '?' in url else '?'
    wd.navigate(f'{url}{sep}pk_timeout={pk_timeout_ms}')
    if not wait_for(lambda: (read_probe(wd) or {}).get('done'), 120):
        raise WebDriverError('1 回目のページの確かめが 120 秒で終わらない')
    wd.navigate(f'{url}{sep}pk_timeout={pk_timeout_ms}&reopen=1')
    if not wait_for(lambda: (read_probe(wd) or {}).get('done'), 120):
        raise WebDriverError('2 回目のページの確かめが 120 秒で終わらない')
    for button, item in (('#b-pk-create', 'L3-3-create'), ('#b-pk-get', 'L3-3-get')):
        wd.click(button)
        if udid:
            time.sleep(3)
            biometric_match(udid, env)
        # 押した印（core.PRESSED）が付いたか、その後に結果が来たかを分けて記録する
        pressed = wait_for(lambda: _detail(wd, item) != 'ボタンを押していない', 15)
        done = pressed and wait_for(lambda: _detail(wd, item) not in ('ボタンを押していない', core.PRESSED), pk_timeout_ms / 1000 + 30)
        log(f'{item}：' + ('押せなかった（印が付かない）' if not pressed else f'{(read_probe(wd) or {}).get("items", {}).get(item)}' if done else '押したが応答が来ない'))
    return read_probe(wd)


def developer_env(xcode):
    env = dict(os.environ)
    if xcode:
        env['DEVELOPER_DIR'] = str(pathlib.Path(xcode) / 'Contents' / 'Developer') if xcode.endswith('.app') else xcode
    return env


def sim(args, log):
    """第14章 11.6 の手順 1〜5。戻り値は保存する記録（判定を含む）。"""
    env = developer_env(args.xcode)
    rec = {'runtime_spec': args.runtime, 'download': None, 'simulator': None, 'host': None, 'hosts_tried': [], 'session_tries': 0, 'probe': None, 'verdict': None}
    if args.download:
        _, tries = download_runtime(args.download, env, log)
        rec['download'] = {'version': args.download, 'tries': tries}
    device = create_and_boot(args.runtime, env, log)
    rec['simulator'] = device
    rec['biometric_enroll'] = enroll_biometrics(device['udid'], env)
    with tempfile.TemporaryDirectory() as work:
        ca, crt, key = make_certs(work, [core.PROBE_HOST, core.FALLBACK_HOST, '127.0.0.1'])
        xcrun('simctl', 'keychain', device['udid'], 'add-root-cert', str(ca), env=env)
        with serve(crt, key, args.port) as port, safaridriver(free_port()) as wd:
            def attempt():
                wd.quit()
                rec['session_tries'] += 1
                launch_safari(device['udid'], env)
                wd.new_session(device['udid'])
                last = None
                for host in core.hosts_to_try(socket.gethostbyname):
                    rec['hosts_tried'].append(host)
                    try:
                        probe = run_page(wd, f'https://{host}:{port}/', device['udid'], env, args.pk_timeout * 1000, log)
                        rec['host'] = host
                        return probe
                    except WebDriverError as e:
                        last = e
                        log(f'{host} で開けない：{e}')
                raise last
            try:
                probe, _ = core.with_retries(attempt, core.SESSION_RETRIES, wait=10, log=log)
            finally:
                wd.quit()
    rec['probe'] = probe
    rec['verdict'] = core.judge(probe)
    xcrun('simctl', 'shutdown', device['udid'], env=env, check=False)
    xcrun('simctl', 'delete', device['udid'], env=env, check=False)
    return rec


def serve_only(args, log):
    """開発機（Linux）でページを HTTPS で配るだけ（ブラウザで開いて JSON が出るかを見る。OS の挙動の合否は見ない）。"""
    if not shutil.which('openssl'):
        raise SystemExit('openssl が無い')
    with tempfile.TemporaryDirectory() as work:
        ca, crt, key = make_certs(work, ['localhost', '127.0.0.1'])
        dest = pathlib.Path(args.ca_out) if args.ca_out else None
        if dest:
            shutil.copy(ca, dest)
        with serve(crt, key, args.port) as port:
            log(f'https://localhost:{port}/ で配っている（認証局 {dest or "（書き出していない）"}）。止めるには Ctrl-C')
            with contextlib.suppress(KeyboardInterrupt):
                while True:
                    time.sleep(3600)
