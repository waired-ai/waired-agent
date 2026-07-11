#!/usr/bin/env bash
# Idempotently upsert the rolling "Model radar" tracking Issue (issue #413).
# Finds the single open issue carrying ISSUE_LABEL and edits its body; creates
# it (and the label) if absent.
#
# Env:
#   GH_TOKEN      token with issues:write (the CATALOG_BOT_TOKEN PAT)
#   BODY_FILE     markdown body to set (default: $OUT_DIR/issue-body.md)
#   OUT_DIR       artifact dir from run.sh
#   ISSUE_LABEL   label identifying the rolling issue (default: model-radar)
#   ISSUE_TITLE   title used on create (default: "Model radar (rolling)")
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/catalog-radar/lib.sh
source "${HERE}/lib.sh"

require_env GH_TOKEN
BODY_FILE="${BODY_FILE:-${OUT_DIR:?OUT_DIR or BODY_FILE required}/issue-body.md}"
ISSUE_LABEL="${ISSUE_LABEL:-model-radar}"
ISSUE_TITLE="${ISSUE_TITLE:-Model radar (rolling)}"
[ -f "${BODY_FILE}" ] || die "body file not found: ${BODY_FILE}"

have_cmd gh || die "gh is required"

# Ensure the label exists (idempotent).
gh label create "${ISSUE_LABEL}" --color BFD4F2 \
  --description "Rolling catalog-radar candidate report (#413)" --force >/dev/null 2>&1 || true

existing="$(gh issue list --label "${ISSUE_LABEL}" --state open --limit 1 --json number --jq '.[0].number // empty')"
if [ -n "${existing}" ]; then
  log "updating Model radar issue #${existing}"
  gh issue edit "${existing}" --body-file "${BODY_FILE}" >/dev/null
  echo "${existing}"
else
  log "creating Model radar issue"
  url="$(gh issue create --title "${ISSUE_TITLE}" --label "${ISSUE_LABEL}" --body-file "${BODY_FILE}")"
  log "created ${url}"
  echo "${url}"
fi
