#!/usr/bin/env bash
# claude-leftovers-corpus.sh -- run packaging/install/testdata/claude-leftovers
# through uninstall.sh's own copy of the rules (waired-agent#1398).
#
# uninstall.sh carries the rules twice: as Python for Linux and as JavaScript
# for Automation for macOS, the one JSON-capable interpreter every Mac has.
# This lifts the chosen program out of the shipped file (a copy would test
# itself), runs `apply` on a scratch copy of every case, and compares the
# result with the case's expected file. claude-leftovers-corpus.ps1 does the
# same for uninstall.ps1, and scripts/install/claude_leftovers_corpus_test.go
# for the Go removers, so the four implementations answer to one set of files.
#
# The comparison itself is done by python3 on both OSes: it only has to parse
# two documents and compare them, which is not the code under test.
#
#   bash scripts/dev/claude-leftovers-corpus.sh python3     # Linux copy
#   bash scripts/dev/claude-leftovers-corpus.sh osascript   # macOS copy
#
# UNINSTALL_SH / CORPUS override the files. Exit 0 when every case matches.
set -euo pipefail

TOOL="${1:?usage: claude-leftovers-corpus.sh python3|osascript}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNINSTALL_SH="${UNINSTALL_SH:-$ROOT/packaging/install/uninstall.sh}"
CORPUS="${CORPUS:-$ROOT/packaging/install/testdata/claude-leftovers}"

command -v python3 >/dev/null 2>&1 || { echo "FAIL python3 is needed to compare results" >&2; exit 1; }

case "$TOOL" in
  python3)   fn=claude_leftovers_py ;;
  osascript) fn=claude_leftovers_jxa ;;
  *) echo "unknown tool: $TOOL" >&2; exit 2 ;;
esac
body="$(awk "/^${fn}\\(\\) \\{\$/,/^\\}\$/" "$UNINSTALL_SH")"
[ -n "$body" ] || { echo "FAIL $UNINSTALL_SH has no $fn to lift" >&2; exit 1; }
eval "$body"
program="$("$fn")"

run_tool() {
  case "$TOOL" in
    python3)   python3 -I -c "$program" "$@" ;;
    osascript) osascript -l JavaScript -e "$program" "$@" ;;
  esac
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

ran=0
failed=0
for kind_dir in managed:managed user-settings:user retired-cache:cache; do
  dir="${kind_dir%%:*}"
  kind="${kind_dir##*:}"
  empty_is_absent=0
  [ "$kind" = user ] && empty_is_absent=1
  for case_dir in "$CORPUS/$dir"/*/; do
    case_dir="${case_dir%/}"
    name="$dir/$(basename "$case_dir")"
    ran=$((ran + 1))
    target="$work/$(basename "$case_dir").json"
    cp "$case_dir/input.json" "$target"
    if ! out="$(run_tool apply "$kind" "$target" 2>&1)"; then
      echo "FAIL $name -- the program failed: $out"
      failed=$((failed + 1))
      continue
    fi
    if ! verdict="$(python3 - "$case_dir" "$target" "$empty_is_absent" <<'PY'
import json, os, sys
case, target, empty_is_absent = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
def load(p):
    with open(p, "rb") as f:
        return json.loads(f.read().decode("utf-8-sig"))
absent = not os.path.exists(target)
if not absent and empty_is_absent:
    try:
        absent = load(target) == {}
    except ValueError:
        pass
if os.path.exists(os.path.join(case, "expected.absent")):
    print("ok" if absent else "want the file removed")
elif os.path.exists(os.path.join(case, "expected.unchanged")):
    if absent:
        print("want the file untouched, it was removed")
    else:
        same = open(target, "rb").read() == open(os.path.join(case, "input.json"), "rb").read()
        print("ok" if same else "want the file byte-for-byte untouched")
else:
    if absent:
        print("want a rewrite, the file was removed")
    else:
        try:
            got = load(target)
        except ValueError as e:
            print("result is not JSON: %s" % e)
            sys.exit(0)
        want = load(os.path.join(case, "expected.json"))
        print("ok" if got == want else "result differs from expected.json: %s" % json.dumps(got, ensure_ascii=False))
PY
)"; then
      verdict="the comparison failed"
    fi
    if [ "$verdict" = ok ]; then
      echo "ok   $name"
    else
      echo "FAIL $name -- $verdict"
      failed=$((failed + 1))
    fi
  done
done
echo "claude-leftovers corpus ($TOOL): $ran cases, $failed failed"
[ "$failed" -eq 0 ]
