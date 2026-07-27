---
status: accepted
---

# Ollama tuning verify を per-model 化し、num_parallel は runner の実値を報告 (20260714 21:31)

## Status
Accepted

## Context
waired#763: post-load tuning verify (`verifyOllamaTuning`) は `/api/ps` の
先頭モデルを見て `OLLAMA_CONTEXT_LENGTH` の適用を判定していた。「context は
server-global」という前提だったが、現行 Ollama はモデルごとに llama-server を
`-c` 付きで起動する per-model 構成。モデル切替直後は前モデルがまだ `/api/ps` に
residentで、別モデルの context を対象 tuning と突き合わせて
`OLLAMA_CONTEXT_LENGTH did not apply` を誤検知していた(正常な大窓構成が壊れて
見える)。加えて status の `num_parallel` は常に intent 値で、Ollama が
per-slot KV 不足時に `OLLAMA_NUM_PARALLEL` を黙って下げても runner の実 `-np` を
反映していなかった。

## Decision
1. **per-model 化**: verify は対象 tag の runner に一致した時のみ判定し、別モデル
   しか載っていなければ `tuningInconclusive` で abstain(次回 boot/swap で再検証)。
   これで cross-model の誤 warning が消える。
2. **runner 実値の報告**: 新規 `internal/platform/proclist`(linux `/proc`,
   windows `Get-CimInstance`, darwin `ps -axww`, 他は unsupported stub)で
   llama-server / `ollama runner` の command line を読み、対象 runner を context で
   相関して実 `-np` を取得。`ModelTuning.ObservedNumParallel` に記録し、status は
   観測値があればそれを、無ければ intent を報告。観測 < intent の時は誤アラームでは
   ない reduce の note を残す。context 表示は per-request 窓(intent)を維持
   (`-c` は総和で誤解を招くため表示には使わず相関のみ)。

## Consequences
- 誤検知 warning が止まり、`num_parallel` が実配信値になる。
- 純粋パーサは shared file に置き linux CI でテスト可能、I/O のみ per-OS。process
  列挙は verify 1 回のみで hot path ではない。相関は単一/一意一致のみ採用し、曖昧なら
  intent へ fallback(誤帰属を避ける)。
- 新規 `internal/` パッケージは testnet-nonrelevant に分類。

## Refs
- https://github.com/waired-ai/waired-agent/pull/21 (Fixes waired-ai/waired#763)
- waired-ai/waired#763 / #761 / #621 / #623
