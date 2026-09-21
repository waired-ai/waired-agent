#!/usr/bin/env python3
"""hostname-guard.py — no tracked file names one of our verification hosts.

This repository is public. CLAUDE.md already says so for notes ("no
secrets, real device identifiers, internal hostnames"), and yet the names
of the computers the product is verified on had spread into 136 tracked
files by 2026-09-22 — the published docs site and the installer included
(waired-ai/waired#1486). They got in the same way every time: a sentence
written right after a run on real hardware keeps the name the work used,
and the next writer sees names already in the tree and takes that as
permission.

The guard does not hold a list of names, because a list of the names
in a public repository would publish them. It matches the SHAPE the
fleet's names share instead — `sv-` followed by a word, or `pc-` followed
by two hyphenated words — which nothing else in the tree uses. Refer to a
host by its hardware and OS ("the Linux host with a 24 GB RTX PRO 4000"),
and use a made-up name for a device name in an example or a test fixture
(`rtx4000-linux`, `my-desktop`).

A line that really needs the shape (a locale code written in lower case,
say) is excused with a marker on that same line:

    `hostname-ok: <why this is not one of our hosts>`

written without the backticks: a marker inside an inline code span is
documentation of the marker, like this one, and is not read as one. A
marker with no reason, or one on a line with nothing to excuse, fails.

Usage (from the repository root; ci.yml's lint job):

    python3 scripts/ci/hostname-guard.py [--root DIR]
"""
from __future__ import annotations

import argparse
import re
import subprocess
import sys

# sv-<word>: any preceding word character rules it out (csv-file, jsv-x).
# pc-<word>-<word>: a preceding hyphen rules it out too, so the "pc" field
# of a target triple (x86_64-pc-windows-msvc) is not a host.
HOST_SHAPE = re.compile(r"(?<![\w])sv-[a-z0-9]+|(?<![\w-])pc-[a-z0-9]+-[a-z0-9]+")
# Spelled in two pieces so this line is not itself a marker.
MARKER = re.compile("hostname" r"-ok:(.*)")
CODE_SPAN = re.compile(r"`[^`]*`")


def tracked_files(root: str) -> list[str]:
    out = subprocess.run(
        ["git", "-C", root, "ls-files", "-z"],
        check=True,
        capture_output=True,
    ).stdout
    return [p for p in out.decode("utf-8", "surrogateescape").split("\0") if p]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--root", default=".", help="repository to scan (default: .)")
    args = ap.parse_args()

    files = tracked_files(args.root)
    if not files:
        print("hostname-guard: no tracked files found — not looking at what it thinks", file=sys.stderr)
        return 1

    problems: list[str] = []
    scanned = 0
    for rel in files:
        path = f"{args.root}/{rel}"
        try:
            with open(path, "rb") as fh:
                data = fh.read()
        except (FileNotFoundError, IsADirectoryError):
            continue  # deleted in the work tree, or a submodule
        if b"\0" in data[:8192]:
            continue  # binary
        scanned += 1
        text = data.decode("utf-8", "replace")
        for n, line in enumerate(text.splitlines(), 1):
            hits = HOST_SHAPE.findall(line)
            marker = MARKER.search(CODE_SPAN.sub("", line))
            if marker and not marker.group(1).strip(" \t*/->}#"):
                problems.append(f"{rel}:{n}: `hostname-ok:` needs a reason after the colon")
            elif marker and not hits:
                problems.append(f"{rel}:{n}: stale `hostname-ok:` marker — nothing on this line has the host-name shape")
            elif hits and not marker:
                names = ", ".join(sorted(set(hits)))
                problems.append(f"{rel}:{n}: {names}")

    if scanned == 0:
        print("hostname-guard: every tracked file was skipped — not looking at what it thinks", file=sys.stderr)
        return 1
    if problems:
        print("hostname-guard: these lines name what looks like one of our verification hosts.", file=sys.stderr)
        print("This repository is public. Describe the host by its hardware and OS instead", file=sys.stderr)
        print("(\"the Linux host with a 24 GB RTX PRO 4000\"), and use a made-up device name in", file=sys.stderr)
        print("examples and fixtures (`rtx4000-linux`, `my-desktop`). waired-ai/waired#1486.", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        return 1
    print(f"hostname-guard: {scanned} tracked text files, no host names")
    return 0


if __name__ == "__main__":
    sys.exit(main())
