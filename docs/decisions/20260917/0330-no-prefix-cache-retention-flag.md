---
status: accepted
---

# プレフィックスキャッシュの retention interval は渡さない (20260917 03:30)

## Status
Accepted。オーナー裁定 2026-09-17(実装セッションでの判断; waired-ai/waired#1434)。

## Context

vLLM 0.29.0 は sliding-window / Mamba のグループを持つモデルのプレフィックスキャッシュに `prefix_cache_retention_interval` を入れ、既定を 0 (プロンプト末尾と接合点のチェックポイントだけ残す) にした (vllm-project/vllm#52216)。前回の pin 移動の記録 (docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md §9) は、gpt-oss-20b でこの既定が会話の途中を編集した再送の再利用を 1,904 トークンから 64 に落とし、`--prefix-cache-retention-interval 256` で 1,792 に戻ることを見ていた。waired-ai/waired#1434 はその flag を製品の argv に足す提案だった。

その後、当初の対象だった gpt-oss-20b / 120b (vLLM で sliding-window を持つ唯一の build) は waired-ai/waired-agent#1416 のオーナー裁定で後継無しに退役した。カタログに残るこのグループの build は Qwen3.5 のハイブリッド (0.8B / 2B / 4B bf16) だけで、4B の attention のブロックサイズは draft 無しで 1,056。

Qwen3.5-4B でコーディングエージェントの会話の形を再生した (docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md §5)。S1 は 1 つの会話を約 52k / 105k まで育てて次のターン・同一の再送・古い tool 結果を消した再送を測り、S2 は 3 つの会話を交互に約 170k まで育てて 6 ラウンド回した:

- MTP 無し、retention 未指定 (既定 0): 次のターン 1.48 / 2.24 s、再送 0.21 / 0.26 s。S2 の再計算 2,089,548 トークン、TTFT p50 57.9 s。
- MTP 無し、`--prefix-cache-retention-interval 1056`: S1 の全数値が未指定の 1% 以内。
- MTP 無し、None (dense): S1 は 1% 以内、S2 の再計算 2,452,812 (+17%)。
- MTP N1、retention 未指定: vLLM 自身が "Hybrid model with EAGLE speculative decoding: defaulting prefix_cache_retention_interval to dense checkpointing" と記録して dense に切り替える。次のターン 1.80 / 2.74 s、S2 の再計算 3,488,784。
- MTP N1、明示的に 0: 次のターン 3.11 / 5.15 s、最初の再送 1.81 / 2.74 s、長いプロンプトの同一の再送は何も当たらない (118,098 のうち 118,098 トークンを再計算)。vllm-project/vllm#53504 と一致する。

## Decision

**`--prefix-cache-retention-interval` は渡さない。** 残るハイブリッドの build では vLLM の既定が測った中で最良になる: draft 無しなら 0、MTP なら vLLM 自身が選ぶ dense。1056 は 1 本の会話 (S1) で何も変えず (S2 は測っていない)、None は長い会話の再計算を 17% 増やし、MTP で 0 を明示するとプレフィックスのヒットが消える。

`TestVLLMCommandArgs` の "never pins the prefix-cache retention interval" が、製品の argv がこの flag を名指ししないことを pin する。

## Consequences

- waired-ai/waired#1434 はこの記録で閉じる。当初の対象は build ごとカタログを離れ、残る対象では既定に勝る値が無かった。
- MTP の既定 on (docs/decisions/20260917/0310-vllm-serves-qwen35-with-two-mtp-draft-tokens.md) で retention は dense に変わり、長い会話の再計算は draft 無しより増える (S2 で +67%)。それはあの決定が受け入れたコストで、flag で取り戻せるものではない — 0 を明示するとヒットが消える。
- sliding-window の build がカタログに戻るなら、この決定を見直す。gpt-oss で 256 が会話の途中の編集の再利用を戻したのは 0500 §9 の記録のとおりで、その build にはこの flag が意味を持つ。
- vLLM が既定を変えたら (pin 移動のたびに) この再生を回し直す。既定に乗る決定は、既定が動くと黙って偽になる。

## Refs
- https://github.com/waired-ai/waired/issues/1434
- https://github.com/waired-ai/waired-agent/pull/1416
- https://github.com/vllm-project/vllm/pull/52216
- https://github.com/vllm-project/vllm/issues/53504
- docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md
- docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md
- docs/decisions/20260917/0310-vllm-serves-qwen35-with-two-mtp-draft-tokens.md
