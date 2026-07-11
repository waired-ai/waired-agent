# catalog-radar — benchmark research task

You are the research step of the waired model-catalog refresh pipeline (issue
#413). You are given a list of newly-released open-weight models that already
passed deterministic filters (license = Apache-2.0 or MIT, text-generation,
recent, not already in the catalog). Your job is to research **coding-agent
quality benchmarks** for each candidate and decide whether it is worth proposing
for the bundled catalog.

## Inputs

- `CANDIDATES_JSON`: a JSON array of candidates, each `{repo_id, config_url,
  license, created_at}`. Read it.
- The current catalog families (for comparison): Qwen2.5-Coder (3B/7B/14B),
  Qwen3-Coder MoE (30B-A3B / Next-80B-A3B / 480B-A35B), Qwen3.5 lineup, Qwen3.6,
  gpt-oss (20B/120B), GLM-4.5-Air. A candidate is only interesting if it plausibly
  **beats a same-size-class catalog member on SWE-bench Verified**.

## What to produce

Write a JSON **array** of research records to `RESEARCH_OUT` (one per candidate
you researched). Schema per record:

```json
{
  "repo_id": "Org/Model",
  "swe_bench_verified": 74.5,
  "secondary": {"livecodebench": 60.1, "aider_polyglot": 55.0},
  "sources": [
    {"url": "https://...", "retrieved": "YYYY-MM-DD", "value": 74.5},
    {"url": "https://...", "retrieved": "YYYY-MM-DD", "value": 74.0}
  ],
  "confidence": "high|medium|low",
  "license": "apache-2.0",
  "recommended": true,
  "rationale": "one or two sentences: which catalog member it beats and why",
  "model": {
    "model_id": "kebab-case-id",
    "display_name": "...",
    "model_aliases": ["Org/Model"],
    "license": "apache-2.0",
    "context_length": 131072,
    "capabilities": ["chat", "tool_use"],
    "runtime": {"preferred": "ollama", "fallback": ["vllm"]},
    "security": {"trust_remote_code_required": false, "allow_persistent_kv_cache": true},
    "variants": [
      {
        "variant_id": "q4-gguf",
        "format": "ollama-tag",
        "quantization": "Q4_K_M",
        "runtime_support": ["ollama"],
        "source": {"type": "ollama", "tag": "model:tag"},
        "total_params": 32000000000,
        "active_params": 0,
        "config_repo": "Org/Model",
        "measured_weight_gb": 19.0,
        "vendor_support": {"nvidia": {"ollama": "stable"}}
      }
    ]
  }
}
```

## Rules — non-negotiable

1. **Cite every benchmark number.** Each `sources[]` entry needs a real URL and
   the date you read it. Prefer the official leaderboards: swebench.com,
   livecodebench.github.io, aider.chat/docs/leaderboards, and the model's own
   card/paper.
2. **Cross-check.** A `confidence: "high"` or `"medium"` record needs **at least
   two independent sources** agreeing within a few points. One source, or a
   self-reported number you could not corroborate → `confidence: "low"`.
3. **Never invent numbers.** If you cannot find SWE-bench Verified for a model,
   set `swe_bench_verified: 0`, `recommended: false`, and explain in the
   rationale. A low-confidence or unbenchmarked candidate is still reported (for
   a human to chase) but will not be auto-proposed.
4. **You do NOT compute VRAM / KV / weight numbers.** Provide only the facts you
   can source: `total_params`, `active_params` (MoE), the quantization options
   and their `source` (Ollama tag or HF repo), `measured_weight_gb` if the
   artifact size is published, `vendor_support`, and `config_repo` (the HF repo
   carrying config.json). The pipeline computes the footprint fields with
   `catalog-tool`.
5. **License must be Apache-2.0 or MIT.** Confirm from the model card; if it is
   anything else, `recommended: false`.
6. `model` is required only when `recommended: true`. For a recommendation,
   include at least one Ollama variant (out-of-box path) when one exists.

Use WebSearch / WebFetch for research. Be skeptical: marketing pages overstate
scores; prefer the leaderboards. When in doubt, lower the confidence.
