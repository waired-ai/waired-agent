#!/usr/bin/env bash
# docs-preview-sweep.sh — delete Firebase Hosting preview channels that no
# longer belong to anything (.github/workflows/docs-preview-reap.yml, #429).
#
# deploy-docs.yml creates one `pr-<N>` channel per docs-touching PR and, until
# this script existed, nothing ever deleted one. Firebase caps a site at 50
# channels, so at this repo's PR rate the site sat permanently at the ceiling
# and every docs PR's preview deploy died with "channel quota reached" — a
# non-required check that is red for an infrastructure reason is a check nobody
# reads when it goes red for a real one.
#
# Usage:
#   docs-preview-sweep.sh [--dry-run]            sweep: expired channels, plus
#                                                every pr-<N> whose PR is closed
#   docs-preview-sweep.sh --pr <N> [--dry-run]   reap exactly pr-<N>, if present
#
# Env:
#   FIREBASE_PROJECT_ID   required; also selects the site via docs-site/.firebaserc
#   GH_TOKEN / GITHUB_TOKEN   required in sweep mode (to list open PRs)
#   GITHUB_REPOSITORY     defaults to waired-ai/waired-agent
#
# Exit codes: 0 = swept (possibly nothing to do), 1 = a delete failed,
# 2 = usage/config/auth error (nothing was deleted).
#
# Which channels are eligible is decided by scripts/ci/docs-preview-channels.mjs
# — a pure stdin/stdout selector, so the "never delete `live`" rules are pinned
# by scripts/ci/docs-preview-select-test.sh without touching real infra.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/../.." && pwd)"
helper="${here}/docs-preview-channels.mjs"

dry_run=0
pr=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry_run=1; shift ;;
    --pr) pr="${2-}"; shift 2 ;;
    --pr=*) pr="${1#--pr=}"; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | grep '^#'; exit 0 ;;
    *) echo "docs-preview-sweep: unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ -n "${pr}" && ! "${pr}" =~ ^[0-9]+$ ]]; then
  echo "docs-preview-sweep: --pr expects a number, got '${pr}'" >&2
  exit 2
fi
if [[ -z "${FIREBASE_PROJECT_ID:-}" ]]; then
  echo "docs-preview-sweep: FIREBASE_PROJECT_ID is required" >&2
  exit 2
fi

# firebase-tools writes firebase-debug.log into the working directory, and only
# docs-site/ gitignores it. Running from there also lets firebase.json /
# .firebaserc resolve the way the deploy job sees them.
cd "${repo_root}/docs-site"

site="$(FIREBASERC=".firebaserc" node "${helper}" site)" || exit 2
echo "docs-preview-sweep: project=${FIREBASE_PROJECT_ID} site=${site}"

channels="$(firebase hosting:channel:list \
  --site "${site}" --project "${FIREBASE_PROJECT_ID}" --json)" || {
  echo "docs-preview-sweep: hosting:channel:list failed" >&2
  exit 2
}

if [[ -n "${pr}" ]]; then
  # Single-PR reap needs no PR listing: whatever the PR's state, it is closed.
  ids="$(PR_ONLY="${pr}" node "${helper}" select <<<"${channels}")" || exit 2
else
  # Fail closed. An unreachable API must abort the sweep, never look like
  # "no PR is open" — that would reap every live preview on the site.
  open_prs="$(node "${helper}" open-prs)" || {
    echo "docs-preview-sweep: could not list open PRs; refusing to sweep" >&2
    exit 2
  }
  ids="$(OPEN_PRS="${open_prs}" node "${helper}" select <<<"${channels}")" || exit 2
fi

if [[ -z "${ids}" ]]; then
  echo "docs-preview-sweep: nothing to delete"
  exit 0
fi

count="$(printf '%s\n' "${ids}" | wc -l | tr -d ' ')"
if (( dry_run )); then
  echo "docs-preview-sweep: would delete ${count} channel(s):"
  printf '%s\n' "${ids}" | sed 's/^/  /'
  exit 0
fi

echo "docs-preview-sweep: deleting ${count} channel(s)"
rc=0
while IFS= read -r id; do
  [[ -z "${id}" ]] && continue
  if firebase hosting:channel:delete "${id}" \
      --site "${site}" --project "${FIREBASE_PROJECT_ID}" --force >/dev/null; then
    echo "  deleted ${id}"
  else
    echo "::warning::docs-preview-sweep: failed to delete ${id}"
    rc=1
  fi
done <<<"${ids}"

exit "${rc}"
