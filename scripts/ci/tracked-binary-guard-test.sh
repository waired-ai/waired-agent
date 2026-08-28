#!/usr/bin/env bash
# Self-test for tracked-binary-guard.sh (#1099).
#
# Asserting only that the guard passes on today's tree would pass just as
# well if the guard had no rule at all — and that is not a hypothetical
# here: this guard exists because a check that looked green for 21 checks
# was not looking at the thing that was wrong. So each case below breaks
# one thing and requires the guard to notice.
#
# The first case reproduces the actual defect: an ELF in the repository
# root, with .gitignore missing that one name.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
guard="${root}/scripts/ci/tracked-binary-guard.sh"

fail=0
check() { # check <expected: pass|fail> <label> <mutation function|->
  local want="$1" label="$2" mutate="$3" got tmp
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" RETURN

  # A minimal tree with the shape the guard reads: a git index, cmd/
  # packages, and an ignore list that covers them.
  ( cd "${tmp}"
    git init -q .
    mkdir -p cmd/waired cmd/catalog-tool
    : > cmd/waired/main.go
    : > cmd/catalog-tool/main.go
    printf '/waired\n/waired.exe\n/catalog-tool\n/catalog-tool.exe\n' > .gitignore
    git add -A . >/dev/null
  )

  [ "${mutate}" = "-" ] || "${mutate}" "${tmp}"
  if (cd "${tmp}" && bash "${guard}" >/dev/null 2>&1); then got=pass; else got=fail; fi
  if [ "${got}" = "${want}" ]; then
    echo "ok   ${label} (${got})"
  else
    echo "FAIL ${label}: want ${want}, got ${got}"
    fail=1
  fi
}

# printf writes the magic bytes directly: committing a real compiler
# output would make this test depend on a toolchain.
stage_binary() { # stage_binary <tree> <name> <magic escape>
  ( cd "$1" && printf "$3" > "$2" && git add -f "$2" >/dev/null )
}

# The exact defect: an ELF tracked in the repository root.
add_tracked_elf()      { stage_binary "$1" catalog-tool '\x7fELF\x02\x01\x01\x00'; }
# Renaming it must not help — rule 1 is about content, not the path.
add_renamed_elf()      { stage_binary "$1" docs-helper  '\x7fELF\x02\x01\x01\x00'; }
# The other three images, so a Windows or macOS build is caught too.
add_tracked_pe()       { stage_binary "$1" waired.exe   'MZ\x90\x00'; }
add_tracked_macho()    { stage_binary "$1" waired-mac   '\xcf\xfa\xed\xfe'; }
add_tracked_macho_fat(){ stage_binary "$1" waired-univ  '\xca\xfe\xba\xbe'; }

# An untracked binary is somebody's working tree, not this guard's
# business — the ignore list is what keeps it that way.
add_untracked_elf() { ( cd "$1" && printf '\x7fELF\x02' > catalog-tool ); }

# The cause, in isolation: a new command whose outputs nobody ignored.
# This is what would have caught #1099 the day catalog-tool was added.
add_unignored_cmd() { mkdir -p "$1/cmd/newtool" && : > "$1/cmd/newtool/main.go"; }

# Half-covered is not covered: the cross-build spelling is how the same
# file arrives from a Windows runner.
drop_exe_entry()    { sed -i '/^\/catalog-tool\.exe$/d' "$1/.gitignore"; }

# The original list's failure mode, reproduced exactly: three of four.
drop_one_cmd_entry(){ sed -i '/^\/catalog-tool$/d' "$1/.gitignore"; }

# /waired must not be accepted as covering /waired-agent; the guard
# matches whole lines for this reason.
add_prefix_cmd()    { mkdir -p "$1/cmd/waired-agent" && : > "$1/cmd/waired-agent/main.go"; }

check pass "a clean tree"                        -
check fail "an ELF tracked in the repo root"     add_tracked_elf
check fail "the same ELF under another name"     add_renamed_elf
check fail "a tracked Windows PE"                add_tracked_pe
check fail "a tracked Mach-O"                    add_tracked_macho
check fail "a tracked Mach-O universal binary"   add_tracked_macho_fat
check pass "an UNtracked binary is not our business" add_untracked_elf
check fail "a new cmd/ package nobody ignored"   add_unignored_cmd
check fail "only the .exe spelling is missing"   drop_exe_entry
check fail "three of four commands listed"       drop_one_cmd_entry
check fail "a prefix entry does not cover a longer name" add_prefix_cmd

if [ "${fail}" -ne 0 ]; then
  echo "tracked-binary-guard-test: FAILED" >&2
  exit 1
fi
echo "tracked-binary-guard-test: all passed"
