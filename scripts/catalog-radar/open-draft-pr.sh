#!/usr/bin/env bash
# Open (or update) a DRAFT PR per escalated candidate (issue #413).
#
# For each model_id in summary.escalated it builds a minimal branch off
# origin/main containing: the new bundled manifest, the benchmarks.json entry,
# and a flagged seen.json record — then opens a DRAFT PR with a
# maintainer checklist (incl. the mandatory `make e2e-vllm` GPU lane) and a
# "numbers to verify" section. Never marks a PR ready; never merges.
#
# Idempotent: re-running force-updates the per-model branch and edits the
# existing PR rather than opening duplicates.
#
# Env:
#   GH_TOKEN            PAT with contents+pull-requests write (CATALOG_BOT_TOKEN)
#   GITHUB_REPOSITORY   owner/repo (set by Actions)
#   OUT_DIR             artifact dir from run.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
# shellcheck source=scripts/catalog-radar/lib.sh
source "${HERE}/lib.sh"

require_env GH_TOKEN
require_env GITHUB_REPOSITORY
OUT_DIR="${OUT_DIR:?OUT_DIR required}"
have_cmd gh || die "gh is required"
have_cmd jq || die "jq is required"
have_cmd go || die "go is required to regenerate the model-catalog docs"

NOW="$(date -u +%FT%TZ)"
AUTH_URL="https://x-access-token:${GH_TOKEN}@github.com/${GITHUB_REPOSITORY}.git"

git config --global user.name "waired-catalog-bot" 2>/dev/null || true
git config --global user.email "catalog-bot@users.noreply.github.com" 2>/dev/null || true

merge_benchmarks() {
  local model_id="$1" repo_id="$2" f="${REPO_ROOT}/internal/catalog/benchmarks.json" tmp
  tmp="$(mktemp)"
  local entry
  entry="$(jq --arg repo "${repo_id}" '
    (map(select(.repo_id == $repo))[0]) as $r
    | ({swe_bench_verified: $r.swe_bench_verified, sources: $r.sources,
        cross_checked: (($r.sources | length) >= 2), confidence: $r.confidence}
       + (if $r.secondary then {secondary: $r.secondary} else {} end))
  ' "${OUT_DIR}/research.json")"
  jq --arg mid "${model_id}" --argjson e "${entry}" '.models[$mid] = $e' "${f}" > "${tmp}" && mv "${tmp}" "${f}"
}

flag_seen() {
  local repo_id="$1" f="${REPO_ROOT}/internal/catalog/discovery/seen.json" tmp
  tmp="$(mktemp)"
  jq --arg repo "${repo_id}" --arg now "${NOW}" '
    .entries[$repo] = ((.entries[$repo] // {first_seen: $now}) + {last_seen: $now, status: "flagged"})
  ' "${f}" > "${tmp}" && mv "${tmp}" "${f}"
}

pr_body() {
  local model_id="$1" repo_id="$2" manifest="$3"
  local sources
  sources="$(jq -r --arg repo "${repo_id}" '
    (map(select(.repo_id == $repo))[0].sources // [])[] | "  - " + .url + " (" + .retrieved + ")"
  ' "${OUT_DIR}/research.json")"
  local swe
  swe="$(jq -r --arg repo "${repo_id}" 'map(select(.repo_id == $repo))[0].swe_bench_verified' "${OUT_DIR}/research.json")"

  cat <<EOF
Auto-drafted by **catalog-radar** (#413). **Draft — do not merge without the GPU lane.**

Adds \`${model_id}\` (\`${repo_id}\`) to the bundled catalog. SWE-bench Verified: **${swe}**.

All footprint numbers (VRAM/KV/weight) were computed by \`catalog-tool\`, not hand-typed;
\`quality_tier\` was freeze-assigned across the catalog. Benchmark scores are LLM-researched
and **must be verified** against the cited sources below before merge.

## Maintainer checklist
- [ ] Verify the benchmark numbers against the cited sources (below).
- [ ] **\`make e2e-vllm\` on a GPU host** — mandatory for any \`internal/catalog\` change (CLAUDE.md).
- [ ] \`go test ./... -timeout 10m\` and \`make verify-cross\`.
- [ ] \`gofmt -l .\`, \`go vet ./...\`, \`golangci-lint run\`.
- [ ] Confirm license is Apache-2.0 / MIT and the source tag/repo exists.
- [ ] Sanity-check the assigned \`quality_tier\` against neighbours.
- [ ] Mark ready for review (remove draft) once the GPU lane is green.

## Numbers to verify
Reproduce the computed fields:
\`\`\`
catalog-tool compute --repo ${repo_id} --quant <Q4_K_M|...> --total-params <N> --context <ctx>
\`\`\`
Benchmark sources:
${sources}

## Notes
- \`quality_tier\` slotted into a gap in the existing ladder (freeze mode) — no other
  manifest's tier changed.
- Closes nothing automatically; part of the recurring #413 refresh.
EOF
}

escalated="$(jq -r '.escalated[]?' "${OUT_DIR}/summary.json")"
[ -n "${escalated}" ] || { log "no escalated candidates; no PRs to open"; exit 0; }

git -C "${REPO_ROOT}" fetch origin main --quiet

for model_id in ${escalated}; do
  manifest="${OUT_DIR}/drafts/${model_id}.json"
  [ -f "${manifest}" ] || { log "no drafted manifest for ${model_id}; skipping"; continue; }
  repo_id="$(jq -r --arg mid "${model_id}" 'map(select(.model.model_id == $mid))[0].repo_id' "${OUT_DIR}/research.json")"
  branch="catalog-radar/${model_id}"
  log "preparing draft PR for ${model_id} (${repo_id}) on ${branch}"

  git -C "${REPO_ROOT}" checkout -B "${branch}" origin/main --quiet
  cp "${manifest}" "${REPO_ROOT}/internal/catalog/bundled/${model_id}.json"
  merge_benchmarks "${model_id}" "${repo_id}"
  flag_seen "${repo_id}"

  # Regenerate the model-catalog table from the just-added manifest so
  # the bundled change ships with its doc update and clears the docs-freshness
  # gate (`catalog-tool docs --check`). `go run` recompiles, embedding the new
  # bundled/*.json — a pre-built binary would miss it.
  ( cd "${REPO_ROOT}" && go run ./cmd/catalog-tool docs ) || die "regenerate model-catalog docs"

  git -C "${REPO_ROOT}" add \
    "internal/catalog/bundled/${model_id}.json" \
    "internal/catalog/benchmarks.json" \
    "internal/catalog/discovery/seen.json" \
    "docs/reference/models.md"
  git -C "${REPO_ROOT}" commit --quiet -m "feat(catalog): add ${model_id} (catalog-radar #413)

Auto-drafted by catalog-radar. Footprint fields computed by catalog-tool;
quality_tier freeze-assigned; benchmark provenance recorded. Draft PR —
needs the GPU lane and human verification of the cited scores.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"

  git -C "${REPO_ROOT}" push --force --quiet "${AUTH_URL}" "HEAD:${branch}"

  body_file="${OUT_DIR}/drafts/${model_id}.pr-body.md"
  pr_body "${model_id}" "${repo_id}" "${manifest}" > "${body_file}"
  title="feat(catalog): add ${model_id} (catalog-radar)"

  existing="$(gh pr list --head "${branch}" --state open --json number --jq '.[0].number // empty')"
  if [ -n "${existing}" ]; then
    log "updating existing PR #${existing}"
    gh pr edit "${existing}" --title "${title}" --body-file "${body_file}" >/dev/null
  else
    gh pr create --draft --title "${title}" --body-file "${body_file}" --head "${branch}" --base main >/dev/null
    log "opened draft PR for ${model_id}"
  fi
done

# Restore the working tree to main so subsequent steps see a clean checkout.
git -C "${REPO_ROOT}" checkout main --quiet 2>/dev/null || true
