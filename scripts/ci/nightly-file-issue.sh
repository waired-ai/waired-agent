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
# Environment:
#   TITLE_TAG   required — dedup key, also expected to be part of TITLE
#                          (e.g. "[nightly-installtest]")
#   TITLE       required — issue title
#   BODY        required — issue body
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
  exit 0
fi

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

# `in:title` search, then a client-side check: GitHub's search tokenizer drops
# the brackets, so "[nightly-installtest]" also matches "[nightly-testnet]"
# once it is reduced to words. Filtering the returned titles for the literal
# tag is what actually makes the dedup key exact.
open_titles="$(gh issue list --repo "${GITHUB_REPOSITORY}" --state open \
  --search "in:title ${TITLE_TAG}" --limit 100 --json title --jq '.[].title' || true)"

if printf '%s\n' "${open_titles}" | grep -qF -- "${TITLE_TAG}"; then
  echo "::notice::an open ${TITLE_TAG} issue already exists; not filing another"
  exit 0
fi

gh issue create --repo "${GITHUB_REPOSITORY}" --title "${TITLE}" --body "${BODY}"
