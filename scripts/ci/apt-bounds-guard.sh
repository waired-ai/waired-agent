#!/usr/bin/env bash
# apt-bounds-guard.sh — every apt-get the installers run must be bounded.
#
# apt waits for ever by default, and nothing in these scripts bounded it.
# On 2026-08-19 `apt-get update -qq` at the prerequisite step stalled and
# took its whole caller down: main's own routing-sentinel job was killed
# twice at the 25-minute ceiling with install.sh's "Installing apt
# prerequisites..." as the last line anything printed (#893). `-qq` is
# why it was silent. The same code runs on a user's machine, where the
# symptom is an installer that appears to do nothing, for ever.
#
# The fix is a $APT_BOUNDS option list at every call site. This guard is
# what keeps the NEXT apt-get from being added without it — the defect
# was never one bad call, it was that no call had a bound and nothing
# said they must.
#
# The rule, stated so it fails closed: on any non-comment line naming
# apt-get, either the line carries $APT_BOUNDS, or the line is printing a
# message rather than running anything (common_log / echo / printf).
# A new invocation written in a style this file has never seen is
# therefore flagged, not missed.
#
# Run from the repository root (CI does this in ci.yml's install-scripts
# job, beside autostart-exec-guard.sh).
set -euo pipefail

scripts="packaging/install/install.sh packaging/install/uninstall.sh"
rc=0

for f in ${scripts}; do
  [ -f "${f}" ] || { echo "::error::missing ${f}" >&2; exit 1; }

  # The bound has to exist before anything can carry it.
  if ! grep -qE '^APT_BOUNDS=' "${f}"; then
    echo "::error file=${f}::no APT_BOUNDS definition; every apt-get here must be bounded (#893)" >&2
    rc=1
    continue
  fi

  found=0
  while IFS= read -r entry; do
    n="${entry%%:*}"
    line="${entry#*:}"
    found=1

    # Naming apt-get is not calling it. Two shapes do that: a message
    # about what is about to happen, and common_require_cmd, whose
    # arguments are command NAMES it checks for on PATH.
    case "${line}" in
      *common_log*|*common_warn*|*common_die*|*echo\ *|*printf\ *) continue ;;
      *common_require_cmd*) continue ;;
    esac

    # The escape keeps the pattern literal: what is matched is the text
    # "$APT_BOUNDS" in the installer, not this script's expansion of it.
    case "${line}" in
      *"\$APT_BOUNDS"*) ;;
      *)
        echo "::error file=${f},line=${n}::unbounded apt-get; add \$APT_BOUNDS so a stalled mirror or a held dpkg lock fails instead of hanging (#893)" >&2
        echo "    ${line}" >&2
        rc=1
        ;;
    esac
    # Strip comments before matching so a commented example never counts.
  done < <(grep -nE 'apt-get' "${f}" | sed -E 's/(:[[:space:]]*)#.*/\1/' | grep -vE ':[[:space:]]*$')

  if [ "${found}" -eq 0 ]; then
    echo "::error file=${f}::no apt-get lines found; this guard is watching the wrong file (#893)" >&2
    rc=1
  fi
done

if [ "${rc}" -eq 0 ]; then
  echo "apt-bounds-guard: every apt-get in the installers is bounded"
fi
exit "${rc}"
