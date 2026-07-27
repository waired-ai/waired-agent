#!/usr/bin/env python3
"""Guard the decision log's shape and its supersede graph.

The log is one file per decision under docs/decisions/YYYYMMDD/HHMM-<slug>.md
(a single append-only decisions.md put every concurrent PR on the same
insertion point; see CLAUDE.md §Decision Log). This checks the parts a
reviewer cannot eyeball across 200+ files:

  * front-matter exists and carries a known `status`
  * front-matter `status` agrees with the `## Status` prose
  * `supersedes` / `superseded_by` point at files that exist
  * those links agree in both directions
  * `status: superseded` names what superseded it
  * filenames keep the YYYYMMDD/HHMM-<ascii-slug>.md shape
  * the monolithic docs/decisions.md is not reintroduced

Exits non-zero with one line per problem. stdlib only.
"""
import os
import re
import sys

ROOT = 'docs/decisions'
MONOLITH = 'docs/decisions.md'
STATUSES = {'accepted', 'superseded', 'rejected', 'deferred'}
NAME_RE = re.compile(r'^([0-2]\d[0-5]\d)-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$')
DAY_RE = re.compile(r'^\d{8}$')
LIST_KEYS = ('supersedes', 'superseded_by')

errors = []


def err(path, msg):
    errors.append(f'{path}: {msg}')


def parse_front_matter(path, text):
    """Return (dict, body). Only the tiny subset we emit is supported."""
    if not text.startswith('---\n'):
        err(path, 'front-matter がない（先頭が "---" ではない）')
        return None, text
    end = text.find('\n---\n', 3)
    if end == -1:
        err(path, 'front-matter が閉じていない')
        return None, text
    meta, body = {}, text[end + 5:]
    key = None
    for raw in text[4:end + 1].split('\n'):
        if not raw.strip():
            continue
        if raw.startswith('  - ') or raw.startswith('- '):
            if key is None:
                err(path, f'リスト項目の親キーがない: {raw.strip()}')
                continue
            meta.setdefault(key, []).append(raw.split('- ', 1)[1].strip())
            continue
        if ':' not in raw:
            err(path, f'解釈できない front-matter 行: {raw}')
            continue
        key, val = raw.split(':', 1)
        key, val = key.strip(), val.strip()
        if val:
            meta[key] = val
            key = None
        else:
            meta.setdefault(key, [])
    return meta, body


def main():
    if os.path.exists(MONOLITH):
        err(MONOLITH, '単一ファイルの決定ログは復活させない（CLAUDE.md §Decision Log）')
    if not os.path.isdir(ROOT):
        print(f'{ROOT} が無いので検査をスキップ')
        return 0

    docs = {}
    for day in sorted(os.listdir(ROOT)):
        daydir = os.path.join(ROOT, day)
        if not os.path.isdir(daydir):
            err(daydir, f'{ROOT}/ 直下は YYYYMMDD ディレクトリのみ')
            continue
        if not DAY_RE.match(day):
            err(daydir, 'ディレクトリ名が YYYYMMDD ではない')
            continue
        for name in sorted(os.listdir(daydir)):
            path = os.path.join(daydir, name)
            if not NAME_RE.match(name):
                err(path, 'ファイル名が HHMM-<ascii-kebab-slug>.md ではない')
                continue
            text = open(path, encoding='utf-8').read()
            meta, body = parse_front_matter(path, text)
            if meta is None:
                continue
            status = meta.get('status')
            if not isinstance(status, str) or status not in STATUSES:
                err(path, f'status が不正: {status!r}（{"/".join(sorted(STATUSES))}）')
                status = None
            m = re.search(r'^## Status\n\s*([A-Za-z]+)', body, re.M)
            if not m:
                err(path, '`## Status` セクションが無いか、状態語で始まっていない')
            elif status and m.group(1).lower() != status:
                err(path, f'front-matter status={status} と `## Status` '
                          f'"{m.group(1)}" が食い違う')
            for key in LIST_KEYS:
                if key in meta and not isinstance(meta[key], list):
                    err(path, f'{key} はリストで書く（"  - <path>"）')
                    meta[key] = [meta[key]]
            if status == 'superseded' and not meta.get('superseded_by'):
                err(path, 'status: superseded なのに superseded_by が無い')
            docs[path] = meta

    for path, meta in docs.items():
        for key in LIST_KEYS:
            for target in meta.get(key, []):
                if target not in docs:
                    err(path, f'{key} の参照先が存在しない: {target}')
                    continue
                other = docs[target]
                back = 'superseded_by' if key == 'supersedes' else 'supersedes'
                if path not in other.get(back, []):
                    err(path, f'{key}: {target} が片側だけ — '
                              f'{target} に "{back}: {path}" が要る')

    if errors:
        print(f'決定ログの検査で {len(errors)} 件の問題:', file=sys.stderr)
        for e in errors:
            print(f'  {e}', file=sys.stderr)
        return 1
    print(f'decision log OK: {len(docs)} 件')
    return 0


sys.exit(main())
