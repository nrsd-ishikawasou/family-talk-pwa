# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""副命令 push の実行の部分（試験設計書 T-L6-09 の (b)、(d)、(e)）。開発機から OS の提供者の配信サービスへ外向きに送る。

何をいつ送るかは tools/ios_probe の schedule が決め、1 通の暗号化と送信は webpush.mjs（Node の組み込みの暗号）が行う。
"""
import json
import pathlib
import shutil
import subprocess
import time
from datetime import datetime, timezone
from urllib.parse import urlsplit

from tools import ios_probe as core

SENDER = pathlib.Path(__file__).with_name('webpush.mjs')


def node():
    n = shutil.which('node')
    if not n:
        raise SystemExit('node が無い（送信の暗号化は Node の組み込みの暗号で行う。開発機の Node 22 以上）')
    return n


def keys(path):
    """VAPID の鍵対を作って path に置く（既にあれば使う）。公開鍵（ページの欄に入れる値）を返す。"""
    path = pathlib.Path(path)
    if not path.exists():
        out = subprocess.run([node(), str(SENDER), 'keys'], check=True, capture_output=True, text=True).stdout
        path.write_text(out, encoding='utf-8')
        path.chmod(0o600)
    return json.loads(path.read_text(encoding='utf-8'))['public']


def load_subscription(path):
    """ページの「購読をコピー」の JSON（{subscription, page}）。page はページの URL で、宣言型の navigate と VAPID の sub に使う。"""
    d = json.loads(pathlib.Path(path).read_text(encoding='utf-8'))
    sub, page = d.get('subscription'), d.get('page')
    if not sub or not sub.get('endpoint') or not (sub.get('keys') or {}).get('p256dh') or not page:
        raise SystemExit('購読の JSON の形が違う（ページの「購読をコピー」の中身をそのまま保存する）')
    if urlsplit(page).scheme != 'https':
        raise SystemExit('ページの URL が https でない')
    return sub, page


def send(args, log, sleep=time.sleep, clock=time.monotonic):
    sub, page = load_subscription(args.sub)
    vapid = json.loads(pathlib.Path(args.keys).read_text(encoding='utf-8'))
    subject = '{0.scheme}://{0.netloc}'.format(urlsplit(page))
    plan = core.schedule(args.item, args.count, args.interval, args.form, page)
    records = []
    if args.delay:
        log(f'{args.delay} 秒待ってから送る')
        sleep(args.delay)
    start = clock()
    for t, form, (body, ttl, urgency, topic) in plan:
        wait = start + t - clock()
        if wait > 0:
            sleep(wait)
        payload = json.dumps(body, ensure_ascii=False)
        req = {'subscription': sub, 'payload': payload, 'ttl': ttl, 'urgency': urgency, 'topic': topic, 'vapid': vapid, 'subject': subject}
        rec = {'sent_at': datetime.now(timezone.utc).isoformat(timespec='milliseconds'), 'item': args.item, 'form': form,
               'kind': (body.get('push') or body.get('notification', {}).get('data', {})).get('kind'), 'bytes': len(payload.encode())}
        if args.dry_run:
            rec['payload'] = body
            rec['status'] = 'dry-run'
        else:
            r = subprocess.run([node(), str(SENDER), 'send'], input=json.dumps(req), capture_output=True, text=True)
            try:
                out = json.loads(r.stdout)
            except json.JSONDecodeError:
                out = {'status': None, 'body': (r.stderr or r.stdout)[-300:]}
            rec['status'] = out.get('status')
            if out.get('body'):
                rec['response'] = out['body']
        log(f'{rec["sent_at"]} {args.item} {form} {rec["kind"]} {rec["status"]}')
        records.append(rec)
    return records
