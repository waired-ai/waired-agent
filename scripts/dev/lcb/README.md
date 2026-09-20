# LiveCodeBench harness (waired-ai/waired#1427)

Four scripts that measure a model's single-shot coding accuracy through an
ollama endpoint, so two models can be compared on the same problems. Written
for the quality-ladder question of waired-ai/waired#1427 and kept for the next
time a model is added.

**Read `docs/knowledges/20260920/1600-livecodebench-easy-medium-cannot-rank-the-2026-models.md`
first.** It records what this harness answered, what it did not (the easy and
medium problems of LiveCodeBench v6 are saturated for 2026 models: 92-96% for
every model measured, no significant difference), the cost per model, and the
traps. Running it again on the same problems will reproduce that, not a ranking.

```sh
# 1. the problem set: LiveCodeBench v6's newest additions, decoded
curl -sLO https://huggingface.co/datasets/livecodebench/code_generation_lite/resolve/main/test6.jsonl
python3 prepare.py                      # -> problems.jsonl (easy 30 / medium 35 / hard 35)

# 2. one model, through an ollama endpoint (a tunnel to the host is fine)
python3 generate.py --endpoint http://127.0.0.1:11434 --model <tag> \
    --out results/<label>.jsonl --problems problems.jsonl \
    --num-ctx 135168 --num-predict 131072

# 3. score in a sandbox (unshare -rn, memory and CPU limits, 6 s per test)
python3 score.py results/<label>.jsonl

# 4. compare runs pairwise (paired exact McNemar over the shared problems)
python3 compare.py results/*.scored.jsonl
```

Notes that matter for a fair comparison:

- Every model gets the same problems, the same sampling (temperature 0.6,
  top_p 0.95, top_k 20, seed 1) and the same output cap. Thinking is left ON,
  because that is how the product serves a coding turn.
- `generate.py` appends one line per problem and skips what it already has, so
  a run resumes after an interruption.
- Do not pass `--keep-alive` when borrowing a host's product engine: it would
  overwrite how long the product keeps its model loaded.
- These seconds are NOT a speed record. `internal/catalog/turnspeeds.json`
  takes only the product's own measurement, three or more times at a
  32,768-token depth.
