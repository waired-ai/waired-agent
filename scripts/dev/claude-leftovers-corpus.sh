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
#   bash scripts/dev/claude-leftovers-corpus.sh awk         # Linux without python3
#
# The awk copy (waired-agent#1407) handles the managed file only, and only in
# the layout Go writes, so it is held to a different bar than the other two:
# see run_awk_corpus below. It runs once per awk found (mawk, gawk, busybox
# awk; CORPUS_AWKS="mawk gawk busybox" names them), because Debian, Ubuntu
# and minimal images do not ship the same one.
#
# UNINSTALL_SH / CORPUS override the files. Exit 0 when every case matches.
set -euo pipefail

TOOL="${1:?usage: claude-leftovers-corpus.sh python3|osascript|awk}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNINSTALL_SH="${UNINSTALL_SH:-$ROOT/packaging/install/uninstall.sh}"
CORPUS="${CORPUS:-$ROOT/packaging/install/testdata/claude-leftovers}"

command -v python3 >/dev/null 2>&1 || { echo "FAIL python3 is needed to compare results" >&2; exit 1; }

lift() {
  local body
  body="$(awk "/^${1}\\(\\) \\{\$/,/^\\}\$/" "$UNINSTALL_SH")"
  [ -n "$body" ] || { echo "FAIL $UNINSTALL_SH has no $1 to lift" >&2; exit 1; }
  eval "$body"
}

case "$TOOL" in
  python3)   fn=claude_leftovers_py ;;
  osascript) fn=claude_leftovers_jxa ;;
  awk)       fn=claude_leftovers_awk ;;
  *) echo "unknown tool: $TOOL" >&2; exit 2 ;;
esac
lift "$fn"
program="$("$fn")"

run_tool() {
  case "$TOOL" in
    python3)   python3 -I -c "$program" "$@" ;;
    osascript) osascript -l JavaScript -e "$program" "$@" ;;
  esac
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Managed cases the awk copy must answer "state unrecognised" for, byte for
# byte untouched: layouts Go never writes. Taking one off this list is how a
# wider awk copy would prove itself.
AWK_UNRECOGNISED="08-utf8-bom 22-compact-one-line 23-crlf-line-ends"

# run_awk_corpus runs every managed case through claude_leftovers_awk_sh with
# each awk. The awk copy removes the env settings RemoveWithOptions removes and
# leaves the hooks, so a case passes when:
#   - plan leaves the file alone, and unchanged/unrecognised leaves it alone;
#   - a changed file is JSON, keeps every top-level key but env exactly as it
#     was, keeps its mode, and its env is expected's env;
#   - removed/kept agree with the Python copy, and the hooks it names as left
#     are the ones the Python copy removes;
#   - a loopback ANTHROPIC_BASE_URL or waired/subagent is taken out unless the
#     case is on AWK_UNRECOGNISED, where the answer must be unrecognised.
run_awk_corpus() {
  lift claude_leftovers_awk_sh
  lift claude_leftovers_py
  local wrapper py impls impl shim ran=0 failed=0
  wrapper="$(claude_leftovers_awk_sh)"
  py="$(claude_leftovers_py)"
  impls="${CORPUS_AWKS:-}"
  if [ -z "$impls" ]; then
    for impl in mawk gawk busybox; do
      command -v "$impl" >/dev/null 2>&1 && impls="$impls $impl"
    done
  fi
  [ -n "${impls// /}" ] || { echo "FAIL no awk to run" >&2; exit 1; }
  for impl in $impls; do
    command -v "$impl" >/dev/null 2>&1 || { echo "FAIL $impl is not on PATH"; failed=$((failed + 1)); continue; }
    shim="$work/shim-$impl"
    mkdir -p "$shim"
    if [ "$impl" = busybox ]; then
      printf '#!/bin/sh\nexec busybox awk "$@"\n' >"$shim/awk"
    else
      printf '#!/bin/sh\nexec %s "$@"\n' "$impl" >"$shim/awk"
    fi
    chmod +x "$shim/awk"
    for case_dir in "$CORPUS/managed"/*/; do
      case_dir="${case_dir%/}"
      local base name target plan py_plan out verdict allow=0
      base="$(basename "$case_dir")"
      name="managed/$base ($impl)"
      ran=$((ran + 1))
      case " $AWK_UNRECOGNISED " in *" $base "*) allow=1 ;; esac
      target="$work/$impl-$base.json"
      cp "$case_dir/input.json" "$target"
      chmod 640 "$target"
      out=""
      if ! plan="$(PATH="$shim:$PATH" sh -c "$wrapper" sh "$program" plan managed "$target" 2>&1)"; then
        echo "FAIL $name -- plan failed: $plan"
        failed=$((failed + 1))
        continue
      fi
      if ! cmp -s "$case_dir/input.json" "$target"; then
        echo "FAIL $name -- plan changed the file"
        failed=$((failed + 1))
        continue
      fi
      if ! out="$(PATH="$shim:$PATH" sh -c "$wrapper" sh "$program" apply managed "$target" 2>&1)"; then
        echo "FAIL $name -- apply failed: $out"
        failed=$((failed + 1))
        continue
      fi
      py_plan="$(python3 -I -c "$py" plan managed "$case_dir/input.json" 2>&1)" || py_plan="state failed"
      if ! verdict="$(python3 - "$case_dir" "$target" "$allow" "$plan" "$py_plan" <<'PY'
import json, os, stat, sys
case, target, allow, plan, py_plan = sys.argv[1], sys.argv[2], sys.argv[3] == "1", sys.argv[4], sys.argv[5]
LOOPBACK = "http://127.0.0.1:"
def lines(text, prefix):
    return [l[len(prefix):] for l in text.splitlines() if l.startswith(prefix)]
def fail(msg):
    print(msg)
    sys.exit(0)
state = (lines(plan, "state ") or [""])[0]
raw_in = open(os.path.join(case, "input.json"), "rb").read()
exists = os.path.exists(target)
raw_out = open(target, "rb").read() if exists else None
if state not in ("unchanged", "unrecognised", "rewrite", "delete"):
    fail("unknown state %r in %r" % (state, plan))
if allow:
    if state != "unrecognised":
        fail("on AWK_UNRECOGNISED but answered %s: take it off the list" % state)
if state in ("unchanged", "unrecognised") and raw_out != raw_in:
    fail("state %s but the file changed" % state)
try:
    doc_in = json.loads(raw_in.decode("utf-8-sig"))
except ValueError:
    doc_in = None
if type(doc_in) != dict:
    if state not in ("unchanged", "unrecognised"):
        fail("edited a file that isn't a JSON object")
    print("ok")
    sys.exit(0)
env_in = doc_in.get("env")
must = (type(env_in) == dict and (
    (type(env_in.get("ANTHROPIC_BASE_URL")) == str and env_in["ANTHROPIC_BASE_URL"].startswith(LOOPBACK)) or
    env_in.get("CLAUDE_CODE_SUBAGENT_MODEL") == "waired/subagent"))
if must and not allow and state not in ("rewrite", "delete"):
    fail("left a setting that breaks Claude Code: state %s" % state)
if state == "unrecognised":
    print("ok")
    sys.exit(0)
if exists:
    try:
        doc_out = json.loads(raw_out.decode("utf-8"))
    except ValueError as e:
        fail("result is not JSON: %s" % e)
    if state == "rewrite" and stat.S_IMODE(os.stat(target).st_mode) != 0o640:
        fail("the rewrite changed the file mode to %o" % stat.S_IMODE(os.stat(target).st_mode))
else:
    doc_out = {}
if state == "delete" and exists:
    fail("state delete but the file is still there")
for k in set(doc_in) | set(doc_out):
    if k != "env" and doc_in.get(k) != doc_out.get(k):
        fail("changed top-level %s" % k)
if os.path.exists(os.path.join(case, "expected.absent")):
    want = {}
elif os.path.exists(os.path.join(case, "expected.unchanged")):
    want = doc_in
else:
    want = json.loads(open(os.path.join(case, "expected.json"), "rb").read().decode("utf-8"))
if doc_out.get("env") != want.get("env"):
    fail("env differs from expected: %s" % json.dumps(doc_out.get("env"), ensure_ascii=False))
py_removed = lines(py_plan, "removed ")
if lines(plan, "removed ") != [r for r in py_removed if not r.startswith("hooks.")]:
    fail("removed %s, the Python copy %s" % (lines(plan, "removed "), py_removed))
if lines(plan, "kept ") != lines(py_plan, "kept "):
    fail("kept %s, the Python copy %s" % (lines(plan, "kept "), lines(py_plan, "kept ")))
if sorted(lines(plan, "left ")) != sorted(r for r in py_removed if r.startswith("hooks.")):
    fail("left %s, the Python copy removes %s" % (lines(plan, "left "), py_removed))
print("ok")
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
  echo "claude-leftovers corpus (awk:$impls): $ran cases, $failed failed"
  [ "$failed" -eq 0 ]
}

if [ "$TOOL" = awk ]; then
  run_awk_corpus
  exit $?
fi

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
