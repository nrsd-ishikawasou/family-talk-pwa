# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""iPhone の Safari の確かめ（基本設計書 第14章 11.6、全体実装設計書 6.3、試験設計書 T-L3-12、T-L6-09）。

macOS の実行環境で iOS の Simulator を起こし、確かめのページ（web/probe/）を HTTPS で配り、safaridriver で開いて
結果の JSON（window.__ft_probe）を読み、T-L3-12 の判定をする（副命令 sim）。iPhone の実機の測りの送信もする（副命令 push）。
Python 3.12 の標準ライブラリだけで書く（tools/requirements.txt に部品を足さない）。Web Push の暗号化は同じ場所の
webpush.mjs（Node の組み込みの暗号だけ）が行う。

本ファイルは副作用の無い部分（判定、版の選び方、名前の選び方、再試行の数え方、push の中身）を持つ。
"""
import base64
import hashlib
import os
import re
import time

FORMAT = 'ft.probe/1'
RESULTS = ('ok', 'ng', 'not_run')

# T-L3-12 の 4 項目と、ページ（web/probe/probe.js）の項目の名前
ITEMS = {
    '1': ['L3-1-ecdsa-generate', 'L3-1-ecdsa-sign-verify', 'L3-1-ecdsa-known-answer', 'L3-1-ecdh-generate', 'L3-1-ecdh-derive',
          'L3-1-ecdh-known-answer', 'L3-1-hkdf-sha256', 'L3-1-aes128gcm', 'L3-1-aes256gcm', 'L3-1-aes256gcm-generate',
          'L3-1-import-jwk', 'L3-1-export-raw', 'L3-1-export-spki', 'L3-1-export-jwk', 'L3-1-import-raw-spki',
          'L3-1-import-raw-compressed-rejected'],
    '2': ['L3-2-open', 'L3-2-reopen', 'L3-2-put', 'L3-2-get', 'L3-2-persist'],
    '3': ['L3-3-create', 'L3-3-get'],
    '4': ['L3-4-display-mode'],
}
NAMES = {'1': 'WebCrypto', '2': 'IndexedDB の鍵', '3': 'パスキー', '4': '表示の形'}

# 取得の再試行（第14章 11.6「3 回まで再試行」。最初の 1 回と再試行の 3 回で、試すのは最大 4 回）
DOWNLOAD_RETRIES = 3
# safaridriver のセッションの張り直し（1 回まで）。実行環境でセッションが途中で切れる報告がある（調査資料 準拠の調査 4）
SESSION_RETRIES = 1

# Simulator の Safari が実行環境の hosts を読むかは未確認（全体実装設計書 16）。読まなければ localhost に替える
PROBE_HOST = 'probe.family-talk.test'
FALLBACK_HOST = 'localhost'


def judge(probe):
    """ページの JSON を T-L3-12 の判定の欄で判定する。

    (1)(2) は全部 ok、(4) は display_mode が browser。(3) は create と get が ok なら ok、どちらかが ng なら ng、
    それ以外（NotAllowedError で生体認証を操作できなかった、押していない）は not_run で赤にしない。
    戻り値は {'pass': bool, 'groups': {番号: {'verdict', 'reasons'}}}。
    """
    groups = {}
    if not isinstance(probe, dict) or probe.get('format') != FORMAT:
        return {'pass': False, 'groups': {}, 'error': f'形が {FORMAT} でない'}
    items = probe.get('items') or {}
    if not probe.get('done'):
        return {'pass': False, 'groups': {}, 'error': 'ページの確かめが終わっていない（done が偽）'}
    for g, ids in ITEMS.items():
        reasons = []
        results = []
        for i in ids:
            it = items.get(i)
            r = it.get('result') if isinstance(it, dict) else None
            if r not in RESULTS:
                reasons.append(f'{i}：結果が無い')
                results.append('ng')
                continue
            results.append(r)
            if r != 'ok':
                reasons.append(f'{i}：{r}（{it.get("detail", "")}）')
        if g == '3':
            verdict = 'ng' if 'ng' in results else ('ok' if all(r == 'ok' for r in results) else 'not_run')
        elif g == '4':
            mode = (items.get('L3-4-display-mode') or {}).get('detail')
            verdict = 'ok' if results == ['ok'] and mode == 'browser' else 'ng'
            if mode != 'browser':
                reasons.append(f'display-mode が {mode}（Safari のタブは browser）')
        else:
            verdict = 'ok' if all(r == 'ok' for r in results) else 'ng'
        groups[g] = {'verdict': verdict, 'reasons': reasons}
    passed = all(groups[g]['verdict'] == 'ok' for g in ('1', '2', '4')) and groups['3']['verdict'] != 'ng'
    return {'pass': passed, 'groups': groups}


def summary(probe, verdict):
    """job の記録に残す 1 行ずつの文。"""
    lines = []
    env = probe.get('env') or {} if isinstance(probe, dict) else {}
    lines.append(f'iOS {env.get("ios_version")}  {env.get("origin")}  判定 {"可" if verdict["pass"] else "不可"}')
    if verdict.get('error'):
        lines.append(f'  {verdict["error"]}')
    for g, v in verdict['groups'].items():
        lines.append(f'  ({g}) {NAMES[g]}：{v["verdict"]}')
        lines.extend(f'      {r}' for r in v['reasons'])
    return lines


def version_key(v):
    return tuple(int(x) for x in re.findall(r'\d+', v))


def ios_runtimes(listing):
    """`xcrun simctl list runtimes -j` の JSON から、使える iOS の版を (版, 識別子, 端末の型の一覧) で返す。"""
    out = []
    for r in listing.get('runtimes', []):
        name = r.get('name', '')
        platform = r.get('platform') or name.split(' ')[0]
        if platform != 'iOS' or not r.get('isAvailable', False):
            continue
        out.append((r['version'], r['identifier'], r.get('supportedDeviceTypes', [])))
    return sorted(out, key=lambda t: version_key(t[0]))


def pick_runtime(listing, spec):
    """版の指定から runtime を選ぶ。latest は入っている最新、N-latest は N 系の最新、それ以外は版の完全な一致（18.0 は 18.0 か 18.0.x）。"""
    rs = ios_runtimes(listing)
    if spec == 'latest':
        cand = rs
    elif m := re.fullmatch(r'(\d+)-latest', spec):
        cand = [r for r in rs if version_key(r[0])[0] == int(m.group(1))]
    else:
        want = version_key(spec)
        cand = [r for r in rs if version_key(r[0])[:len(want)] == want]
    if not cand:
        have = ', '.join(r[0] for r in rs) or 'なし'
        raise LookupError(f'iOS の runtime {spec} が無い（入っている版：{have}）')
    return cand[-1]


def pick_device_type(device_types):
    """runtime が対応する iPhone の型のうち、番号の付いた型で最も新しい物（無ければ最初の iPhone）。"""
    phones = [d for d in device_types if d.get('productFamily') == 'iPhone' or d.get('name', '').startswith('iPhone')]
    if not phones:
        raise LookupError('この runtime に iPhone の型が無い')
    numbered = [d for d in phones if re.fullmatch(r'iPhone \d+', d.get('name', ''))]
    if numbered:
        return max(numbered, key=lambda d: version_key(d['name']))
    return phones[0]


def hosts_to_try(resolve):
    """ページを開く名前の順。PROBE_HOST が 127.0.0.1 に解ければ先に試し、駄目なら localhost。resolve は名前から IPv4 を返す関数。"""
    try:
        if resolve(PROBE_HOST) == '127.0.0.1':
            return [PROBE_HOST, FALLBACK_HOST]
    except OSError:
        pass
    return [FALLBACK_HOST]


def with_retries(fn, retries, wait=0.0, sleep=time.sleep, log=None):
    """fn を最初の 1 回と retries 回の再試行で呼ぶ。戻り値は (fn の値, 試した回数)。全部が失敗したら最後の例外に試した回数を付けて投げる。"""
    tries = 0
    while True:
        tries += 1
        try:
            return fn(), tries
        except Exception as e:  # noqa: BLE001  失敗の種類によらず数える
            if log:
                log(f'{tries} 回目：{e}')
            if tries > retries:
                e.tries = tries
                raise
            sleep(wait)


# ---------- iPhone の実機の測りの送信（T-L6-09 の (b)、(d)、(e)） ----------

ROOM_ID = '0199e1c2-0000-7000-8000-00000000a001'
SENDER_ID = '0199e1c2-0000-7000-8000-00000000a002'


def uuid7(now_ms=None, rand=os.urandom):
    """RFC 9562 5.7 の UUIDv7（Python 3.12 の uuid には無い）。"""
    ms = int(time.time() * 1000) if now_ms is None else now_ms
    b = bytearray(ms.to_bytes(6, 'big') + rand(10))
    b[6] = 0x70 | (b[6] & 0x0F)
    b[8] = 0x80 | (b[8] & 0x3F)
    h = b.hex()
    return f'{h[:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:]}'


def topic(tag):
    """第2章 DD-2-8 の Topic：tag の SHA-256 の先頭 24 バイトを base64url にした 32 文字。"""
    return base64.urlsafe_b64encode(hashlib.sha256(tag.encode()).digest()[:24]).decode().rstrip('=')


def message(item, seq, of, form, site):
    """(b)・(d) の 1 通。form は legacy か declarative。戻り値は (中身, TTL, Urgency, Topic)。

    中身の ft.push/1 と宣言型の包み（ft.push.declarative/1）は本システムの形のまま（試験が api/schemas で検証する）。
    旧来の形は、版の測りの番号を ft_probe に持たせて ft.push/1 を push に包む（旧来の形の中身は OS が読まない）。
    (b) は Topic を付けない：配信サービスが届く前の通知を置き換えると、鍵を読めたかの数え方が配信の数に混ざるため。
    """
    tag = f'room:{ROOM_ID}'
    push = {'v': 1, 'kind': 'message', 'event_id': uuid7(), 'room_id': ROOM_ID, 'sender_id': SENDER_ID}
    if form == 'legacy':
        body = {'ft_probe': {'item': item, 'seq': seq, 'of': of, 'tag': tag, 'badge': seq}, 'push': push}
    elif form == 'declarative':
        body = {'web_push': 8030, 'mutable': True, 'notification': {
            'title': f'確かめ {item} {seq}/{of}', 'body': '確かめさんからメッセージ', 'navigate': site + '?from=push',
            'tag': tag, 'silent': False, 'mutable': True, 'app_badge': str(seq), 'data': push}}
    else:
        raise ValueError(form)
    return body, 86400, 'normal', (None if item == 'b' else topic(tag))


def call(seq, of, call_id, form, site, end_reason=None):
    """(e) の着信の 1 回（kind call）か終わり（kind call_end）。第7章 DD-7-7：Urgency high、TTL 60、Topic は call:<call_id> から。"""
    tag = f'call:{call_id}'
    if end_reason:
        push = {'v': 1, 'kind': 'call_end', 'call_id': call_id, 'room_id': ROOM_ID, 'sender_id': SENDER_ID, 'reason': end_reason}
    else:
        push = {'v': 1, 'kind': 'call', 'call_id': call_id, 'room_id': ROOM_ID, 'sender_id': SENDER_ID, 'video': False}
    if form == 'legacy':
        body = {'ft_probe': {'item': 'e', 'seq': seq, 'of': of, 'tag': tag}, 'push': push}
    elif form == 'declarative':
        title = '確かめさんとの通話' if end_reason else f'確かめ e {seq}/{of}'
        body = {'web_push': 8030, 'mutable': True, 'notification': {
            'title': title, 'body': '応答するには開いてください', 'navigate': site + '?from=call', 'tag': tag,
            'silent': bool(end_reason), 'mutable': True, 'data': push}}
    else:
        raise ValueError(form)
    return body, 60, 'high', topic(tag)


def schedule(item, count, interval, form, site, call_id=None):
    """送る順と時刻（秒）。(b) と (d) は count 通を interval 秒おき。form が both なら旧来の形の後に宣言型。
    (e) は第7章 7 の 0・20・40 秒の 3 回と、60 秒の不在の call_end。"""
    out = []
    if item in ('b', 'd'):
        forms = ['legacy', 'declarative'] if form == 'both' else [form]
        t = 0.0
        for f in forms:
            for i in range(1, count + 1):
                out.append((t, f, message(item, i, count, f, site)))
                t += interval
        return out
    if item == 'e':
        cid = call_id or uuid7()
        f = 'legacy' if form == 'both' else form
        out = [(20.0 * (i - 1), f, call(i, 3, cid, f, site)) for i in (1, 2, 3)]
        out.append((60.0, f, call(4, 3, cid, f, site, end_reason='missed')))
        return out
    raise ValueError(item)
