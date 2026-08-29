---
status: accepted
---

# GPU レーンは導出した設定で走る — 主張はリテラルでなくプロパティで

## Status

Accepted。waired-agent#955。参照 #887 / #891 / #1131 / #1132 / waired#1229。

## Context

`make e2e-vllm` は「GPU テスト実行義務」の裁定が名指すターゲットで、
#891 の PR 本文も「これらの serve フラグを出す前に走らせるもの」として引用していた。
**そのフラグを一度も観測していなかった。**

`internal/e2e/inference/vllm_test.go` は `VLLMConfig` を自前で組み立て、
`MaxNumBatchedTokens` / `KVOffloadingGiB` / `EnablePromptTokensDetails` を
セットしていなかった。`VLLMConfig` は 0 を「フラグを出さない」と読むので、
**L4 レーンの argv に `--max-num-batched-tokens` も `--kv-offloading-size` も
一度も載ったことがない**。エンジンは #887 が入れた当の設定について
vLLM 自身の既定で走っていた。レーンが買っていたのは「vLLM が配信する」であって
「うちのチューニングが正しい」ではない。

unit テストは argv しか見られない(プールは見えない)ので、
この GPU テスト群が唯一の観測点だった。

なぜ直せなかったか: 導出関数 `vllmMaxNumBatchedTokens` / `vllmKVOffloadingGiB` は
`cmd/waired-agent` の**非公開関数**で、`package main` は Go の仕様上
どのパッケージからも import できない。**e2e から呼ぶ手段が存在しなかった。**

## Decision

**1. 導出を `internal/router` へ移して export する。**

`internal/router/vllm_tuning.go` は既に `VLLMMaxModelLen` / `VLLMUsesFP8KV` /
`VLLMKVFactor` / `VLLMVRAMBudgetMB` を持ち、いずれも `hardware.Profile` を受ける。
serve フラグの導出は同じ family なので同じ場所に置く。
`cmd/waired-agent` の呼び出し元は 2 か所だけ。

**数値を e2e に再入力する案は採らない。** テストは**本番と同じ関数**を呼ぶ。
「同じことを言う場所が N 個あると各所が別のものを忘れる」。

**2. 主張はプロパティで書く。リテラルは 1 つも書かない。**

数字はカードごとに違う(issue は 1 ホストの実測、L4 は別の値を出す)。
さらに**エンジンの版でも動く** — pin を 0.24.0 → 0.28.0 に動かしたとき、
同 argv・同カードで KV プールが 14% 動いた(#1131/#1132)。
絶対値に依存した瞬間にこのレーンは壊れる。

| lane | 主張 |
|---|---|
| derived chunk | エンジンが報告する `max_num_batched_tokens` が**導出値と一致**／プール > `max_model_len`／`usage.prompt_tokens_details` が在る |
| smaller chunk | override 腕が効く／プール > 窓 |
| kv offloading | 起動して配信する／プール > 窓 |
| 横断 | **チャンクが大きいほどプールが小さい**／**オフロードを入れるとプールが減る** |

エンジンが実効チャンクを自分で印字することは 0.28.0 の実機で確認済みなので、
#955 が求めた「エンジンが報告するフラグが導出値と一致」は**直接**主張できる。
横断のプール比較はその裏取りで、**届かなかったフラグは 2 本とも同じプールを出す** —
「起動した」だけを見るテストがフラグの黙った脱落を通してしまう穴を、ここで塞ぐ。

**3. レーンに配線し、配線をガードに守らせる。**

`e2e-vllm-serve-flags` を Makefile に足し、`installtest-inference.yml` の
`targets=`(full 側のみ)に載せる。`scripts/ci/gpu-e2e-lane-guard.py` が
「gpu タグの Test 関数はどれかのターゲットの `-run` に一致すること」を強制する。
**外して落ちることを確認済み** — ガードが空振りしていない根拠。
テストは `internal/e2e/inference` に置く(ガードはそこと
`internal/e2e/agentgrade` しか見ない)。

## Consequences

- `make e2e-vllm-serve-flags` が増える。0.5B スモークモデルで engine 3 起動、
  レーンに ~20 分。`quick` には入れず `full`(nightly / `gpu=full` dispatch)のみ。
- **`Makefile` を触るので testnet が発火する**(クロスリポ ~16 分)。
  新しい e2e ターゲットを足す以上不可避で、正当。
- `internal/router` が docs サーフェスガードの対象なので、この PR は
  `docs-not-needed:` を必要とする。純粋な移設でユーザーに見える面は変わらない。
- 導出のロジックは 1 行も変えていない。#1148 が入れた定数の実測裏付けコメント
  (>=160GiB の第 3 段、`vllmMinBatchedTokens=256` の 0.28.0 実地確認)も
  そのまま移設した。
- KV オフロードの**効能**(退去→再送の時間差)は主張していない。
  spawn と時間を余計に食う割に、フラグが届いた証拠としては
  「プールが減る」で足りる。効能そのものは #887 の実測が持っている。

## Refs

- waired-agent#955(本件)/ #887(serve フラグ)/ #891(導入 PR)
- #1131 / #1132(エンジン pin 移動。プールが版で動く実測)
- waired-ai/waired#1229(ターゲットが在るのに誰も走らせていなかった型 = このガードの由来)
