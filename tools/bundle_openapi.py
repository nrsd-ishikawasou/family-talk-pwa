# SPDX-FileCopyrightText: 2026 株式会社流山ソフトウェア開発 (Nagareyama Software Development Co., Ltd.)
# SPDX-License-Identifier: Apache-2.0
"""api/openapi.yaml の外のファイルへの参照を components/schemas に名前つきで取り込む（基本設計書 第14章 8）。

oapi-codegen は外のファイルへの参照から型の名を作れず、oneOf の和の型が壊れる。そこで生成の前に束ねる。
名はディレクトリの名から決まった形で付ける（schemas/ft.event.mls/1.json を FtEventMls1）。
oapi-codegen は経路ごとの servers を読まず 1 つの BaseURL しか使わないので、各経路の path に
実際の前置き（経路の servers、無ければ全体の servers）を付けて絶対の path にし、全体の servers を / にする。
あわせて oapi-codegen の設定（static のタグと x-websocket の操作を除く）を書く。

使い方：python -m tools.bundle_openapi --out <dir>
  <dir>/openapi.bundled.yaml と <dir>/oapi-codegen.yaml を書く（中間の生成ファイル。コミットしない）。
"""
import argparse
import json
import pathlib
import re

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
API = ROOT / 'api'
METHODS = ('get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace')


def schema_name(path):
    """schemas/<dir>/<版>.json を、dir と版の語を大文字で始めてつないだ名にする。"""
    rel = path.relative_to(API / 'schemas')
    words = re.split(r'[._/-]', '/'.join(list(rel.parts[:-1]) + [rel.stem]))
    return ''.join(w[:1].upper() + w[1:] for w in words if w)


class Bundler:
    def __init__(self):
        self.names = {}
        self.schemas = {}

    def load(self, path):
        path = path.resolve()
        if path in self.names:
            return self.names[path]
        name = schema_name(path)
        if name in self.schemas or name in self.names.values():
            raise ValueError(f'名が重なる: {name}（{path}）')
        self.names[path] = name
        doc = json.loads(path.read_text(encoding='utf-8'))
        doc.pop('$schema', None)
        doc.pop('$id', None)
        self.schemas[name] = self.walk(doc, path.parent)
        return name

    def walk(self, node, base):
        if isinstance(node, dict):
            ref = node.get('$ref')
            if isinstance(ref, str) and not ref.startswith('#'):
                target = '#/components/schemas/' + self.load(base / ref)
                rest = {k: self.walk(v, base) for k, v in node.items() if k != '$ref'}
                if rest:
                    return {'allOf': [{'$ref': target}], **rest}
                return {'$ref': target}
            return {k: self.walk(v, base) for k, v in node.items()}
        if isinstance(node, list):
            return [self.walk(x, base) for x in node]
        return node


def server_prefix(servers, where):
    """servers の url（1 つだけ、/ で始まる相対の url）を、path の前に付ける形にする（/ は空）。"""
    if not isinstance(servers, list) or len(servers) != 1:
        raise ValueError(f'servers は 1 つだけ: {where}')
    url = servers[0].get('url', '')
    if not url.startswith('/') or '{' in url:
        raise ValueError(f'servers の url は / で始まる変数の無い相対の url: {where}')
    return url.rstrip('/')


def absolute_paths(spec):
    """各経路の path に実際の前置きを付け、経路と操作の servers を除き、全体の servers を / にする。"""
    default = server_prefix(spec.get('servers'), 'servers')
    paths = {}
    for path, item in spec.get('paths', {}).items():
        prefix = server_prefix(item.pop('servers'), path) if 'servers' in item else default
        for m in METHODS:
            if m in item and 'servers' in item[m]:
                raise ValueError(f'操作ごとの servers は扱わない: {m} {path}')
        full = prefix + path
        if full in paths:
            raise ValueError(f'前置きを付けた path が重なる: {full}')
        paths[full] = item
    spec['paths'] = paths
    spec['servers'] = [{'url': '/'}]
    return spec


def bundle(spec_text):
    spec = yaml.safe_load(spec_text)
    b = Bundler()
    spec = b.walk(spec, API)
    comps = spec.setdefault('components', {}).setdefault('schemas', {})
    for name, schema in b.schemas.items():
        if name in comps:
            raise ValueError(f'components/schemas に同じ名がある: {name}')
        comps[name] = schema
    return absolute_paths(spec)


def excluded_operations(spec):
    """x-websocket を持つ操作の operationId（strict の生成から除く。srv-boundary が直接受ける）。"""
    out = []
    for item in spec.get('paths', {}).values():
        for m, op in item.items():
            if m in METHODS and op.get('x-websocket'):
                out.append(op['operationId'])
    return sorted(out)


def oapi_config(spec):
    """oapi-codegen の設定。出力の道のりは命令の -o で渡す（置き場によって中身が変わらないように）。

    embedded-spec は使わない（読むのに第2の OpenAPI の部品が要る）。spec は genimpl が go:embed で取り込む。
    """
    return {
        'package': 'gen',
        'generate': {'std-http-server': True, 'strict-server': True, 'models': True},
        'output-options': {'exclude-tags': ['static'], 'exclude-operation-ids': excluded_operations(spec)},
    }


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--out', required=True)
    a = ap.parse_args(argv)
    out = pathlib.Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    spec = bundle((API / 'openapi.yaml').read_text(encoding='utf-8'))
    (out / 'openapi.bundled.yaml').write_text(
        yaml.safe_dump(spec, allow_unicode=True, sort_keys=False, width=1000), encoding='utf-8')
    (out / 'oapi-codegen.yaml').write_text(
        yaml.safe_dump(oapi_config(spec), allow_unicode=True, sort_keys=False), encoding='utf-8')
    print(f'書いた：{out}（openapi.bundled.yaml、oapi-codegen.yaml）')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
