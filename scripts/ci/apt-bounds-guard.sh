#!/usr/bin/env bash
# apt-bounds-guard.sh — the installers may run apt-get in exactly one place.
#
# apt waits for ever by default and nothing here bounded it. On 2026-08-19
# six CI jobs across four branches sat at install.sh's prerequisite step
# for 19-24 minutes and printed nothing — not one line from apt, because
# that call uses `-qq` (#893). The same code runs on a user's machine,
# where the symptom is an installer that appears to do nothing, for ever.
#
# Bounding it took two things, and the first alone was not enough: the
# apt options ($APT_BOUNDS) bound what apt knows it is doing — a
# connection that goes quiet, a lock somebody else holds — and a branch
# carrying only those stalled for 19 minutes exactly as before. So the
# calls also go under a wall clock (timeout "$APT_TIMEOUT"), which bounds
# the modes nobody has enumerated, including that one.
#
# Both live in one helper, apt_bounded, and this guard's rule is that
# nothing else runs apt-get. That is deliberately stricter than "every
# call carries the options": a rule about call sites is satisfied by
# copying a line and trimming it; a rule that there is only one call site
# is not.
#
# Naming apt-get is not running it. Three shapes do that and stay allowed:
# a message about what is happening (common_log / common_warn /
# common_die / echo / printf), a comment, and common_require_cmd, whose
# arguments are command names it looks for on PATH.
#
# Run from the repository root (CI does this in ci.yml's install-scripts
# job, beside autostart-exec-guard.sh).
set -euo pipefail

scripts="packaging/install/install.sh packaging/install/uninstall.sh"
rc=0

for f in ${scripts}; do
  [ -f "${f}" ] || { echo "::error::missing ${f}" >&2; exit 1; }

  for want in 'APT_BOUNDS=' 'APT_TIMEOUT=' 'apt_bounded() {'; do
    if ! grep -qF "${want}" "${f}"; then
      echo "::error file=${f}::no ${want} — apt-get here has to be bounded by both the options and the clock (#893)" >&2
      rc=1
    fi
  done

  # The clock is only a bound while the command can still be killed by it.
  # needrestart ships enabled on Ubuntu and prompts from an apt post-invoke
  # hook; `timeout` runs apt-get in its own process group, so that prompt's
  # read of the terminal raises SIGTTIN — which stops `timeout` too, and a
  # stopped timeout never fires. #1097 hung an update for ever that way,
  # with the packages already installed. Suspending needrestart is what
  # removes the prompt; DEBIAN_FRONTEND does not reach it.
  if ! grep -qF 'NEEDRESTART_SUSPEND=1' "${f}"; then
    echo "::error file=${f}::no NEEDRESTART_SUSPEND=1 — needrestart's prompt stops apt-get AND its timeout with SIGTTIN (#1097)" >&2
    rc=1
  fi

  invocations=0
  while IFS= read -r entry; do
    n="${entry%%:*}"
    line="${entry#*:}"

    # Drop a trailing comment, then skip lines that only mentioned it.
    stripped="${line%%#*}"
    case "${stripped}" in *apt-get*) ;; *) continue ;; esac

    case "${stripped}" in
      *common_log*|*common_warn*|*common_die*|*echo\ *|*printf\ *) continue ;;
      *common_require_cmd*) continue ;;
    esac

    invocations=$((invocations + 1))

    # The one permitted call carries both bounds. The escapes keep the
    # patterns literal: what is matched is the text in the installer, not
    # this script's expansion of it.
    case "${stripped}" in
      *"timeout \"\$APT_TIMEOUT\""*"apt-get \$APT_BOUNDS"*"</dev/null"*) ;;
      *"timeout \"\$APT_TIMEOUT\""*"apt-get \$APT_BOUNDS"*)
        echo "::error file=${f},line=${n}::apt-get without </dev/null; a read of the terminal must return EOF, not stop the process group (#1097)" >&2
        echo "    ${line}" >&2
        rc=1
        ;;
      *)
        echo "::error file=${f},line=${n}::apt-get outside apt_bounded; route it through that helper so a stall fails instead of hanging (#893)" >&2
        echo "    ${line}" >&2
        rc=1
        ;;
    esac
  done < <(grep -nE 'apt-get' "${f}")

  if [ "${invocations}" -ne 1 ]; then
    echo "::error file=${f}::found ${invocations} apt-get invocations, want exactly 1 (the one inside apt_bounded) (#893)" >&2
    rc=1
  fi
done

if [ "${rc}" -eq 0 ]; then
  echo "apt-bounds-guard: apt-get runs only through apt_bounded, in both installers"
fi
exit "${rc}"
