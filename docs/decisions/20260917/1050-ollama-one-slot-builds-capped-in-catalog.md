---
status: accepted
---

# ollama が一度に 1 リクエストしか処理しないビルドの上限はカタログで持つ (20260917 10:50)

## Status
Accepted。オーナー裁定 2026-09-17(waired-ai/waired-agent#1423 に記録: 「ollamaの制限は準拠しましょう」「これらのモデルについてcapacityの最大値を1にする設定を入れてください」)。実装は proto の waired-ai/waired-agent#1439 と、この記録を含む agent の PR。コントロールプレーン側は別の PR で続く。

## Context

- ollama v0.34.0(`internal/runtime/ollama_version.go` の `OllamaPinnedVersion`)のスケジューラは、固定の model family の一覧に当たるモデルを、`OLLAMA_NUM_PARALLEL` に関わらず 1 スロットで起動する(`server/sched.go` `Scheduler.load` 505〜510 行、`"model architecture does not currently support parallel requests"`、ref ollama/ollama#4165)。一覧は `mllama, qwen3vl, qwen3vlmoe, qwen35, qwen35moe, qwen3next, lfm2, lfm2moe, nemotron_h, nemotron_h_moe, nemotron_h_omni`。v0.34.0 で `numParallel` を下げる箇所はこれと embedding モデル(`!completion`)の 2 つだけで、値はそのまま runner の `-c NumCtx*numParallel -np numParallel` になる(`llm/llama_server.go:375-376`)。メモリ量でスロット数を減らす経路は無い(v0.34.0 の木で `numParallel` への代入は sched.go:502、:508、:715 だけ)。
- 比較されるのはタグの config blob の `model_family`。2026-09-17 に registry.ollama.ai と huggingface.co から読んだ結果、カタログの ollama ビルド 18 のうち 16 が `qwen35` / `qwen35moe`(qwen3.5 0.8b q8-gguf、qwen3.5 2b / 4b / 9b / 27b / 35b-a3b / 122b-a10b q4-gguf、qwen3.6-27b mtp-q4-gguf と q4-gguf、qwen3.6-35b-a3b mtp-q4-gguf / q4-gguf / mtp-q3-gguf / mtp-q2-gguf、qwen3.8-27b mtp-q4-gguf / q3-gguf / q2-gguf)。qwen3.8-flash-next q2-gguf は `qwen4exp`、granite4-350m bf16-gguf は `granite` で、どちらも一覧に無く、2 スロットの要求はそのまま通る。
- この変更の前の agent: `computeOllamaTuningOpts` は、コンテキストウィンドウ 2 つ分の KV キャッシュが入るホストでは `OLLAMA_NUM_PARALLEL=2` を求めた。runner は `-np 1` で起動し、verify が「ollama reduced request parallelism from 2 to 1 — the engine's reason: …」を tuning の警告に足し、次の reconcile で waired-ai/waired-agent#846 の観測値による clamp が要求を 1 に下げ、`ServeInputsEqual` が入力の変化を見てエンジンを 1 回再起動した(結果は同じ `-np 1`)。公開する Capacity は runner の `-np` を読むのですでに正しかった(waired-ai/waired-agent#1303)。正しくなかったのは、管理者の「Max concurrent requests」の上書き(コントロールプレーンの `inference_max_clients` → map の Capacity と DesiredParallel)がそのまま公開・適用されることと、agent の `RecommendedMaxParallel`(floor(maxCtx / ctx))が実際より大きいこと。
- #846 の原因の読み直し。#846(closed)は、Ryzen AI Max+ 395 のホストで見た「2 → 1」を、スロットあたりの KV の価格に sizing が入れていないもの(prompt cache、context checkpoint、vision tower)のせいだとした。そのホストが動かしていたのは qwen3.5-122b-a10b、つまり `qwen35moe` のビルドなので、減ったのは上の一覧によるもので、メモリではない。#1303 の macOS 2 台(2 を公開して `-np 1`)も同じ原因と矛盾しない(そのとき動かしていたモデルはここでは確かめ直していない)。
- 同じ Qwen 3.6 / 3.8 のモデルは、vLLM のビルドでは複数のリクエストをまとめて処理する。llama-server 自体がこれらの family を `-np 2` で正しく動かすかどうかは試していない(#1423)。

## Decision

1. **上限はカタログの variant が持つ。** `proto/catalog` の `Variant.MaxParallel`(`max_parallel`、omitempty、0 = 上限無し)。`Validate` は 0 以上で、0 以外は ollama だけが動かすビルドにしか許さない(vLLM は同じモデルをまとめて処理する)。`VariantSHA` には入れない。同梱の値は上の 16 ビルドに 1。
2. **読むのは `ServedMaxParallel(engineType, modelID, variantID)` / `ServedMaxParallelIn`。** エンジンが ollama でなければ 0。variant が一致すればその値。variant が空か未知なら、そのモデルの ollama ビルドすべてが同じ値のときだけその値(食い違えば 0 — 多く許すビルドを少ないほうに丸めると過小に報告するため)。
3. **agent は 3 つの値を上限で抑える。** `computeOllamaTuningOpts` の自動サイジングの `NumParallel`、`RecommendedMaxParallel`、管理者の上書き。上書きを抑えるときは警告を付けない — 失うものが無く、上限で止まる理由はコントロールプレーン側が説明する。公開する容量(`capacityFn`)とローカルの受け入れ(`cmd/waired-agent/local_admission.go` の `SetBuildLimit`)は `ServingMaxParallel()` で抑える。受け入れは map の値を抑える前の形で持ち、上限の無いビルドへ切り替えたときは管理者の上書きが戻る。
4. **#846 の観測値による clamp は残す。** カタログが印を付けていないビルドで runner が減らしたときの受け皿。「ollama tuning verified」のログは `observed_parallel`(runner の `-np`、0 = 読めていない)を持つ。
5. **ollama の一覧はテストにだけ写す。** `internal/catalog/ollama_single_request_families_test.go` の写しを、`internal/catalog/max_parallel_integration_test.go`(build tag `integration`)が pin されたタグの `server/sched.go` から読み直して照合し、同梱の各 ollama タグの `model_family` を読んで一覧の family に `max_parallel` 1 がちょうど付いていることを見る。`.github/workflows/catalog-sources.yml` が回し、`internal/runtime/ollama_version.go` の変更でも起動する。required check ではない。製品コードの真実は 1 つ(カタログ)に留める。
6. **コントロールプレーンも同じ上限で抑える**(別 PR): 管理者の上書きと Public Share のゲスト数の上限を抑え、NAVI がその理由を示す。

### 退けた案

- (a) **2 を求め続け、観測した `-np` に任せる。** 公開する容量は正しいままだが、警告、無駄な再起動、管理者の上書き、`RecommendedMaxParallel` の 4 つが間違ったまま残る。
- (b) **pull したモデルの family から実行時に導く。** コントロールプレーンは family を見られないので、上書きとゲスト数の上限を抑えられない。agent とコントロールプレーンで真実が 2 つになる。
- (c) **vLLM のビルドにも上限を付ける。** vLLM は同じモデルをまとめて処理する。上限は ollama のスケジューラの事実で、モデルの性質ではない。

## Consequences

- これらのビルドで 2 を求めていたホストは、この版に上げた後に速度の計測を 1 回やり直す(`cmd/waired-agent/inference_bench_cache.go` の `benchCacheKey` が要求した `NumParallel` を含むため)。
- これらのビルドで「ollama reduced request parallelism from 2 to 1」の警告と、それに続く 1 回の再起動が出なくなる。
- ollama の pin を上げて一覧に family が増減したときは、カタログの編集が要る。catalog-sources のレーンが落ちるが required ではないので、pin を動かす人が見る(`ollama_version.go` のコメントに書いた)。
- #846 の原因の記述を直した(`inference_ollama_tuning.go`、`inference_ollama_verify.go`、`inference_warm_slots.go`、`internal/runtime/ollama.go` のコメント)。「スロットあたりの価格の抜け」は、v0.34.0 では減少の説明にならない。
- 利用者向けには、docs-site の「動作を確認する」と「モデルを変更する」に、Ollama では Qwen3.8 Flash Next 以外の Qwen 3.5 / 3.6 / 3.8 のモデルが一度に 1 つのリクエストにしか答えないことを書いた。`waired status --observability` の Engine 行の引用も、いまの出力(`0 running, 1 conversations kept warm, inflight=0`)に直した。
- 他のモデルで似た問題が無いかの調査(オーナーの依頼)は docs/knowledges/20260917/1050-engine-overrides-ollama-0340-vllm-0290.md。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1423
- https://github.com/waired-ai/waired-agent/pull/1439
- https://github.com/waired-ai/waired-agent/issues/846
- https://github.com/waired-ai/waired-agent/issues/1303
- https://github.com/ollama/ollama/blob/v0.34.0/server/sched.go(`Scheduler.load`)
- docs/knowledges/20260917/1050-engine-overrides-ollama-0340-vllm-0290.md
- docs/decisions/20260917/0320-ollama-mtp-draft-is-written-per-host.md(draft は 2 つ目のスロットより先)
- `proto/catalog/max_parallel.go`、`cmd/waired-agent/inference_ollama_tuning.go`、`cmd/waired-agent/local_admission.go`、`internal/catalog/max_parallel_integration_test.go`
