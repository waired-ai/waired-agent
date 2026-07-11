#!/usr/bin/env bash
# renovate-review — post a Claude breakage-risk verdict comment on a Renovate PR.
#
# Division of labour (issue #532): CI verifies actual behaviour; this gate has a
# headless Claude READ the upstream release notes / changelogs for the bumped
# dependencies and post a PASS / CONCERN / FAIL verdict so the human merge call is
# informed. No automerge, no branch-protection change — advisory, Renovate PRs only.
#
# The shell stages inputs into files and does every `gh` write; Claude only gets
# Read / Write / WebSearch / WebFetch (the same safety boundary as catalog-radar,
# #413) — it never touches git or gh.
#
# Env (set by .github/workflows/renovate-review.yml):
#   GH_TOKEN          token with pull-requests:write (default GITHUB_TOKEN)
#   GH_REPO           owner/repo (gh picks this up automatically)
#   WR_PR             PR number from the workflow_run payload (may be empty)
#   HEAD_BRANCH       PR head ref (renovate/*) — fallback PR resolution
#   ANTHROPIC_API_KEY headless Claude key (unused when MOCK_VERDICT is set)
#   CLAUDE_MODEL      headless model id (default: claude-opus-4-8)
#   CLAUDE_MAX_TURNS  max agent turns (default: 30)
#   OUT_DIR           artifact dir (default: mktemp)
#   SETTLE_TIMEOUT    seconds to wait for CI checks to settle (default: 900)
#   DIFF_MAX_BYTES    cap on the staged diff size (default: 200000)
#   MARKER            idempotent-comment marker (default: <!-- renovate-review-bot -->)
#   DRY_RUN           "true" → render the verdict but do NOT post (preview)
#   ALLOW_NON_RENOVATE "true" → skip the Renovate-PR guard (manual workflow_dispatch)
#   MOCK_VERDICT      path to a verdict.md to use instead of calling claude (selftest)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() { printf '[renovate-review] %s\n' "$*" >&2; }
die() { printf '[renovate-review] ERROR: %s\n' "$*" >&2; exit 1; }
require_env() { local n="$1"; [ -n "${!n:-}" ] || die "required env ${n} is not set"; }
have_cmd() { command -v "$1" >/dev/null 2>&1; }

OUT_DIR="${OUT_DIR:-$(mktemp -d)}"
mkdir -p "${OUT_DIR}"
CLAUDE_MODEL="${CLAUDE_MODEL:-claude-opus-4-8}"
CLAUDE_MAX_TURNS="${CLAUDE_MAX_TURNS:-30}"
SETTLE_TIMEOUT="${SETTLE_TIMEOUT:-900}"
DIFF_MAX_BYTES="${DIFF_MAX_BYTES:-200000}"
MARKER="${MARKER:-<!-- renovate-review-bot -->}"
DRY_RUN="${DRY_RUN:-false}"

have_cmd gh || die "gh is required"
have_cmd jq || die "jq is required"
require_env GH_TOKEN

# 1) Resolve the PR number: prefer the workflow_run payload, else by head branch.
PR="${WR_PR:-}"
if [ -z "${PR}" ]; then
  [ -n "${HEAD_BRANCH:-}" ] || die "no PR number (WR_PR) and no HEAD_BRANCH to resolve from"
  log "resolving open PR by head branch ${HEAD_BRANCH}"
  PR="$(gh pr list --head "${HEAD_BRANCH}" --state open --limit 1 --json number --jq '.[0].number // empty')"
fi
[ -n "${PR}" ] || die "could not resolve an open PR (head=${HEAD_BRANCH:-?})"
log "reviewing PR #${PR}"

# 2) Stage PR metadata; soft-confirm this really is a Renovate dependency PR. The
#    workflow_run job gate already enforces renovate/* — this is belt-and-suspenders
#    for that path. A manual workflow_dispatch (ALLOW_NON_RENOVATE=true) reviews any PR.
gh pr view "${PR}" \
  --json number,title,body,headRefName,labels,url,author,additions,deletions,changedFiles \
  > "${OUT_DIR}/pr-meta.json"

if [ "${ALLOW_NON_RENOVATE:-false}" != "true" ]; then
  head_ref="$(jq -r '.headRefName // ""' "${OUT_DIR}/pr-meta.json")"
  has_dep_label="$(jq -r '([.labels[].name] | index("dependencies")) != null' "${OUT_DIR}/pr-meta.json")"
  case "${head_ref}" in
    renovate/*) : ;;
    *)
      if [ "${has_dep_label}" != "true" ]; then
        log "PR #${PR} is not a Renovate PR (head=${head_ref}, dependencies label=${has_dep_label}); skipping"
        exit 0
      fi
      ;;
  esac
fi

# 3) Let the rest of CI settle so the verdict reflects the final check rollup. `ci`
#    may finish while testnet / installtest are still running; this waits (bounded)
#    for them. `gh pr checks` exits non-zero when a check fails or is pending — that
#    is expected input here, not an error, hence `|| true`.
log "waiting up to ${SETTLE_TIMEOUT}s for checks to settle"
timeout "${SETTLE_TIMEOUT}" gh pr checks "${PR}" --watch --interval 30 >/dev/null 2>&1 || true
gh pr checks "${PR}" > "${OUT_DIR}/pr-checks.txt" 2>&1 || true

# 4) Stage the diff (capped — a lockfile bump can be huge and is low-signal).
gh pr diff "${PR}" > "${OUT_DIR}/pr-diff-full.txt" 2>/dev/null || true
if [ "$(wc -c < "${OUT_DIR}/pr-diff-full.txt")" -gt "${DIFF_MAX_BYTES}" ]; then
  {
    head -c "${DIFF_MAX_BYTES}" "${OUT_DIR}/pr-diff-full.txt"
    printf '\n\n[diff truncated at %s bytes — likely a large lockfile bump. Reason from the dependency versions in PR_META and the upstream changelog, not the raw diff.]\n' "${DIFF_MAX_BYTES}"
  } > "${OUT_DIR}/pr-diff.txt"
else
  cp "${OUT_DIR}/pr-diff-full.txt" "${OUT_DIR}/pr-diff.txt"
fi

# 5) Produce the verdict markdown (headless claude, or an injected mock).
verdict="${OUT_DIR}/verdict.md"
if [ -n "${MOCK_VERDICT:-}" ]; then
  log "using MOCK_VERDICT=${MOCK_VERDICT}"
  cp "${MOCK_VERDICT}" "${verdict}"
else
  require_env ANTHROPIC_API_KEY
  have_cmd claude || die "claude CLI not found (npm i -g @anthropic-ai/claude-code)"
  log "invoking claude (${CLAUDE_MODEL}, max-turns ${CLAUDE_MAX_TURNS})"
  prompt_text="$(cat "${HERE}/prompt.md")

PR_META file:     ${OUT_DIR}/pr-meta.json
PR_DIFF file:     ${OUT_DIR}/pr-diff.txt
PR_CHECKS file:   ${OUT_DIR}/pr-checks.txt
VERDICT_OUT file: ${verdict}

Read PR_META, PR_DIFF and PR_CHECKS, research the bumped dependencies per the rules
above, and Write the verdict markdown to VERDICT_OUT. Output nothing else."
  claude -p "${prompt_text}" \
    --model "${CLAUDE_MODEL}" \
    --max-turns "${CLAUDE_MAX_TURNS}" \
    --allowedTools "WebSearch,WebFetch,Read,Write" \
    --permission-mode acceptEdits \
    --output-format json > "${OUT_DIR}/claude.log" 2>&1 \
    || die "claude invocation failed (see ${OUT_DIR}/claude.log)"
fi
[ -s "${verdict}" ] || die "verdict.md was not produced"

# Defensive: guarantee the idempotency marker is present so the upsert can find the
# comment to refresh on re-runs even if the model forgot to emit it.
if ! grep -qF "${MARKER}" "${verdict}"; then
  tmp="$(mktemp)"
  { printf '%s\n\n' "${MARKER}"; cat "${verdict}"; } > "${tmp}"
  mv "${tmp}" "${verdict}"
fi

# 6) Upsert a single marked comment on the PR.
if [ "${DRY_RUN}" = "true" ]; then
  log "DRY_RUN=true — not posting. Verdict follows:"
  cat "${verdict}" >&2
  exit 0
fi

repo="${GH_REPO:?GH_REPO is required to upsert the comment}"
ids="$(gh api "repos/${repo}/issues/${PR}/comments" --paginate \
  --jq '.[] | select(.body | contains("'"${MARKER}"'")) | .id')"
existing_id="$(printf '%s\n' "${ids}" | head -n1)"
if [ -n "${existing_id}" ]; then
  log "updating existing verdict comment ${existing_id}"
  gh api -X PATCH "repos/${repo}/issues/comments/${existing_id}" -F body=@"${verdict}" >/dev/null
else
  log "posting new verdict comment"
  gh pr comment "${PR}" --body-file "${verdict}" >/dev/null
fi
log "done."
