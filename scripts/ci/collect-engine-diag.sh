#!/usr/bin/env bash
# collect-engine-diag.sh — stage the engine's own logs into ./diag for the
# workflow's upload-artifact step. Runs on all three CI legs: `shell: bash`
# is Git Bash on windows-latest, which the job-summary step in the same
# workflow already relies on.
#
# ONE script rather than a `run:` block per leg, because the defect this
# fixes is what having three of them produced. They were written at
# different times and each had forgotten something different: the Windows
# install+inference and routing-sentinel legs collected nothing at all, the
# Linux install+inference leg collected nothing, only the Windows
# daemon-path collector took the agent's own log, and every one of them
# missed engine.log.1 (waired-agent#1112).
#
# ENGINE.LOG.1 IS USUALLY THE INTERESTING ONE. openEngineLog renames
# engine.log -> engine.log.1 on every spawn (internal/runtime/ollama.go),
# and the adapter respawns a crashed engine automatically. So on the exact
# failure class these collectors exist for — an engine that crash-loops —
# the first and informative attempt has already been rotated into .1, and
# the engine.log left behind holds the last, least informative one.
# internal/platform/logdump is the one place in the product that gets this
# right; it takes *.log.1 before *.log, for both engines.
#
# Usage: collect-engine-diag.sh <state-dir> [extra-file ...]
#
# The state dir is passed in because it differs per OS and the workflow
# already knows which leg it is running. On Windows give it the Git Bash
# spelling (/c/ProgramData/waired).
set -uo pipefail

state_dir="${1:?usage: collect-engine-diag.sh <state-dir> [extra-file ...]}"
shift

# The Unix state dir is 0700 and owned by the service account, so traversal
# fails as the CI user even though the log itself is world-readable. Windows
# needs none of this: the runner is already Administrator and the DACL grants
# it Full Control by inheritance (internal/platform/secrets).
SUDO=""
if [ "$(id -u 2>/dev/null || echo 0)" != "0" ] && command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
fi

mkdir -p diag

# Both engines. vLLM is linux-only, so its directory is simply absent
# elsewhere and the loop skips it — no per-OS branch needed here.
for engine in ollama vllm; do
  for name in engine.log engine.log.1; do
    src="${state_dir}/runtimes/${engine}/logs/${name}"
    if $SUDO test -f "${src}"; then
      $SUDO cp "${src}" "diag/${engine}-${name}" || true
    fi
  done
done

# The Windows-only agent log file (waired-agent#636): the Event Log carries
# Warn and above only, so without this a Windows bundle has no agent records
# at all. Absent on the other two, where journalctl / the transcript answer.
if $SUDO test -f "${state_dir}/logs/waired-agent.log"; then
  $SUDO cp "${state_dir}/logs/waired-agent.log" diag/ || true
fi

# Whatever transcript the leg tee'd, named by the caller.
for extra in "$@"; do
  if [ -f "${extra}" ]; then
    cp "${extra}" diag/ || true
  fi
done

# The daemon's own view at teardown. Both are reads, so the waired#838 write
# guard leaves them on TCP.
for route in setup/state inference/status; do
  out="diag/$(printf '%s' "${route}" | tr / -).json"
  curl -fsS --max-time 5 "http://127.0.0.1:9476/waired/v1/${route}" > "${out}" 2>&1 ||
    echo "unreachable" > "${out}"
done

if command -v journalctl >/dev/null 2>&1; then
  $SUDO journalctl -u waired-agent --no-pager -n 2000 2>&1 |
    tee diag/waired-agent.journal >/dev/null || true
fi

# So the upload step can read what root just copied. A no-op on Windows.
if [ -n "$SUDO" ]; then
  $SUDO chown -R "$(id -un)" diag || true
fi
ls -la diag || true

# Say so when the one thing this exists for is not here. The upload step uses
# if-no-files-found: ignore, and diag/ is never empty — the transcript and the
# two JSON reads are always in it — so an artifact with no engine log at all
# looks exactly like a good one. That is how waired-agent#1156 went unnoticed:
# the macOS legs had been shipping a plausible-looking bundle with no engine
# log in it since the collector was written.
if ! ls diag/*engine.log* >/dev/null 2>&1; then
  echo "::warning::collect-engine-diag: no engine log under ${state_dir} —" \
    "the artifact will not say why an engine failed to start"
fi
