#!/usr/bin/env bash
# Hermetic self-test for the catalog-radar harness: drives run.sh end-to-end
# with mocked discovery + research (no network, no Anthropic API), and asserts
# the full artifact set is produced and the drafted manifest validates.
#
# Needs: go, jq. Run locally or in the workflow dry-run.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
fail() { echo "SELFTEST FAIL: $*" >&2; exit 1; }

# A local config.json fixture so `draft` needs no network.
cat > "${WORK}/config.json" <<'JSON'
{"num_hidden_layers":64,"hidden_size":5120,"num_attention_heads":40,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":131072}
JSON

# Mock discovery delta: one fresh candidate.
cat > "${WORK}/candidates.json" <<'JSON'
{"orgs":["NewOrg"],"since_days":60,"candidates":[
  {"repo_id":"NewOrg/Fresh-Coder-32B","author":"NewOrg","created_at":"2026-06-10T00:00:00Z","license":"apache-2.0","why_passed":"test","config_url":"https://example/config.json"}
]}
JSON

# Mock research: recommended, high confidence, 2 sources, model spec using a
# LOCAL config_path so the draft step is hermetic.
cat > "${WORK}/research.json" <<JSON
[
  {
    "repo_id": "NewOrg/Fresh-Coder-32B",
    "swe_bench_verified": 74.5,
    "sources": [
      {"url": "https://www.swebench.com/", "retrieved": "2026-06-18", "value": 74.5},
      {"url": "https://newvendor.example/blog", "retrieved": "2026-06-18", "value": 74.0}
    ],
    "confidence": "high",
    "license": "apache-2.0",
    "recommended": true,
    "rationale": "Beats the 30B MoE on SWE-bench Verified at a 24GB footprint.",
    "model": {
      "model_id": "fresh-coder-32b",
      "display_name": "Fresh Coder 32B",
      "model_aliases": ["NewOrg/Fresh-Coder-32B"],
      "license": "apache-2.0",
      "context_length": 131072,
      "capabilities": ["chat", "tool_use"],
      "runtime": {"preferred": "ollama", "fallback": ["vllm"]},
      "security": {"trust_remote_code_required": false, "allow_persistent_kv_cache": true},
      "variants": [
        {"variant_id": "q4-gguf", "format": "ollama-tag", "quantization": "Q4_K_M",
         "runtime_support": ["ollama"], "source": {"type": "ollama", "tag": "fresh-coder:32b-q4_K_M"},
         "total_params": 32000000000, "config_path": "${WORK}/config.json", "measured_weight_gb": 19.0}
      ]
    }
  }
]
JSON

OUT_DIR="${WORK}/out" \
MOCK_CANDIDATES="${WORK}/candidates.json" \
MOCK_RESEARCH="${WORK}/research.json" \
  bash "${HERE}/run.sh" >/dev/null

out="${WORK}/out"
[ -f "${out}/issue-body.md" ] || fail "issue-body.md not produced"
grep -q "NewOrg/Fresh-Coder-32B" "${out}/issue-body.md" || fail "issue body missing candidate"
grep -q "74.5" "${out}/issue-body.md" || fail "issue body missing benchmark"

jq -e '.escalated | index("fresh-coder-32b")' "${out}/summary.json" >/dev/null \
  || fail "candidate not escalated"

manifest="${out}/drafts/fresh-coder-32b.json"
[ -f "${manifest}" ] || fail "drafted manifest missing"

# Tier must have been applied (draft emits 0; tier assigns a real value).
tier="$(jq '.variants[0].quality_tier' "${manifest}")"
[ "${tier}" -ge 1 ] 2>/dev/null || fail "quality_tier not applied (got ${tier})"

# Footprint fields computed by catalog-tool.
kv="$(jq '.variants[0].kv_bytes_per_token_fp16' "${manifest}")"
[ "${kv}" -gt 0 ] 2>/dev/null || fail "kv_bytes_per_token_fp16 not computed"

# The drafted manifest validates against the bundled catalog.
(cd "${REPO_ROOT}" && go run ./cmd/catalog-tool validate --file "${manifest}") >/dev/null \
  || fail "drafted manifest does not validate"

echo "SELFTEST PASS"
