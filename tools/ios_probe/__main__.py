# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""iPhone の Safari の確かめ（基本設計書 第14章 11.6）。

使い方：
  python -m tools.ios_probe sim --runtime 18-latest [--download 18.0] [--xcode /Applications/Xcode_16.4.app] [--out FILE]
      macOS の実行環境で Simulator を起こし、確かめのページを開いて T-L3-12 を判定する（CI の job ios）
  python -m tools.ios_probe judge FILE
      保存した結果（sim の記録かページの JSON）を判定し直す
  python -m tools.ios_probe serve [--port N] [--ca-out FILE]
      開発機で確かめのページを HTTPS で配る（ブラウザで JSON が出るかを見る）
  python -m tools.ios_probe push keys --keys FILE
  python -m tools.ios_probe push send --keys FILE --sub FILE --item b|d|e [--count N] [--interval S] [--delay S] [--form both|legacy|declarative]
      iPhone の実機の測り（T-L6-09）の送信。手順は web/probe/README.md

終了コード：0 は可（緑）、1 は不可（赤）、2 は使い方の誤り。
"""
import argparse
import json
import pathlib
import sys

from tools import ios_probe as core


def log(s):
    print(s, file=sys.stderr, flush=True)


def write(path, rec):
    if path:
        pathlib.Path(path).write_text(json.dumps(rec, ensure_ascii=False, indent=1) + '\n', encoding='utf-8')


def parser():
    p = argparse.ArgumentParser(prog='python -m tools.ios_probe', description='iPhone の Safari の確かめ（基本設計書 第14章 11.6、試験設計書 T-L3-12、T-L6-09）')
    sub = p.add_subparsers(dest='cmd', required=True)
    s = sub.add_parser('sim', help='Simulator でページを開いて T-L3-12 を判定する（macOS の実行環境）')
    s.add_argument('--runtime', required=True, help='iOS の版：latest（入っている最新）、18-latest（18 系の最新）、18.0（その版）')
    s.add_argument('--download', metavar='版', help='先に xcodebuild -downloadPlatform で取得する版（例 18.0。3 回まで再試行）')
    s.add_argument('--xcode', help='使う Xcode（例 /Applications/Xcode_16.4.app。DEVELOPER_DIR に渡す）')
    s.add_argument('--port', type=int, default=0, help='ページを配る port（既定は空いている物）')
    s.add_argument('--pk-timeout', type=int, default=60, help='パスキーの操作を待つ秒（既定 60）')
    s.add_argument('--out', help='記録（JSON）の置き場')
    j = sub.add_parser('judge', help='保存した結果を判定し直す')
    j.add_argument('file')
    v = sub.add_parser('serve', help='開発機でページを HTTPS で配る')
    v.add_argument('--port', type=int, default=8443)
    v.add_argument('--ca-out', help='認証局の証明書（PEM）を書き出す置き場（ブラウザに信頼させる時）')
    ps = sub.add_parser('push', help='実機の測りの送信（T-L6-09）').add_subparsers(dest='push_cmd', required=True)
    k = ps.add_parser('keys', help='VAPID の鍵対を作り、公開鍵を出す')
    k.add_argument('--keys', required=True, help='鍵対の置き場（リポジトリの外に置く）')
    n = ps.add_parser('send', help='T-L6-09 の (b)、(d)、(e) の順で送る')
    n.add_argument('--keys', required=True)
    n.add_argument('--sub', required=True, help='ページの「購読をコピー」の中身を保存したファイル')
    n.add_argument('--item', required=True, choices=['b', 'd', 'e'])
    n.add_argument('--count', type=int, default=10, help='(b)、(d) の通数（既定 10）')
    n.add_argument('--interval', type=float, default=6.0, help='(b)、(d) の間隔の秒（既定 6）')
    n.add_argument('--delay', type=float, default=0.0, help='送り始めるまでの秒（(b) の 5 分後は 300）')
    n.add_argument('--form', default='both', choices=['both', 'legacy', 'declarative'], help='(e) の both は旧来の形')
    n.add_argument('--log', help='送った記録（JSON）の置き場')
    n.add_argument('--dry-run', action='store_true', help='送らずに中身を出す')
    return p


def main(argv=None):
    args = parser().parse_args(argv)
    if args.cmd == 'judge':
        d = json.loads(pathlib.Path(args.file).read_text(encoding='utf-8'))
        probe = d.get('probe', d) if isinstance(d, dict) else d
        verdict = core.judge(probe)
        os_version = ((d.get('simulator') or {}).get('version') if isinstance(d, dict) else None)
        for line in core.summary(probe if isinstance(probe, dict) else {}, verdict, os_version):
            print(line)
        return 0 if verdict['pass'] else 1
    if args.cmd == 'sim':
        from tools.ios_probe import sim
        try:
            rec = sim.sim(args, log)
        except Exception as e:  # noqa: BLE001  記録を残してから赤にする
            rec = {'error': f'{type(e).__name__}: {e}', 'tries': getattr(e, 'tries', None)}
            write(args.out, rec)
            log(f'失敗：{rec["error"]}')
            return 1
        write(args.out, rec)
        for line in core.summary(rec['probe'] or {}, rec['verdict'], rec['simulator']['version']):
            print(line)
        print(f'  名前 {rec["host"]}（試した順 {rec["hosts_tried"]}）、セッション {rec["session_tries"]} 回、取得 {rec["download"]}')
        return 0 if rec['verdict']['pass'] else 1
    if args.cmd == 'serve':
        from tools.ios_probe import sim
        sim.serve_only(args, log)
        return 0
    from tools.ios_probe import push
    if args.push_cmd == 'keys':
        print(push.keys(args.keys))
        return 0
    records = push.send(args, log)
    write(args.log, records)
    if args.dry_run:
        print(json.dumps(records, ensure_ascii=False, indent=1))
    return 0 if all(r['status'] == 'dry-run' or (isinstance(r['status'], int) and 200 <= r['status'] < 300) for r in records) else 1


if __name__ == '__main__':
    sys.exit(main())
