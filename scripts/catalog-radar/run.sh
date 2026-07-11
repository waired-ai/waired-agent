#!/usr/bin/env bash
# catalog-radar orchestrator (issue #413).
#
# Produces, into OUT_DIR, every artifact the GitHub workflow (M5) needs to
# upsert the "Model radar" tracking Issue and open draft PRs:
#
#   candidates.json     deterministic HF discovery delta (catalog-tool radar)
#   research.json       LLM-researched benchmarks (headless claude, or MOCK)
#   summary.json        propose() classification (escalated/reported/rejected)
#   issue-body.md       rendered radar Issue body
#   specs/*.spec.json   draft specs for escalated candidates
#   drafts/<id>.json    full drafted manifests (catalog-tool draft + validate)
#   drafts/<id>.tier.json   freeze tier assignment incl. the new model
#
# All footprint numbers come from catalog-tool; the LLM only supplies cited
# benchmark facts. Nothing here touches git or gh — that is M5's job.
#
# Env:
#   CATALOG_TOOL     path to the catalog-tool binary (default: build into OUT_DIR)
#   OUT_DIR          artifact directory (default: mktemp)
#   CLAUDE_MODEL     headless model id (default: claude-opus-4-8)
#   CLAUDE_MAX_TURNS max agent turns (default: 20)
#   MAX_CANDIDATES   cap candidates sent to the LLM (default: 8)
#   ORGS / SINCE_DAYS  passthrough to `catalog-tool radar`
#   MOCK_CANDIDATES  path to a candidates.json to use instead of calling HF
#   MOCK_RESEARCH    path to a research.json to use instead of calling claude
#                    (MOCK_* make the whole run hermetic — used by the self-test
#                    and the workflow dry-run)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
# shellcheck source=scripts/catalog-radar/lib.sh
source "${HERE}/lib.sh"

# apply_tiers writes the freeze-assigned quality_tier values into a drafted
# manifest's variants (draft emits tier 0; tier assigns the real values).
apply_tiers() {
  local manifest="$1" tier_json="$2" model_id="$3" tmp
  tmp="$(mktemp)"
  jq --slurpfile t "${tier_json}" --arg mid "${model_id}" '
    ($t[0].assignments
      | map(select(.model_id == $mid))
      | map({key: .variant_id, value: .new_tier})
      | from_entries) as $tiers
    | .variants |= map(.quality_tier = ($tiers[.variant_id] // .quality_tier))
  ' "${manifest}" > "${tmp}" && mv "${tmp}" "${manifest}"
}

OUT_DIR="${OUT_DIR:-$(mktemp -d)}"
mkdir -p "${OUT_DIR}/specs" "${OUT_DIR}/drafts"
CLAUDE_MODEL="${CLAUDE_MODEL:-claude-opus-4-8}"
CLAUDE_MAX_TURNS="${CLAUDE_MAX_TURNS:-20}"
MAX_CANDIDATES="${MAX_CANDIDATES:-8}"

have_cmd jq || die "jq is required"

# Resolve / build catalog-tool.
if [ -z "${CATALOG_TOOL:-}" ]; then
  have_cmd go || die "go is required to build catalog-tool (or set CATALOG_TOOL)"
  CATALOG_TOOL="${OUT_DIR}/catalog-tool"
  log "building catalog-tool"
  (cd "${REPO_ROOT}" && go build -o "${CATALOG_TOOL}" ./cmd/catalog-tool)
fi

# 1) Deterministic discovery (or injected mock).
if [ -n "${MOCK_CANDIDATES:-}" ]; then
  log "using MOCK_CANDIDATES=${MOCK_CANDIDATES}"
  cp "${MOCK_CANDIDATES}" "${OUT_DIR}/candidates.json"
else
  log "discovery: scanning HuggingFace"
  # No --record: discovery does not mutate the in-repo seen ledger. Terminal
  # states are persisted per-model on the draft-PR branch (open-draft-pr.sh);
  # un-acted candidates re-surface weekly by design.
  radar_args=()
  [ -n "${ORGS:-}" ] && radar_args+=(--orgs "${ORGS}")
  [ -n "${SINCE_DAYS:-}" ] && radar_args+=(--since-days "${SINCE_DAYS}")
  (cd "${REPO_ROOT}" && "${CATALOG_TOOL}" radar "${radar_args[@]}") > "${OUT_DIR}/candidates.json"
fi

n_cand="$(jq '.candidates | length' "${OUT_DIR}/candidates.json")"
log "discovery: ${n_cand} candidate(s)"
jq --argjson n "${MAX_CANDIDATES}" '.candidates[:$n]' "${OUT_DIR}/candidates.json" > "${OUT_DIR}/candidates-capped.json"

# 2) Benchmark research — empty, mocked, or headless claude.
if [ "${n_cand}" -eq 0 ]; then
  log "no candidates; rendering empty radar"
  echo "[]" > "${OUT_DIR}/research.json"
elif [ -n "${MOCK_RESEARCH:-}" ]; then
  log "using MOCK_RESEARCH=${MOCK_RESEARCH}"
  cp "${MOCK_RESEARCH}" "${OUT_DIR}/research.json"
else
  require_env ANTHROPIC_API_KEY
  have_cmd claude || die "claude CLI not found (npm i -g @anthropic-ai/claude-code)"
  log "research: invoking claude (${CLAUDE_MODEL}, max-turns ${CLAUDE_MAX_TURNS})"
  prompt_text="$(cat "${HERE}/prompt.md")

CANDIDATES_JSON file: ${OUT_DIR}/candidates-capped.json
RESEARCH_OUT file: ${OUT_DIR}/research.json

Read CANDIDATES_JSON, research each candidate per the rules above, and Write the
resulting JSON array to RESEARCH_OUT. Output nothing else."
  claude -p "${prompt_text}" \
    --model "${CLAUDE_MODEL}" \
    --max-turns "${CLAUDE_MAX_TURNS}" \
    --allowedTools "WebSearch,WebFetch,Read,Write" \
    --permission-mode acceptEdits \
    --output-format json > "${OUT_DIR}/claude.log" 2>&1 \
    || die "claude invocation failed (see ${OUT_DIR}/claude.log)"
  [ -s "${OUT_DIR}/research.json" ] || die "claude did not write research.json"
fi

jq -e 'type == "array"' "${OUT_DIR}/research.json" >/dev/null \
  || die "research.json is not a JSON array"

# 3) Validate + classify + render (Go; hallucination-proof).
log "propose: validating research and rendering"
"${CATALOG_TOOL}" propose \
  --research "${OUT_DIR}/research.json" \
  --issue-out "${OUT_DIR}/issue-body.md" \
  --spec-dir "${OUT_DIR}/specs" > "${OUT_DIR}/summary.json"

# 4) For each escalated candidate: draft -> tier -> validate (deterministic).
escalated="$(jq -r '.escalated[]?' "${OUT_DIR}/summary.json")"
for model_id in ${escalated}; do
  spec="${OUT_DIR}/specs/${model_id}.spec.json"
  manifest="${OUT_DIR}/drafts/${model_id}.json"
  log "draft: ${model_id}"
  if ! (cd "${REPO_ROOT}" && "${CATALOG_TOOL}" draft --spec "${spec}") > "${manifest}" 2>"${OUT_DIR}/drafts/${model_id}.draft.log"; then
    log "WARNING draft failed for ${model_id}; skipping (see drafts/${model_id}.draft.log)"
    rm -f "${manifest}"
    continue
  fi
  tier_json="${OUT_DIR}/drafts/${model_id}.tier.json"
  (cd "${REPO_ROOT}" && "${CATALOG_TOOL}" tier --manifest "${manifest}" --format json) > "${tier_json}"
  apply_tiers "${manifest}" "${tier_json}" "${model_id}"
  if ! (cd "${REPO_ROOT}" && "${CATALOG_TOOL}" validate --file "${manifest}"); then
    log "WARNING validate failed for ${model_id}; leaving manifest for review"
  fi
done

log "done. artifacts in ${OUT_DIR}"
echo "${OUT_DIR}"
