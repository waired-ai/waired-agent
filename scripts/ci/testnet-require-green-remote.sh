#!/usr/bin/env bash
# testnet-require-green-remote.sh — cross-repo release gate for the agent
# repo (#184/#738): require a green real-NAT testnet run FOR AN AGENT
# COMMIT before releasing it, where the testnet harness lives in the
# private monorepo (waired-ai/waired), not in this repo.
#
# usage: testnet-require-green-remote.sh <agent-full-40-char-sha>
#
# env: GH_TOKEN        (required; fine-grained PAT scoped to the monorepo
#                       with Actions Read+Write — secret WAIRED_TESTNET_TOKEN)
#      TESTNET_REPO    (default waired-ai/waired)
#      TESTNET_GATE    (set to "off" to skip the gate — emergency lever,
#                       wired from the repo variable of the same name)
#      WAIT_TIMEOUT_S  (default 5400 — quota wait + ~25 min run + headroom)
#      POLL_INTERVAL_S (default 30)
#
# Cross-repo green semantics: the monorepo's testnet.yml takes a
# workflow_dispatch input `agent_ref` and embeds it in the run name
# ("testnet [agent:<ref>] ..."), building the agent images/tarballs from
# that ref of this repo. A monorepo run's head_sha is a MONOREPO commit,
# so the agent SHA in the display title is the only cross-repo join key:
# green for agent commit X = a workflow_dispatch run whose display title
# contains X and whose "testnet (…)" JOB concluded success (job-level,
# not workflow-level — a gate-skipped run also "succeeds"; see the
# monorepo's testnet-green-exists.sh). The run validates agent@X against
# monorepo main at dispatch time — exactly what a client release should
# be validated against (the deployed CP tracks monorepo main).
#
# exit 0: a green run exists (or completed while waiting) for the SHA.
# exit 1: no green run and none could be produced.
set -euo pipefail

sha="${1:?usage: testnet-require-green-remote.sh <agent-full-sha>}"
repo="${TESTNET_REPO:-waired-ai/waired}"
timeout_s="${WAIT_TIMEOUT_S:-5400}"
poll_s="${POLL_INTERVAL_S:-30}"

if [[ "${TESTNET_GATE:-}" == "off" ]]; then
  echo "::warning::TESTNET_GATE=off — SKIPPING the cross-repo testnet release gate for ${sha:0:7}. Re-enable the gate (unset the TESTNET_GATE repo variable) as soon as the emergency is over."
  exit 0
fi

if [[ ! "${sha}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::testnet-require-green-remote: '${sha}' is not a full 40-char SHA" >&2
  exit 1
fi

# Dispatch runs for this agent SHA: joined on the display title (see the
# header). event=workflow_dispatch excludes PR/nightly runs, whose titles
# carry agent 'main' rather than a pinned SHA.
runs_for_sha() { # prints "id status conclusion" per matching run
  # NB: `gh api --jq` takes a bare expression only (no jq --arg support);
  # the SHA is hex so direct interpolation is injection-safe.
  gh api "repos/${repo}/actions/workflows/testnet.yml/runs?event=workflow_dispatch&per_page=100" \
    --jq ".workflow_runs[] | select(.display_title | contains(\"${sha}\")) | \"\(.id) \(.status) \(.conclusion)\""
}

green_exists() {
  local id
  while read -r id status conclusion; do
    [[ "${status}" == "completed" && "${conclusion}" == "success" ]] || continue
    # Job-level verdict, mirroring the monorepo's testnet-green-exists.sh.
    local job_conclusion
    job_conclusion="$(gh api "repos/${repo}/actions/runs/${id}/jobs?per_page=100" \
      --jq '[.jobs[] | select(.name | startswith("testnet (")) | .conclusion] | first // "none"')"
    if [[ "${job_conclusion}" == "success" ]]; then
      echo "run ${id}: testnet job succeeded for agent ${sha:0:7}"
      return 0
    fi
  done < <(runs_for_sha)
  return 1
}

dispatch() {
  echo "::notice::no green testnet for agent ${sha:0:7}; dispatching ${repo} testnet.yml (agent_ref=${sha}) and waiting"
  gh workflow run testnet.yml --repo "${repo}" --ref main -f "agent_ref=${sha}"
}

dispatched=0
dispatch_at=0
deadline=$(( $(date +%s) + timeout_s ))
while :; do
  if green_exists; then
    echo "::notice::green testnet run (testnet job success) exists for agent ${sha:0:7} — release may proceed"
    exit 0
  fi
  lines="$(runs_for_sha)"
  if grep -Eq ' (queued|in_progress|waiting|pending) ' <<<"${lines} "; then
    echo "testnet run for agent ${sha:0:7} still in progress; waiting..."
  elif (( !dispatched )); then
    dispatch
    dispatched=1
    dispatch_at="$(date +%s)"
    # The dispatched run takes a few seconds to appear in the runs list;
    # fall through to the sleep and pick it up on the next poll.
  elif (( $(date +%s) - dispatch_at > 180 )); then
    # We dispatched, the appear-in-list grace window has passed, and every
    # matching run has completed without a job-level green.
    echo "::error::dispatched testnet run for agent ${sha:0:7} did not succeed — refusing to release. Investigate the testnet failure in ${repo} first." >&2
    exit 1
  fi
  if (( $(date +%s) >= deadline )); then
    echo "::error::timed out (${timeout_s}s) waiting for a green testnet run for agent ${sha:0:7}" >&2
    exit 1
  fi
  sleep "${poll_s}"
done
