# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""config.yaml の項目の形と、各章の数値の表との一致を確かめる（試験設計書 T-L2-07。全体実装設計書 6.3）。

使い方：python -m tools.config_check
終了コード：0 一致、1 食い違いあり、2 入力が読めない。
"""
import argparse
import pathlib
import re
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
DESIGN = ROOT / 'docs' / 'jp' / 'design'
LAYERS = {'env', 'file', 'db', 'user', 'room_me', 'device', 'guardian'}
NUM = {'int', 'duration', 'bytes', 'percent'}

# 各章の数値の表の行（項目名）と config.yaml のキーの対応。None は他の表の参照の行（第11章 7.2 の注記）
TABLES = {
    ('04_基本設計書_トークとメッセージ.md', '## 7. '): {
        'テキストの上限': ['room.text_max'], '1 回の送信の添付': None, '編集の期限': ['room.edit_window'],
        '取消の期限（送信者）': ['room.retract_window'], 'アナウンス': ['room.announcements_max'],
        '投票の選択肢': ['room.poll_options_max'], '投票の締切の上限': ['room.poll_close_max'],
        'ノートの添付': ['room.note_attachments_max'], '予約送信の上限': ['room.schedule_horizon'],
        '入力中の timeout': ['room.typing_timeout'], '空のグループの削除': ['room.empty_group_delete_after'],
        'スタンプのパック': ['room.sticker_packs_max', 'room.stickers_per_pack'], 'グループの上限': ['room.group_members_max']},
    ('05_基本設計書_メディア.md', '## 9. '): {
        '1 回の送信の件数': ['media.items_per_send'], '写真の 1 件の上限（オリジナル）': ['media.photo_original_max'],
        '写真の画質の長辺': ['media.photo_long_edge_standard', 'media.photo_long_edge_high'], '画素数の上限': ['media.pixels_max'],
        '動画の長さ・大きさ': ['media.video_duration_max', 'media.video_size_max'], 'ファイルの大きさ': ['media.file_size_max'],
        '音声の長さ': ['media.audio_duration_max'],
        'アルバム': ['media.albums_per_room', 'media.album_items_max', 'media.album_items_per_add'],
        '端末のキャッシュ': ['media.device_cache'], '地図のタイルのキャッシュ': ['media.map_cache'],
        '添付の上限（サーバー）': ['media.server_quota'], '消す規則': ['media.retention_video_years', 'media.retention_original_photo_years']},
    ('06_基本設計書_通話.md', '## 8. '): {
        '呼び出し': ['calls.ring_timeout'], '映像の上限': ['calls.video_max'], '音声のみの上限': ['calls.audio_max'],
        '空の通話の終了': ['calls.empty_timeout'], '再接続': ['calls.reconnect_timeout'], 'SFU のトークン': ['calls.sfu_token_ttl'],
        'サイマルキャストの層': ['calls.simulcast_layers'], '高画質の送信': ['calls.high_quality'],
        'UDP の範囲': ['server.sfu_udp_start', 'server.sfu_udp_end'], 'TURN': ['server.turn_udp_port', 'server.turn_tls_port'],
        '鍵の猶予': ['calls.key_grace']},
    ('07_基本設計書_通知.md', '## 11. '): {
        'メッセージの TTL': ['push.message_ttl'], '着信の TTL': ['push.call_ttl'], '着信の繰り返し（iPhone）': ['push.call_repeat'],
        '再送': ['push.retry_backoff'], '受け口の保管': ['push.up_box_max'], '常時接続の ping': ['push.up_ping'],
        'ミュートの期間': ['push.mute_durations'], '一時停止': ['push.pause_options'], '復号の待ち（SW、シェル）': ['push.decrypt_timeout'],
        '本文の冒頭': ['push.preview_chars']},
}
UNIT = {'秒': 1, '分': 60, '時間': 3600, '日': 86400, '年': 31536000, 'MB': 1024 ** 2, 'GB': 1024 ** 3, '%': 1}


def nums(text):
    """表の文から (数, 単位) を順に取る。「1 パック」「1 回」「1 端末」は「〜あたり」で値ではない。"""
    out = []
    for m in re.finditer(r'(\d[\d,]*(?:\.\d+)?)\s*(億|万)?\s*(秒|分|時間|日|年|MB|GB|%|人|文字|件|個|p)?', text):
        if m.group(3) is None and re.match(r'\s*(パック|回|端末)', text[m.end():]):
            continue
        v = float(m.group(1).replace(',', ''))
        if m.group(2) == '億':
            v *= 1e8
        if m.group(2) == '万':
            v *= 1e4
        out.append((v * UNIT.get(m.group(3) or '', 1), m.group(3)))
    return out


def table(fname, head):
    text = (DESIGN / fname).read_text(encoding='utf-8')
    sec = text[text.index(head):]
    sec = sec[:sec.index('\n## ', 1)]
    rows = {}
    for line in sec.split('\n'):
        if line.startswith('|') and not line.startswith('|項目') and not re.match(r'^\|(\s*:?-{3,}:?\s*\|)+\s*$', line):
            cells = [c.strip() for c in line.strip('|').split('|')]
            rows[cells[0]] = cells[1:]
    return rows


def check_items(items):
    bad = []
    keys = [i['key'] for i in items]
    for k in {k for k in keys if keys.count(k) > 1}:
        bad.append(f'{k}：キーが重複')
    for i in items:
        k = i['key']
        if not set(i['layers']) <= LAYERS:
            bad.append(f'{k}：置ける場所に無い値 {sorted(set(i["layers"]) - LAYERS)}')
        if i.get('env') and not re.match(r'^FT_[A-Z0-9_]+$', i['env']):
            bad.append(f'{k}：環境変数の名前の形')
        if i.get('env') and 'env' not in i['layers']:
            bad.append(f'{k}：環境変数があるのに置ける場所に env が無い')
        if not i.get('chapter'):
            bad.append(f'{k}：出典の章が無い')
        r = i.get('range') or {}
        if 'min' in r and 'max' in r and r['min'] > r['max']:
            bad.append(f'{k}：下限が上限より大きい')
        d = i['default']
        if i['type'] in NUM and isinstance(d, (int, float)) and not isinstance(d, bool):
            if 'min' in r and d < r['min']:
                bad.append(f'{k}：既定値 {d} が下限 {r["min"]} より小さい')
            if 'max' in r and d > r['max']:
                bad.append(f'{k}：既定値 {d} が上限 {r["max"]} より大きい')
        if 'enum' in r and d is not None and d not in r['enum']:
            bad.append(f'{k}：既定値 {d!r} が列挙に無い')
    return bad


def check_tables(cfg):
    bad = []
    ok = 0
    for (fname, head), mapping in TABLES.items():
        rows = table(fname, head)
        if set(rows) - set(mapping):
            bad.append(f'{fname}：対応表に無い行 {sorted(set(rows) - set(mapping))}')
        if set(mapping) - set(rows):
            bad.append(f'{fname}：表に無い行の対応 {sorted(set(mapping) - set(rows))}')
        for name, keys in mapping.items():
            if name not in rows or keys is None:
                continue
            init = rows[name][0]
            rng = rows[name][1] if len(rows[name]) > 1 else ''
            who = rows[name][2] if len(rows[name]) > 2 else ''
            missing = [k for k in keys if k not in cfg]
            if missing:
                bad.append(f'{name}：config.yaml に無い {missing}')
                continue
            flat = []
            for k in keys:
                v = cfg[k]['default']
                flat += v if isinstance(v, list) else [v]
            want = [n for n, _ in nums(init)]
            have = [float(v) for v in flat if isinstance(v, (int, float)) and not isinstance(v, bool)]
            if have and want[:len(have)] != have:
                bad.append(f'{name}：初期値 {init!r} と既定値 {have} が違う')
            else:
                ok += 1
            if len(keys) == 1 and '〜' in rng and '第' not in rng:
                lo, _, hi = rng.partition('〜')
                r = cfg[keys[0]].get('range') or {}
                ln, hn = nums(lo), nums(hi)
                unit = (hn[0][1] if hn and hn[0][1] else None) or (nums(init)[0][1] if nums(init) else None)
                if ln and ln[0][1] is None and unit:
                    ln = [(ln[0][0] * UNIT.get(unit, 1), unit)]
                if hn and hn[0][1] is None and unit:
                    hn = [(hn[0][0] * UNIT.get(unit, 1), unit)]
                if ln and float(r.get('min', -1)) != ln[0][0]:
                    bad.append(f'{name}：範囲の下限 {lo!r} と min {r.get("min")} が違う')
                if hn and float(r.get('max', -1)) != hn[0][0]:
                    bad.append(f'{name}：範囲の上限 {hi!r} と max {r.get("max")} が違う')
                if not hn and r.get('max') is not None:
                    bad.append(f'{name}：範囲に上限が無いのに max {r.get("max")}')
            for k in keys:
                layers = set(cfg[k]['layers'])
                if (rng == '固定' or who == '—') and layers and who != '管理者（設置の道具）':
                    bad.append(f'{name}：固定・変更なしなのに {k} の置ける場所が {sorted(layers)}')
                if who == '管理者' and not (layers & {'db', 'env', 'file'}):
                    bad.append(f'{name}：管理者が変えるのに {k} の置ける場所が {sorted(layers)}')
                if who == '構成員' and not (layers & {'user', 'device'}):
                    bad.append(f'{name}：構成員が変えるのに {k} の置ける場所が {sorted(layers)}')
    return bad, ok


def main(argv=None):
    argparse.ArgumentParser(description=__doc__.split('\n')[0]).parse_args(argv)
    try:
        items = yaml.safe_load((ROOT / 'api' / 'config.yaml').read_text(encoding='utf-8'))['items']
    except (OSError, KeyError, yaml.YAMLError) as e:
        print(f'config.yaml が読めない：{e}')
        return 2
    cfg = {i['key']: i for i in items}
    bad = check_items(items)
    tbad, ok = check_tables(cfg)
    bad += tbad
    print(f'項目 {len(items)}、表と照らした行 {ok}')
    for b in bad:
        print('NG ' + b)
    return 1 if bad else 0


if __name__ == '__main__':
    sys.exit(main())
