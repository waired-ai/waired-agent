#!/usr/bin/env bash
# tracked-binary-guard.sh — no compiled binary is tracked, and every
# cmd/ package's stray build output is ignored.
#
# waired-ai/waired-agent#1099 committed `catalog-tool`, a 14,999,779-byte
# ELF, to the root of this PUBLIC repository. It survived review, `gofmt`,
# `go vet`, `golangci-lint`, gitleaks and 21 green checks, because nothing
# here has ever looked at what a tracked file IS — only at what it says.
#
# The cause was not carelessness at the keyboard, it was a hand-written
# list. .gitignore carried the stray `go build ./cmd/<x>` outputs for
# waired, waired-agent and waired-tray; cmd/ has four packages, and the
# fourth was the one that got built in the repository root. A list that
# has to be edited whenever a sibling directory appears will eventually
# not be.
#
# So this guard asserts two things:
#
#   1. No tracked file begins with an executable image's magic number
#      (ELF, PE/COFF, or either Mach-O byte order). This is the direct
#      rule, and it is about content: renaming the file, or adding it
#      under a path no glob covers, does not get past it.
#
#   2. Every directory under cmd/ has BOTH `/<name>` and `/<name>.exe` in
#      .gitignore. This is the rule that would have prevented it, and it
#      is derived from the tree rather than typed, so adding a fifth
#      command fails here until its outputs are ignored.
#
# Rule 2 is not redundant with rule 1. Rule 1 fires after the binary is
# already committed and needs a history-touching fix; rule 2 fires when
# the command is added, before anyone has run a bare `go build`.
#
# Run from the repository root (ci.yml's lint job).
set -euo pipefail

problems=()

# --- 1. no tracked file is an executable image -----------------------------
#
# Read four bytes rather than shelling out to file(1): file's output
# wording is not stable across distributions, and the magic numbers are.
#
#   7f 45 4c 46  ELF          (Linux)
#   4d 5a        PE/COFF      ("MZ", Windows .exe/.dll)
#   cf fa ed fe  Mach-O 64    (little-endian, arm64/amd64 macOS)
#   ce fa ed fe  Mach-O 32    (little-endian)
#   ca fe ba be  Mach-O fat   (big-endian universal binary)
#
# feedface/feedfacf (big-endian Mach-O) are not listed: no toolchain here
# produces them, and cafebabe already collides with Java class files,
# which is a collision worth having in this direction.
while IFS= read -r -d '' f; do
  [ -f "${f}" ] || continue
  magic="$(head -c 4 -- "${f}" 2>/dev/null | od -An -tx1 -v 2>/dev/null | tr -d ' \n')"
  case "${magic}" in
    7f454c46*|4d5a*|cffaedfe*|cefaedfe*|cafebabe*)
      problems+=("${f} is a compiled binary and is tracked (magic ${magic:0:8})")
      ;;
  esac
done < <(git ls-files -z)

# --- 2. every cmd/ package's root-level build output is ignored -------------
#
# `go build ./cmd/<x>` with no -o writes ./<x> — that is what happened.
# The .exe spelling matters because the same mistake on Windows, or a
# GOOS=windows cross-build, lands ./<x>.exe.
if [ ! -f .gitignore ]; then
  problems+=(".gitignore is missing; this guard cannot check the ignore list")
else
  for d in cmd/*/; do
    [ -d "${d}" ] || continue
    name="$(basename "${d}")"
    for want in "/${name}" "/${name}.exe"; do
      # Exact line match: a substring test would accept /waired for
      # /waired-agent and silently cover nothing.
      if ! grep -qxF -- "${want}" .gitignore; then
        problems+=("cmd/${name} builds to ${want} in the repo root, and .gitignore does not list ${want}")
      fi
    done
  done
fi

if [ "${#problems[@]}" -gt 0 ]; then
  printf 'tracked-binary-guard: %s\n' "${problems[@]}" >&2
  cat >&2 <<'MSG'

A compiled binary must never be tracked: this repository is public, the
file is opaque to review, and it is stale the moment it lands. Delete it
(git rm --cached <file>) and make sure .gitignore names it.
MSG
  exit 1
fi

echo "tracked-binary-guard: no tracked binaries; every cmd/ output is ignored"
