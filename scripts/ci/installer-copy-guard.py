#!/usr/bin/env python3
"""installer-copy-guard.py — keep the installers' printed text on the copy conventions.

Reads the strings the installers print (helper calls, heredoc and here-string
bodies, quoted prose in the Inno [Code] section and the Debian maintainer
scripts) and checks each against scripts/ci/installer-copy-rules.txt. The
rules are the ones the 2026-09-08 installer copy pass applied
(docs/decisions/20260908/*-installer-copy.md): no `please`, no `machine`, no
bare `tray`, contractions, one `Warning:` label from the helper, no shouting,
ASCII punctuation on the shell side, and so on.

Exempt a line with a same-line marker naming why:

    common_log "..."   # copy-ok: <why>
    Write-Host '...'   # copy-ok: <why>
    Log('...');        // copy-ok: <why>

A marker on a line the guard would not have flagged is itself an error, so
stale exemptions do not accumulate.

    python3 scripts/ci/installer-copy-guard.py            # the shipped files
    python3 scripts/ci/installer-copy-guard.py --root DIR --rules FILE FILE...

Exit 1 on any hit, 0 when clean.
"""
import argparse
import os
import re
import sys

DEFAULT_FILES = [
    "packaging/install/install.sh",
    "packaging/install/uninstall.sh",
    "packaging/install/install.ps1",
    "packaging/install/uninstall.ps1",
    "packaging/windows/waired-setup.iss",
    "packaging/debian/waired/postinst",
    "packaging/debian/waired/prerm",
    "packaging/debian/waired-tray/postinst",
    "packaging/debian/waired-tray/prerm",
]

SH_HELPERS = re.compile(r"^\s*(?:\$SUDO\s+)?(common_log|common_warn|common_die|section|_banner_row|printf|echo)\b")
PS_HELPERS = re.compile(r"^\s*(Common-Log|Common-Warn|Common-Die|Write-Host|Write-Warning|Section|Read-Host|Common-Run|Skip-Absent)\b")
MARKER = re.compile(r"(?:#|//|\{)\s*copy-ok:\s*\S")
WORD = re.compile(r"[A-Za-z]{2,}")


def lang_of(path):
    if path.endswith(".ps1"):
        return "ps"
    if path.endswith(".iss"):
        return "iss"
    return "sh"


def quoted(line, lang):
    """Return the quoted literals on a line (outer quotes stripped), in order."""
    out, i, n = [], 0, len(line)
    while i < n:
        c = line[i]
        if c == '"':
            j, buf = i + 1, []
            while j < n:
                if lang == "sh" and line[j] == "\\" and j + 1 < n:
                    buf.append(line[j + 1]); j += 2; continue
                if lang == "ps" and line[j] == "`" and j + 1 < n:
                    buf.append(line[j + 1]); j += 2; continue
                if line[j] == '"':
                    break
                buf.append(line[j]); j += 1
            out.append("".join(buf)); i = j + 1
        elif c == "'":
            j, buf = i + 1, []
            while j < n:
                if line[j] == "'":
                    if lang in ("ps", "iss") and j + 1 < n and line[j + 1] == "'":
                        buf.append("'"); j += 2; continue
                    if lang == "sh" and line[j:j + 4] == "'\\''":
                        buf.append("'"); j += 4; continue
                    break
                buf.append(line[j]); j += 1
            out.append("".join(buf)); i = j + 1
        elif c == "#" and lang != "iss" and (i == 0 or line[i - 1] in " \t"):
            break
        elif lang == "iss" and line.startswith("//", i):
            break
        else:
            i += 1
    return out


def strip_vars(s, lang):
    s = re.sub(r"\$\((?:[^()]|\([^()]*\))*\)", " ", s)
    s = re.sub(r"\$\{[^}]*\}", " ", s)
    if lang == "ps":
        s = re.sub(r"\$(?:script|env|global):[A-Za-z_][A-Za-z0-9_]*", " ", s)
    s = re.sub(r"\$[A-Za-z_][A-Za-z0-9_]*", " ", s)
    return s


def printed_text(path, lines):
    """Yield (line_no, text) for what the file prints."""
    lang = lang_of(path)
    if lang == "sh":
        heredoc = None
        for idx, line in enumerate(lines, 1):
            if heredoc:
                if line.strip() == heredoc:
                    heredoc = None
                    continue
                yield idx, strip_vars(line, lang)
                continue
            s = line.strip()
            if s.startswith("#"):
                continue
            m = re.search(r"<<-?\s*(['\"]?)(\w+)\1", line)
            if m:
                heredoc = m.group(2)
            if SH_HELPERS.match(line) or re.match(r"^\s*(?:local\s+)?[A-Za-z_][A-Za-z0-9_]*=['\"]", line):
                lits = [strip_vars(q, lang) for q in quoted(line, lang)]
                text = " ".join(l for l in lits if WORD.search(l))
                if text.strip():
                    yield idx, text
    elif lang == "ps":
        block = False
        here = None
        for idx, line in enumerate(lines, 1):
            if here:
                if line.startswith('"@') or line.startswith("'@"):
                    here = None
                    continue
                yield idx, strip_vars(line, lang)
                continue
            s = line.strip()
            if block:
                block = "#>" not in s
                continue
            if s.startswith("<#"):
                block = "#>" not in s
                continue
            if s.startswith("#"):
                continue
            if s.endswith('@"') or s.endswith("@'"):
                here = True
                continue
            lits = [strip_vars(q, lang) for q in quoted(line, lang)]
            text = " ".join(l for l in lits if len(WORD.findall(l)) >= 2)
            if text.strip():
                yield idx, text
    else:  # iss
        for idx, line in enumerate(lines, 1):
            s = line.strip()
            if s.startswith("//") or s.startswith(";"):
                continue
            lits = [q for q in quoted(line, lang) if len(WORD.findall(q)) >= 2]
            if lits:
                yield idx, " ".join(lits)


def load_rules(path):
    rules = []
    for ln, raw in enumerate(open(path, encoding="utf-8"), 1):
        line = raw.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        parts = line.split("\t")
        if len(parts) < 5:
            sys.exit(f"{path}:{ln}: expected 5 tab-separated fields (name, scope, pattern, allow, ruling)")
        name, scope, pattern, allow, ruling = parts[:5]
        try:
            rx = re.compile(pattern)
            ax = re.compile(allow) if allow.strip() else None
        except re.error as e:
            sys.exit(f"{path}:{ln}: bad regex: {e}")
        rules.append((name, set(scope.split(",")), rx, ax, ruling))
    return rules


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=os.path.join(os.path.dirname(__file__), "..", ".."))
    ap.add_argument("--rules", default=os.path.join(os.path.dirname(__file__), "installer-copy-rules.txt"))
    ap.add_argument("files", nargs="*")
    args = ap.parse_args()
    rules = load_rules(args.rules)
    files = args.files or DEFAULT_FILES
    hits = 0
    for rel in files:
        path = os.path.join(args.root, rel) if not os.path.isabs(rel) else rel
        if not os.path.exists(path):
            print(f"{rel}: missing", file=sys.stderr)
            hits += 1
            continue
        lang = lang_of(path)
        lines = open(path, encoding="utf-8", errors="replace").read().split("\n")
        for idx, text in printed_text(path, lines):
            line = lines[idx - 1]
            marker = bool(MARKER.search(line))
            found = []
            for name, scope, rx, ax, ruling in rules:
                if "all" not in scope and lang not in scope:
                    continue
                probe = ax.sub(" ", text) if ax else text
                m = rx.search(probe)
                if m:
                    found.append((name, m.group(0), ruling))
            if found and not marker:
                for name, what, ruling in found:
                    print(f"{rel}:{idx}: {name}: {what!r} in {text.strip()[:90]!r}  ({ruling})")
                    hits += 1
            elif marker and not found:
                print(f"{rel}:{idx}: stale copy-ok marker (nothing to exempt)")
                hits += 1
    if hits:
        print(f"installer-copy-guard: {hits} problem(s)", file=sys.stderr)
        return 1
    print("installer-copy-guard: OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
