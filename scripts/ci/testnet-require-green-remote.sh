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
#
# That window absorbs a lag of seconds. It does NOT absorb a job whose
# conclusion never finalises at all — GitHub occasionally leaves a job at
# in_progress/null on a run it has already concluded as success — so that
# case is decided in green_exists rather than waited out (waired-agent#697).
#
# The verdict table lives in scripts/ci/testnet-require-green-remote-test.sh:
# a stub `gh` builds the JSON the API would return and this script's own
# --jq expressions run over it, so the filters are exercised without a
# token, a network call, or a ~25-minute GCP run.
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
  gh api --paginate "repos/${repo}/actions/workflows/testnet.yml/runs?event=workflow_dispatch&per_page=100" \
    --jq ".workflow_runs[] | select(.display_title | contains(\"${sha}\")) | \"\(.id) \(.status) \(.conclusion)\""
}

# Job-level verdicts for one run: one line per "testnet (…)" job, with an
# unfinalised job printed as "pending" so the caller can tell it apart from
# a job that is absent.
#
# --paginate because a busy monorepo run carries more jobs than one page,
# and filter=all because the default (latest) hides the attempts a re-run
# replaced — the green one can be either.
testnet_job_conclusions() { # $1 = run id
  gh api --paginate "repos/${repo}/actions/runs/${1}/jobs?per_page=100&filter=all" \
    --jq '.jobs[] | select(.name | startswith("testnet (")) | .conclusion // "pending"'
}

# green_exists: is there a run for this agent SHA whose testnet job(s) say
# the harness passed?
#
# The run's own conclusion is not enough — a gate-skipped run also succeeds
# (see the header) — so the verdict is read per job. Three answers, not two:
#
#   no testnet job at all  -> NOT green. This is the gate-skipped run the
#                             job-level check exists to catch.
#   any job failed/cancelled/timed out/skipped -> NOT green.
#   every job succeeded    -> green.
#
# and a fourth that used to be silently folded into the first: a job whose
# conclusion never finalised (`null`) on a run GitHub already concluded as
# success. `first // "none"` mapped that to the same "none" as "no job at
# all", so a green testnet read as a gate-skipped one and the gate refused
# to release (waired-agent#697; observed on #671, where the run was
# completed/success while its testnet job sat at in_progress/null). It is
# accepted here, because the caller has already required the RUN to be
# completed/success and a successful run cannot contain a failed job — but
# it is reported separately, since "we could not read the verdict" and "the
# verdict was pass" are different claims and only the second is evidence.
#
# Reading every job rather than `first` matters for the same reason: the
# testnet job is a matrix, and element 0 is not a vote.
green_exists() {
  local id
  while read -r id status conclusion; do
    [[ "${status}" == "completed" && "${conclusion}" == "success" ]] || continue
    local jobs
    # A transient API failure must not read as "not green", and must not
    # kill the script under set -e: skip this run and let the caller poll.
    if ! jobs="$(testnet_job_conclusions "${id}")"; then
      echo "::warning::could not read the testnet jobs of run ${id} (transient?); will re-check"
      continue
    fi
    [[ -z "${jobs}" ]] && continue                      # gate-skipped run
    grep -qvE '^(success|pending)$' <<<"${jobs}" && continue
    if grep -q '^pending$' <<<"${jobs}"; then
      echo "::notice::run ${id}: the run concluded success but $(grep -c '^pending$' <<<"${jobs}") testnet job conclusion(s) never finalised; taking the run's verdict for agent ${sha:0:7}"
    else
      echo "run ${id}: testnet job succeeded for agent ${sha:0:7}"
    fi
    return 0
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
  done < <(gh api --paginate "repos/${repo}/actions/workflows/testnet.yml/runs?event=workflow_dispatch&per_page=100" \
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
    echo "::error::dispatched testnet run for agent ${sha:0:7} did not produce a green testnet job — refusing to release. Look for a run whose display title contains ${sha:0:7} in ${repo}: a failed testnet job is the usual cause, but a run that never appeared, was cancelled, or was gate-skipped reaches here too." >&2
    exit 1
  fi
  if (( $(date +%s) >= deadline )); then
    echo "::error::timed out (${timeout_s}s) waiting for a green testnet run for agent ${sha:0:7}" >&2
    exit 1
  fi
  sleep "${poll_s}"
done
