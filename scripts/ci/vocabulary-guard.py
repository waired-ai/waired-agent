#!/usr/bin/env python3
"""Product-copy vocabulary guard.

Reads scripts/ci/vocabulary-rules.txt and fails when a string literal in the
CLI, the Waired app, the notice package or the management package's shared consent
copy (public_use.go) matches a banned pattern. Only string literals are read: comments,
identifiers and _test.go files are not, so a rule about what the product
prints cannot fire on a remark about it.

Why a guard and not a review note: the terminology audit (waired#1056) found
that a retired term re-enters through whichever surface nobody re-read,
and that a term's presence in the repo is taken as evidence it is fine. The
rules file cites the ruling for each pattern, so a hit points at the record
rather than at a reviewer's memory.

Exemptions are keyed at the site, not by position (CLAUDE.md §Test
discipline): `// vocab: <why>` on the same line excuses that line's hits,
and a marker on a line with no hit fails as a stale claim, so an exemption
cannot outlive the string it excused.

usage: vocabulary-guard.py [--rules FILE] [--root DIR] [DIR ...]
"""
import argparse
import os
import re
import sys

DEFAULT_SCOPES = ["cmd/waired", "internal/gui/tray", "internal/notice", "internal/management/public_use.go"]
MARKER = re.compile(r"//\s*vocab:\s*\S")


def literals(src):
    """Yield (line, text) for every string literal outside comments."""
    i, n, line = 0, len(src), 1
    while i < n:
        c = src[i]
        if c == "\n":
            line += 1
            i += 1
            continue
        if src.startswith("//", i):
            j = src.find("\n", i)
            i = n if j < 0 else j
            continue
        if src.startswith("/*", i):
            j = src.find("*/", i + 2)
            seg = src[i:j + 2] if j >= 0 else src[i:]
            line += seg.count("\n")
            i = n if j < 0 else j + 2
            continue
        if c == "`":
            j = src.find("`", i + 1)
            if j < 0:
                return
            text = src[i + 1:j]
            yield line, text
            line += text.count("\n")
            i = j + 1
            continue
        if c == '"':
            j = i + 1
            buf = []
            while j < n and src[j] != '"':
                if src[j] == "\\" and j + 1 < n:
                    buf.append({"n": "\n", "t": "\t", '"': '"', "\\": "\\"}.get(src[j + 1], "\\" + src[j + 1]))
                    j += 2
                    continue
                buf.append(src[j])
                j += 1
            yield line, "".join(buf)
            i = j + 1
            continue
        if c == "'":
            j = i + 1
            while j < n and src[j] != "'":
                j += 2 if src[j] == "\\" else 1
            i = j + 1
            continue
        i += 1


def load_rules(path):
    rules = []
    with open(path, encoding="utf-8") as f:
        for lineno, raw in enumerate(f, 1):
            s = raw.rstrip("\n")
            if not s.strip() or s.lstrip().startswith("#"):
                continue
            parts = s.split("\t")
            if len(parts) != 4:
                sys.exit(f"{path}:{lineno}: expected 4 tab-separated fields, got {len(parts)}")
            scope, pattern, allow, ruling = parts
            rules.append((scope, re.compile(pattern, re.I | re.M), None if allow == "-" else re.compile(allow, re.I), ruling.strip()))
    if not rules:
        sys.exit(f"{path}: no rules — the guard is not looking at what it thinks")
    return rules


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rules", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "vocabulary-rules.txt"))
    ap.add_argument("--root", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
    ap.add_argument("scopes", nargs="*", default=DEFAULT_SCOPES)
    args = ap.parse_args()
    root = os.path.abspath(args.root)
    rules = load_rules(args.rules)

    files = []
    for scope in args.scopes:
        d = os.path.join(root, scope)
        if os.path.isfile(d):
            # A single file: the one shared-copy file in a package whose
            # other literals are API error bodies, not product copy.
            files.append((os.path.dirname(scope), d))
            continue
        if not os.path.isdir(d):
            sys.exit(f"vocabulary-guard: {scope} is not a directory or file under {root}")
        for name in sorted(os.listdir(d)):
            if name.endswith(".go") and not name.endswith("_test.go"):
                files.append((scope, os.path.join(d, name)))
    if not files:
        sys.exit("vocabulary-guard: found no Go files to read — the guard is not looking at what it thinks")

    problems = []
    checked = 0
    for scope, path in files:
        src = open(path, encoding="utf-8").read()
        lines = src.split("\n")
        rel = os.path.relpath(path, root)
        hits_by_line = {}
        for line, text in literals(src):
            checked += 1
            for rscope, pat, allow, ruling in rules:
                if rscope != "*" and not rel.startswith(rscope.rstrip("/") + "/"):
                    continue
                m = pat.search(text)
                if not m:
                    continue
                if allow is not None and allow.search(text):
                    continue
                hits_by_line.setdefault(line, []).append((text, pat.pattern, ruling))
        for lineno, srcline in enumerate(lines, 1):
            marked = bool(MARKER.search(srcline))
            hits = hits_by_line.get(lineno, [])
            if hits and not marked:
                for text, pattern, ruling in hits:
                    shown = text.replace("\n", "\\n")
                    if len(shown) > 90:
                        shown = shown[:87] + "..."
                    problems.append(f'{rel}:{lineno}: "{shown}" matches /{pattern}/ — {ruling}')
            elif marked and not hits:
                problems.append(f"{rel}:{lineno}: `// vocab:` excuses nothing on this line (stale claim) — remove the marker")

    if problems:
        print("vocabulary-guard: product copy uses a retired or banned term:", file=sys.stderr)
        for p in problems:
            print("  " + p, file=sys.stderr)
        print("  Fix the string, or excuse this site with `// vocab: <why>` on the same line.", file=sys.stderr)
        return 1
    print(f"vocabulary-guard: ok ({checked} literals in {len(files)} files, {len(rules)} rules)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
