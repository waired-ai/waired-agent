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

# Mock research. Two candidates, differing only in whether their headline score
# cites an accepted source: the first escalates, the second is reported and
# never auto-drafted. That pair is the shell-level A/B for the rule that a
# vendor's own number cannot propose a model. Both use a LOCAL config_path so
# the draft step is hermetic.
cat > "${WORK}/research.json" <<JSON
[
  {
    "repo_id": "NewOrg/Fresh-Coder-32B",
    "scores": {
      "livebench_2026_06_25_code_generation": {"value": 74.5, "source": "livebench",
        "url": "https://livebench.ai/table_2026_06_25.csv", "retrieved": "2026-06-18"},
      "vendor_card_humaneval": {"value": 88.0,
        "url": "https://newvendor.example/blog", "retrieved": "2026-06-18"}
    },
    "confidence": "high",
    "license": "apache-2.0",
    "recommended": true,
    "rationale": "Ahead of the 27B we carry on the accepted coding table, at a 24GB footprint.",
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
  },
  {
    "repo_id": "NewOrg/Vendor-Only-30B",
    "scores": {
      "vendor_card_swe_bench": {"value": 79.0,
        "url": "https://newvendor.example/paper", "retrieved": "2026-06-18"},
      "vendor_card_humaneval": {"value": 92.0,
        "url": "https://newvendor.example/paper", "retrieved": "2026-06-18"}
    },
    "confidence": "high",
    "license": "apache-2.0",
    "recommended": true,
    "rationale": "Higher numbers than the first candidate, but every one is the vendor's own.",
    "model": {
      "model_id": "vendor-only-30b",
      "display_name": "Vendor Only 30B",
      "license": "apache-2.0",
      "context_length": 131072,
      "capabilities": ["chat", "tool_use"],
      "runtime": {"preferred": "ollama", "fallback": ["vllm"]},
      "security": {"trust_remote_code_required": false, "allow_persistent_kv_cache": true},
      "variants": [
        {"variant_id": "q4-gguf", "format": "ollama-tag", "quantization": "Q4_K_M",
         "runtime_support": ["ollama"], "source": {"type": "ollama", "tag": "vendor-only:30b-q4_K_M"},
         "total_params": 30000000000, "config_path": "${WORK}/config.json", "measured_weight_gb": 18.0}
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
grep -q "NewOrg/Vendor-Only-30B" "${out}/issue-body.md" || fail "issue body missing the vendor-only candidate"

# The A/B: identical shape, different provenance, opposite outcomes.
jq -e '.escalated == ["fresh-coder-32b"]' "${out}/summary.json" >/dev/null \
  || fail "only the accepted-source candidate should escalate: $(cat "${out}/summary.json")"
jq -e '.reported | index("NewOrg/Vendor-Only-30B")' "${out}/summary.json" >/dev/null \
  || fail "a vendor-only candidate must be reported, not escalated"
[ -f "${out}/bench/fresh-coder-32b.bench.json" ] \
  || fail "propose --bench-dir did not emit the benchmarks.json entry"
jq -e '.scores.livebench_2026_06_25_code_generation.source == "livebench"' \
  "${out}/bench/fresh-coder-32b.bench.json" >/dev/null \
  || fail "the bench entry lost its provenance"
[ -f "${out}/bench/vendor-only-30b.bench.json" ] \
  && fail "a reported-only candidate must not get a benchmarks.json entry"

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
