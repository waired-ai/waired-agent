#!/usr/bin/env bash
# provider-fixture-ctx-guard.sh — a provider fixture's agent context must be
# cancellable (waired-agent#925).
#
# cmd/waired-agent's test fixtures hand-build an agentInferenceProvider whose
# state store lives under t.TempDir(). The provider detaches goroutines —
# endPull fires `go reconcileEngineServe`, a pull detaches the remeasure that
# ends at a benchmark, a vLLM swap detaches `go runEngineBootstrap` — and each
# of them writes the state dir through catalog's saveLocked, which re-creates
# the parent directory before writing it. A fixture that returns while one of
# those is still going has a write racing t.TempDir()'s removal:
#
#   testing.go: TempDir RemoveAll cleanup: ... directory not empty
#
# Every assertion passes, only the cleanup fails, it is green on a rerun, and
# it appears on the windows and seeded-host legs and never on linux.
#
# WHY A GUARD, and not a third round of fixing the fixture that failed. The
# rule has been written down twice and forgotten twice: hostCutoffProvider
# wrote the whole explanation for itself while the bootstrap fixture went on
# racing (#926), then the helper both of those used went uncalled by
# probeOrderProvider, which failed the same way twice more (#1124, then
# 2026-09-06 and 2026-09-12). CLAUDE.md §"Test discipline" already says why:
# "an opt-in helper only protects the tests that remember to call it".
#
# THE RULE. context.Background() is the marker of the shape that cannot be
# fixed from outside: a fixture whose agent context cannot be cancelled has no
# way to tell its detached work to stop, so no cleanup can join it. Take the
# context from providerLifetime(t) instead — it hands back the state dir, a
# cancellable context and the arm that stops the engine, cancels, and waits —
# or, if this one really is deliberate, say so ON THE LINE:
#
#   agentCtx: context.Background(), // background: <why>
#
# (CLAUDE.md §"Test discipline": a guard's exemption is keyed at the site,
# #1103. The marker may also sit anywhere in the comment block directly
# above the line, since a reason worth giving rarely fits on one.)
#
# Run from the repository root (CI does this in ci.yml's lint job).
set -euo pipefail

pkg="cmd/waired-agent"
[ -d "${pkg}" ] || { echo "::error::missing ${pkg} (run from repo root)" >&2; exit 1; }

helper="providerLifetime"
fail=0

# Invariant 1: the helper exists and is actually used. A guard that only
# policed the spelling would stay green over a helper nobody calls, which is
# the exact failure it was written for.
if ! grep -rqE "^func ${helper}\(" "${pkg}"/*_test.go; then
  echo "::error::${helper} is not defined in ${pkg}; the rule below has nothing to point at." >&2
  fail=1
else
  # -o so a file with two callers counts twice, and so a single-file tree
  # (the self-test's) is counted the same way as the real one — grep -c
  # drops the filename prefix when there is only one file to read.
  armed="$(grep -rho "${helper}(t)" "${pkg}"/*_test.go | grep -c . || true)"
  if [ "${armed}" -lt 5 ]; then
    echo "::error::only ${armed} fixture(s) take their lifetime from ${helper}; expected at least 5." >&2
    echo "  Either the fixtures were converted back to hand-rolled contexts, or the helper" >&2
    echo "  changed shape and this guard stopped seeing its callers (waired-agent#925)." >&2
    fail=1
  fi
fi

# Invariant 2: no unexplained context.Background() as an agent context.
hits="$(grep -nE 'agentCtx:[[:space:]]*context\.Background\(\)|\.agentCtx[[:space:]]*=[[:space:]]*context\.Background\(\)' \
  "${pkg}"/*_test.go || true)"

while IFS= read -r hit; do
  [ -n "${hit}" ] || continue
  file="${hit%%:*}"
  rest="${hit#*:}"
  line="${rest%%:*}"
  text="${rest#*:}"

  # Keyed at the site: the marker is on the line itself, or anywhere in the
  # contiguous comment block directly above it — a reason worth giving is
  # usually longer than one line.
  case "${text}" in *"// background:"*) continue ;; esac
  above=$((line - 1))
  exempted=0
  while [ "${above}" -ge 1 ]; do
    prev="$(sed -n "${above}p" "${file}")"
    case "${prev}" in
      *"// background:"*) exempted=1; break ;;
      [[:space:]]*"//"*|"//"*) above=$((above - 1)) ;;
      *) break ;;
    esac
  done
  [ "${exempted}" -eq 0 ] || continue

  echo "::error file=${file},line=${line}::agentCtx set to context.Background() with no reason given." >&2
  echo "  A provider fixture's agent context must be cancellable and come from ${helper}(t):" >&2
  echo "  nothing can join work the fixture cannot cancel, and the state dir is removed under" >&2
  echo "  it (waired-agent#925). If this one is deliberate, say so on the line:" >&2
  echo "      // background: <why>" >&2
  fail=1
done <<EOF
${hits}
EOF

[ "${fail}" -eq 0 ] || exit 1

exempt="$(printf '%s\n' "${hits}" | grep -c . || true)"
echo "provider-fixture-ctx-guard: ok — ${armed} fixtures take their lifetime from ${helper}, ${exempt} explained exception(s)"
