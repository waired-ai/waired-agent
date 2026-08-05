# catalog-radar — benchmark research task

You are the research step of the waired model-catalog refresh pipeline
(waired-ai/waired#413). You are given a list of newly-released open-weight
models that already passed deterministic filters (license = Apache-2.0 or MIT,
text-generation, recent, not already in the catalog). Your job is to find
**published coding-benchmark results** for each candidate and decide whether it
is worth proposing for the bundled catalog.

Read `ACCEPTED_SOURCES_JSON` first. It is the list of leaderboards whose numbers
may justify adding a model, and it is the only such list — nothing you find
elsewhere carries that weight, however authoritative it looks. Every entry has
a `last_updated` date, because a leaderboard that has stopped moving cannot rank
a model released after it stopped.

## Inputs

- `CANDIDATES_JSON`: a JSON array of candidates, each `{repo_id, config_url,
  license, created_at}`. Read it.
- `ACCEPTED_SOURCES_JSON`: the leaderboards whose numbers may back a proposal,
  each `{id, name, url, last_updated, notes}`. Read it.
- The current catalog: the Qwen3.5 lineup carries the low and middle range, with
  Qwen3.6, GLM and DeepSeek above it. **A candidate is interesting when it is
  worth replacing a generation we carry** — a newer generation of a family, or a
  model that beats one on an accepted source. Another model of the SAME
  generation as one we already ship is not interesting; the catalog carries one
  generation per band on purpose.

## What to produce

Write a JSON **array** of research records to `RESEARCH_OUT` (one per candidate
you researched). Schema per record:

```json
{
  "repo_id": "Org/Model",
  "scores": {
    "<benchmark_and_version>": {
      "value": 74.5,
      "source": "<id from ACCEPTED_SOURCES_JSON, or omit if it is not one>",
      "url": "https://...",
      "retrieved": "YYYY-MM-DD",
      "notes": "optional"
    }
  },
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

1. **Cite every number.** Each score needs a real URL and the date you read it.
2. **Only an accepted source can justify adding a model.** Set `source` to an
   `id` from `ACCEPTED_SOURCES_JSON` **only** when the number came from that
   leaderboard's own published results. A number from the model card, the
   paper, a vendor blog, or any site republishing one of those is still worth
   recording — record it with **no** `source` field. Do not launder a vendor
   number into an accepted one by citing a mirror of it.

   Why this is strict: gpt-oss-120b reads **82.7, 81.9, 42.68 and 83.2** on one
   benchmark depending on who ran it and at what reasoning effort. Numbers from
   different runners are not comparable to each other, so a self-reported score
   cannot be weighed against a catalog member's.
3. **Check the source is still alive.** Before citing a leaderboard, find its
   own last-update date and compare it with `last_updated` in
   `ACCEPTED_SOURCES_JSON`. If what you found is materially older, say so in the
   rationale. A live URL is not live data: LiveCodeBench's leaderboard has been
   frozen since 2025-05 and Aider's since 2025-11, and both still serve.
4. **Pin the version and the window.** Many coding benchmarks are versioned or
   sliding (LiveCodeBench's v1..v6 are cumulative windows; LiveBench replaces
   its questions). Put the version or window in the score's key — e.g.
   `livebench_2026_06_25_code_generation` — so two numbers under the same name
   can never turn out to be different tests.
5. **Never invent numbers, and never write a zero for "not found".** If you
   cannot find a score, **omit it**. A zero is a measurement; the absence of one
   is not, and writing `0` for "I could not find it" is what put three fake
   zeros in the store this pipeline feeds. A candidate with no accepted score is
   still reported for a human to chase; it is simply not auto-proposed.
6. **Cross-check.** A `confidence: "high"` or `"medium"` record needs the
   accepted score **plus at least one more score** corroborating it — the vendor
   card is fine in that second role. One score on its own → `confidence: "low"`.
7. **You do NOT compute VRAM / KV / weight numbers.** Provide only the facts you
   can source: `total_params`, `active_params` (MoE), the quantization options
   and their `source` (Ollama tag or HF repo), `measured_weight_gb` if the
   artifact size is published, `vendor_support`, and `config_repo` (the HF repo
   carrying config.json). The pipeline computes the footprint fields with
   `catalog-tool`.
8. **License must be Apache-2.0 or MIT.** Confirm from the model card; if it is
   anything else, `recommended: false`.
9. `model` is required only when `recommended: true`. For a recommendation,
   include at least one Ollama variant (out-of-box path) when one exists.

10. **Check you may republish the number.** These scores are transcribed into a
   public repository with citations. Some leaderboards forbid that — Artificial
   Analysis's terms prohibit republishing its data outside a commercial licence,
   for instance. If a source's terms do not permit it, do not record the number,
   and say so in the rationale.

Use WebSearch / WebFetch for research. Be skeptical: marketing pages overstate
scores, and a vendor's own table is a claim rather than a measurement. When in
doubt, lower the confidence.
