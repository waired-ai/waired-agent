#!/usr/bin/env bash
# nightly-file-issue.sh — file a GitHub issue when a SCHEDULED run goes red,
# deduplicated on a tag in the title.
#
# Why this exists (#215): a nightly is the one lane nobody is watching. The
# installtest-inference nightly ran red for five consecutive working days
# while the failure it was reporting (#178) sat in its logs, and the only
# reason anyone found out is that a user hit the bug and filed it by hand.
# Job summaries do not help — a summary is only read by someone who already
# opened the run.
#
# Deliberately NOT "required check": these lanes install a real engine, pull
# a multi-GB model from an external registry and run on self-hosted hardware,
# so they are exactly the kind of externally-flaky check that should stay out
# of the ruleset (the same call already made for the testnet gate).
#
# Deduplicated on TITLE_TAG rather than on the SHA: the point is that a human
# looks, and ten issues for one broken nightly is how a lane gets muted. One
# open issue per tag until someone closes it.
#
# That rule stands. What did NOT work is what it did with an issue that was
# already open: nothing at all. The [nightly-installtest] issue filed on
# 2026-07-28 was still open a month later — its root-cause tracker closed on
# 08-05 — and every red night since had been absorbed into it, printing a
# ::notice:: and telling nobody. In that month two lanes went red that were
# green when it was filed, and its body still named the July lanes and the
# July run (waired-agent#1112).
#
# So: one issue per outage, and when the SET OF RED LANES CHANGES under an
# open one, a comment says so. Same set, same silence as before — a comment
# every morning restating an unchanged outage is how a report gets muted,
# which is the thing this file exists to avoid.
#
# Environment:
#   TITLE_TAG   required — dedup key, also expected to be part of TITLE
#                          (e.g. "[nightly-installtest]")
#   TITLE       required — issue title
#   BODY        required — issue body
#   RED_LANES   optional — the current red-lane list (nightly-red-lanes.sh's
#                          output). When an open tagged issue does not
#                          already carry these exact lanes, a comment is
#                          added naming them and this run. Unset keeps the
#                          pre-waired-agent#1112 behaviour: notice and exit.
#   DRY_RUN     optional — non-empty prints the decision and touches nothing,
#                          so the logic is verifiable without a red nightly
#   GH_TOKEN    required unless DRY_RUN (needs `issues: write`)
#   GITHUB_REPOSITORY  required unless DRY_RUN
set -euo pipefail

: "${TITLE_TAG:?TITLE_TAG is required (the dedup key, e.g. [nightly-installtest])}"
: "${TITLE:?TITLE is required}"
: "${BODY:?BODY is required}"

case "${TITLE}" in
  *"${TITLE_TAG}"*) ;;
  *)
    echo "::error::TITLE does not contain TITLE_TAG (${TITLE_TAG}); the next run would not dedupe against this issue" >&2
    exit 1
    ;;
esac

if [ -n "${DRY_RUN:-}" ]; then
  echo "nightly-file-issue: DRY_RUN — would search for an open issue tagged ${TITLE_TAG}"
  echo "nightly-file-issue: DRY_RUN — title: ${TITLE}"
  if [ -n "${RED_LANES:-}" ]; then
    echo "nightly-file-issue: DRY_RUN — would comment on an open one whose lanes differ"
  fi
  exit 0
fi

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

# `in:title` search, then a client-side check: GitHub's search tokenizer drops
# the brackets, so "[nightly-installtest]" also matches "[nightly-testnet]"
# once it is reduced to words. Filtering the returned titles for the literal
# tag is what actually makes the dedup key exact.
open_issues="$(gh issue list --repo "${GITHUB_REPOSITORY}" --state open \
  --search "in:title ${TITLE_TAG}" --limit 100 --json number,title \
  --jq '.[] | [.number, .title] | @tsv' || true)"

# The client-side filter is what makes the key exact; the number comes with
# it so the open issue can be commented on rather than only counted.
# `|| true` because grep exits 1 on no match and this runs under `set -e`.
existing="$(printf '%s\n' "${open_issues}" | grep -F -- "${TITLE_TAG}" | head -1 | cut -f1 || true)"

# A machine-readable signature of the red lanes, compared instead of the
# rendered list. The rendered list is multi-line, and a multi-line pattern is
# several patterns to grep -F — one of them the empty string, which matches
# anything. That mistake reports every outage as already-reported.
lanes_marker() {
  printf '<!-- red-lanes: %s -->' \
    "$(printf '%s' "${1}" | sed -e 's/^- //' -e '/^$/d' | paste -sd';' -)"
}
marker=""
if [ -n "${RED_LANES:-}" ]; then
  marker="$(lanes_marker "${RED_LANES}")"
fi

if [ -n "${existing}" ]; then
  if [ -z "${marker}" ]; then
    echo "::notice::an open ${TITLE_TAG} issue already exists (#${existing}); not filing another"
    exit 0
  fi
  # Has this outage already been reported with these lanes? Both the body
  # and every comment are searched: the body carries the first report, a
  # comment carries each later change, and re-stating either is the noise
  # the dedup rule is about.
  said="$(gh issue view "${existing}" --repo "${GITHUB_REPOSITORY}" \
    --json body,comments --jq '.body, .comments[].body' 2>/dev/null || true)"
  if printf '%s\n' "${said}" | grep -qF -- "${marker}"; then
    echo "::notice::${TITLE_TAG} #${existing} already reports these lanes; saying nothing"
    exit 0
  fi
  echo "::notice::${TITLE_TAG} #${existing} is open with a different set of red lanes; commenting"
  gh issue comment "${existing}" --repo "${GITHUB_REPOSITORY}" --body "$(printf \
    'The red lanes have changed since this issue was filed:\n\n%s\nRun: %s/%s/actions/runs/%s\n\n%s\n' \
    "${RED_LANES}" "${GITHUB_SERVER_URL}" "${GITHUB_REPOSITORY}" "${GITHUB_RUN_ID}" "${marker}")"
  exit 0
fi

# The marker rides the body too, so the first report is comparable against
# the nights that follow it.
gh issue create --repo "${GITHUB_REPOSITORY}" --title "${TITLE}" \
  --body "$(printf '%s\n\n%s\n' "${BODY}" "${marker}")"
