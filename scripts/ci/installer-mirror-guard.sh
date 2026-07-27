#!/usr/bin/env bash
# installer-mirror-guard.sh — a PR that changes one half of an installer
# pair must change the other half, or say why not.
#
# Why this exists: CLAUDE.md §Cross-OS parity already says install.sh /
# uninstall.sh changes mirror to install.ps1 / uninstall.ps1 and vice
# versa. Nothing enforced it, and 48ce3ae ("one non-interactive
# contract, and the knobs each script lacked") is the commit that found
# the divergence AFTERWARDS and filled it in. Prose did not catch it; a
# check run does.
#
# One-line opt-out in the PR body, same shape as docs-surface-guard's:
#
#     mirror-not-needed: <reason>
#
# Plenty of changes genuinely apply to one OS — a launchd path, a UAC
# re-exec, an apt source. That is not a loophole; it is the record of a
# decision that was previously invisible.
#
# Structure: decide() takes the changed-file list on stdin and the PR
# body in the environment, and nothing else. The git plumbing is the
# thin wrapper below it. That is the seam rule in CLAUDE.md §Test
# discipline — the decision is what needs testing, so the decision is
# what takes the input.
#
# Environment (set by .github/workflows/mirror-guard.yml):
#   BASE_REF  — the PR's target branch name (e.g. `main`)
#   HEAD_SHA  — the PR head commit
#   PR_BODY   — the pull request body, verbatim
set -euo pipefail

# The pairs. Left and right must move together.
PAIRS=(
  "packaging/install/install.sh|packaging/install/install.ps1"
  "packaging/install/uninstall.sh|packaging/install/uninstall.ps1"
)

# decide reads the changed-file list on stdin and prints any violations.
# Exit status is 0 when the change is balanced or opted out, 1 otherwise.
decide() {
  local changed
  changed=$(cat)

  local touched_one=()
  local pair left right
  for pair in "${PAIRS[@]}"; do
    left="${pair%%|*}"
    right="${pair##*|}"
    local has_left=0 has_right=0
    printf '%s\n' "${changed}" | grep -qxF "${left}" && has_left=1
    printf '%s\n' "${changed}" | grep -qxF "${right}" && has_right=1
    if [ "${has_left}" -eq 1 ] && [ "${has_right}" -eq 0 ]; then
      touched_one+=("${left} changed, ${right} did not")
    elif [ "${has_right}" -eq 1 ] && [ "${has_left}" -eq 0 ]; then
      touched_one+=("${right} changed, ${left} did not")
    fi
  done

  if [ ${#touched_one[@]} -eq 0 ]; then
    echo "installer-mirror-guard: installer pairs are balanced"
    return 0
  fi

  # Body opt-out. Case-insensitive, must carry a non-empty reason. The
  # workflow listens for `edited`, so adding the line re-runs the check
  # without a push.
  local reason
  reason=$(printf '%s\n' "${PR_BODY:-}" \
    | tr -d '\r' \
    | grep -ioE '^[[:space:]]*mirror-not-needed:[[:space:]]*.+' \
    | head -1 \
    | sed -E 's/^[[:space:]]*[Mm][Ii][Rr][Rr][Oo][Rr]-[Nn][Oo][Tt]-[Nn][Ee][Ee][Dd][Ee][Dd]:[[:space:]]*//' \
    || true)

  if [ -n "${reason}" ]; then
    echo "installer-mirror-guard: opted out via PR body — ${reason}"
    return 0
  fi

  {
    echo "::error::This PR changes one half of an installer pair but not the other."
    printf '%s\n' "${touched_one[@]}" | sed 's/^/  /'
    cat <<'EOF'

CLAUDE.md §Cross-OS parity: install.sh / uninstall.sh changes mirror to
install.ps1 / uninstall.ps1, and vice versa. The two scripts are one
contract — the same flags, the same prompts, the same exit codes — and
they have drifted before (48ce3ae found it after the fact).

If the change really applies to one OS only — a launchd path, a UAC
re-exec, an apt source, a Windows service name — add a line to the PR
body and this check will pass:

  mirror-not-needed: launchd-only teardown, no Windows equivalent
EOF
  } >&2
  return 1
}

# Wrapper: everything below here is plumbing, and is not what the
# self-test exercises.
main() {
  local base_ref="${BASE_REF:-}" head_sha="${HEAD_SHA:-HEAD}"
  if [ -z "${base_ref}" ]; then
    echo "::error::BASE_REF is not set (this script expects the PR context)" >&2
    exit 1
  fi
  # Diff from the merge base, not the base branch tip: the tip moves
  # while a PR is open, and diffing against a moved tip would attribute
  # other people's merges to this PR.
  git fetch --no-tags --quiet origin "${base_ref}"
  local base
  base=$(git merge-base FETCH_HEAD "${head_sha}")
  git diff --name-only "${base}" "${head_sha}" | decide
}

# Sourced by the self-test (which calls decide directly), executed by CI.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
