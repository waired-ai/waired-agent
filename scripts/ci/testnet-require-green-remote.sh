#!/usr/bin/env bash
# testnet-require-green-remote.sh — cross-repo testnet gate for the agent
# repo (#184/#738): require a green real-NAT testnet run FOR AN AGENT
# COMMIT, where the testnet harness lives in the private monorepo
# (waired-ai/waired), not in this repo. Callers: release.yml (tag SHA),
# testnet-nightly.yml (main HEAD), testnet-pr.yml (PR head SHA, #2).
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
#      DISPATCH_REASON (optional caller tag, e.g. agent-pr-123 — forwarded
#                       as the monorepo dispatch input `reason` and echoed
#                       into the run title as "[reason]". When set, queued/
#                       in-progress dispatches carrying the same [reason]
#                       but a DIFFERENT sha are cancelled first: they are
#                       superseded runs of an older push of the same PR,
#                       and cancelling them frees their testnet slot —
#                       the monorepo's testnet-cleanup.yml tears down
#                       cancelled runs.)
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
#
# Completion-instant consistency: the runs-list status/conclusion and the
# per-run job conclusion returned by the GitHub REST API are eventually
# consistent — they trail a run actually finishing by a few seconds. So a
# run that just went green can momentarily still read as not-green here.
# Before any hard failure the loop re-checks over a short settle window,
# which cannot mask a real failure (a failed run stays not-green). See the
# give-up branch below.
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
  if [[ -n "${DISPATCH_REASON:-}" ]]; then
    # Fall back to a reason-less dispatch if the monorepo copy of
    # testnet.yml predates the `reason` input (unknown-input dispatches
    # are rejected outright).
    if gh workflow run testnet.yml --repo "${repo}" --ref main \
        -f "agent_ref=${sha}" -f "reason=${DISPATCH_REASON}"; then
      return 0
    fi
    echo "::warning::dispatch with reason=${DISPATCH_REASON} failed (monorepo testnet.yml too old?); retrying without it"
  fi
  gh workflow run testnet.yml --repo "${repo}" --ref main -f "agent_ref=${sha}"
}

# Cancel superseded dispatches of the same caller tag (see DISPATCH_REASON
# in the header). The bracketed form "[<reason>]" is matched verbatim so
# agent-pr-12 never matches agent-pr-123's runs. DISPATCH_REASON is
# caller-controlled CI config (a PR number), not user input — safe to
# interpolate into the jq expression like the hex sha above.
cancel_superseded() {
  [[ -z "${DISPATCH_REASON:-}" ]] && return 0
  local id
  while read -r id; do
    [[ -z "${id}" ]] && continue
    echo "::notice::cancelling superseded testnet run ${id} ([${DISPATCH_REASON}], not agent ${sha:0:7})"
    gh api -X POST "repos/${repo}/actions/runs/${id}/cancel" >/dev/null \
      || echo "::warning::could not cancel run ${id} (already finishing?)"
  done < <(gh api "repos/${repo}/actions/workflows/testnet.yml/runs?event=workflow_dispatch&per_page=100" \
    --jq ".workflow_runs[]
          | select(.status == \"queued\" or .status == \"in_progress\" or .status == \"waiting\" or .status == \"pending\")
          | select(.display_title | contains(\"[${DISPATCH_REASON}]\"))
          | select(.display_title | contains(\"${sha}\") | not)
          | .id")
}

cancel_superseded

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
    # We dispatched, the appear-in-list grace window has passed, and no
    # matching run is in a non-terminal state. Before hard-failing, absorb
    # the completion-instant race (see the header note): a run that JUST
    # went green can still read as not-green on this poll because the
    # runs-list conclusion and the /runs/{id}/jobs job conclusion lag the
    # run finishing by a few seconds. Empirically a genuinely-green run was
    # failed here 2s after its testnet job succeeded. Re-check green_exists()
    # over a short settle window; a genuinely failed run stays not-green
    # across all of it, so this never masks a real failure.
    for _ in 1 2 3 4 5 6; do
      sleep 15
      if green_exists; then
        echo "::notice::green testnet run settled after completion for agent ${sha:0:7} — release may proceed"
        exit 0
      fi
    done
    echo "::error::dispatched testnet run for agent ${sha:0:7} did not succeed — refusing to release. Investigate the testnet failure in ${repo} first." >&2
    exit 1
  fi
  if (( $(date +%s) >= deadline )); then
    echo "::error::timed out (${timeout_s}s) waiting for a green testnet run for agent ${sha:0:7}" >&2
    exit 1
  fi
  sleep "${poll_s}"
done
