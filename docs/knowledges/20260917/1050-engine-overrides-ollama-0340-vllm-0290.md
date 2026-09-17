# エンジンが agent の要求を変える箇所 — ollama 0.34.0 / llama.cpp b10760 / vLLM 0.29.0 (20260917 10:50)

## Issue

waired-ai/waired-agent#1423 は、ollama のスケジューラが Qwen 3.5 / 3.6 / 3.8 のビルドを `OLLAMA_NUM_PARALLEL` に関わらず 1 スロットで起動することを見つけた。オーナーは 2026-09-17 に、ollama の制限に従ってカタログで上限を持つ(docs/decisions/20260917/1050-ollama-one-slot-builds-capped-in-catalog.md)と決め、あわせて「他のモデルにも似た問題が無いか調べる」よう求めた。この記録はその調査。pin されたエンジン(ollama 0.34.0、同梱の llama.cpp b10760、vLLM 0.29.0)が、agent が求めたものと違うものを走らせる箇所を、ソースを引いて並べる。次の pin 移動のときに読み直す。

「根拠」の列は、ソースで確認したこと(**ソース**)と、そこから推した結論(**推測**)を分ける。推測は、その旨を書いた部分だけが推測で、規則の行 (file:line) 自体はソースで確認している。

## Learnings

| # | エンジン | 当たるビルド | エンジンの規則 (file:line @ tag) | 効果 | waired-agent の扱い | 根拠 |
|---|---|---|---|---|---|---|
| 1 | ollama 0.34.0 | `model_family` が `qwen35` / `qwen35moe` の 16 ビルド(qwen3.5 全部、qwen3.6-27b、qwen3.6-35b-a3b、qwen3.8-27b)。qwen3.8-flash-next(`qwen4exp`)と granite4-350m(`granite`)は当たらない | `server/sched.go` `Scheduler.load` 505〜510 行: family が一覧(`mllama, qwen3vl, qwen3vlmoe, qwen35, qwen35moe, qwen3next, lfm2, lfm2moe, nemotron_h, nemotron_h_moe, nemotron_h_omni`)にあれば `numParallel = 1`。他に下げるのは embedding モデルだけ。値はそのまま `-np`(`llm/llama_server.go:375-376`)。メモリ量で減らす経路は無い | 2 を求めても runner は `-np 1`。一度に 1 リクエスト | カタログの `max_parallel` 1 で、tuning の要求・推奨最大・管理者の上書き・公開する容量・ローカルの受け入れを抑える(#1423)。観測した `-np` による clamp(#846)は印の無いビルドの受け皿として残す | ソース。**#846 の原因の訂正**: #846 が Ryzen AI Max+ 395 のホストで見た「2 → 1」は、スロットあたりの KV の価格の抜け(prompt cache、context checkpoint、vision tower)ではなく、この一覧による。そのホストは qwen3.5-122b-a10b(`qwen35moe`)を動かしていた。#1303 の macOS 2 台も同じ原因と矛盾しない(動かしていたモデルは確かめ直していない — 推測) |
| 2 | vLLM 0.29.0 | `nvidia/Qwen3.6-27B-NVFP4`、`nvidia/GLM-5.2-NVFP4` | config.json の `quantization_config.kv_cache_scheme` が `{dynamic: false, num_bits: 8, type: float}`(quant_method modelopt)。`vllm/utils/torch_utils.py` `resolve_kv_cache_dtype_string`(506〜524 行)+ `get_kv_cache_quant_algo_string`(442 行〜)が `auto` をこの scheme から `fp8` に解決する | agent は fp16 の選択を `--kv-cache-dtype` の省略(`cmd/waired-agent/inference_vllm_tuning.go` `vllmKVCacheDType` が `""` を返す)= `auto` で表すので、カタログが `kv_cache_types: ["fp8", "fp16"]` を出しているこの 2 ビルドで fp16 を選んでも fp8 で走る | 未対応。waired-ai/waired-agent#1441 に起票した(ソースと checkpoint の設定を読んだ結論で、実機では再現していない)。vLLM の `CacheDType` は明示の `float16` / `bfloat16` を受ける。`nvidia/Qwen3.6-35B-A3B-NVFP4` の config.json には `kv_cache_scheme` が無い | ソース |
| 3 | ollama 0.34.0 | unsloth の mtp-q3 / mtp-q2 タグ(qwen3.6-35b-a3b、qwen3.8-27b) | MTP の draft はタグか要求が `draft_num_predict` を設定したときだけ走る(`server/routes.go:158-160`、`llm/llama_server.go:811-813`)。unsloth のタグは設定していない | draft 無しで走る | 対応済み: waired-ai/waired-agent#1428(merged)で agent がホストごとに draft を書く(docs/decisions/20260917/0320-ollama-mtp-draft-is-written-per-host.md) | ソース |
| 4 | ollama 0.34.0 | ollama の全ビルド | コンテキストウィンドウを超えるチャットは黙って切られる: `server/prompt.go:35-78` が古いメッセージを落とす(Debug ログ)、`llm/llama_server.go:290-331` が `num_keep=4` を残してプロンプトを半分にする(Warn) | 超えた要求が、切れた文脈で答えられる | この経路は agent の gateway が先に断る: コンテキストウィンドウを超える要求は `context_overflow`(`internal/gateway/anthropic.go`、waired-ai/waired-agent#1187) | ソース |
| 5 | llama.cpp b10760 | `qwen35` / `qwen35moe` / `qwen4exp` | context shift と cache reuse が無効: projector を読むと `tools/server/server-context.cpp:1174-1182`、IMROPE(`n_pos_per_embd=4`)で `get_can_shift` が false(`src/llama-kv-cache.cpp:1193-1195`) | 生成がスロットの上限に達すると、shift せずに finish `length` で止まる | 扱い無し。#4 の gateway の拒否で、コンテキストウィンドウの中で始まった生成が上限で止まる形になる | ソース |
| 6 | llama.cpp b10760 | MTP の draft を持つビルド | draft のコンテキストの KV キャッシュは `--cache-type-k` に関わらず常に f16(`common/speculative.cpp:2481-2482`、`common/common.h:341-342`) | KV を q8_0 / q4_0 にしても draft の分は f16 | 対応済み: カタログの価格付けがそう見積もる(docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md §7) | ソース |
| 7 | llama.cpp b10760 | V キャッシュを量子化する全ビルド | V キャッシュを量子化し flash attention が auto なら FA を on にする。FA を切ると読み込みが失敗する(`src/llama-context.cpp:3688-3696`) | 主 KV キャッシュが黙って f16 に落ちることは無い | agent の verify の f16 への degrade 検出(`cmd/waired-agent/inference_ollama_verify.go`)が守る経路は、いまは大きな音で失敗する | ソース。他に黙って f16 にするものが無いことは推測 |
| 8 | ollama 0.34.0 | ollama の全ビルド | `OLLAMA_CONTEXT_LENGTH` 未設定なら既定は VRAM の合計から: 23 GiB 未満 4096、47 GiB 未満 32768、それ以上 262144(`server/routes.go:2064-2071`)。モデルの学習時のコンテキストを超える要求は Warn を出して切り詰める(`llm/server.go:112-116`) | 環境変数を渡さなければコンテキストウィンドウはホストの VRAM で決まる | 対応済み: agent はサイズ情報が無くても `OLLAMA_CONTEXT_LENGTH=200704` を渡す(docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md 決定 2)。例外は `internal_only` のモデル(#12) | ソース |
| 9 | vLLM 0.29.0 | Qwen3_5 / Qwen3_5Moe(プレフィックスキャッシュ有効時) | `mamba_cache_mode="align"` にし、attention のブロックサイズを上げる(`vllm/platforms/interface.py:916-925`) | プレフィックスキャッシュの当たりの粒度が粗くなる | 扱い無し。fp8 で約 1k〜1.6k トークン単位の当たりになるのは推測(0300 §2 の実測はブロック 1056〜1088) | ソース。粒度は推測 |
| 10 | vLLM 0.29.0 | sparse-MLA のモデル(glm-5.2、deepseek-v4-flash) | fp8 は `fp8_ds_mla` になる。SM12 の GPU では auto も `fp8_ds_mla`(`mla_attention.py:349-362`、`481-492`)。DeepSeek-V4 は fp8 を assert する(`models/deepseek_v4/attention.py:103-114`) | fp16 は起動時に失敗し、黙って別のものにはならない | 扱い無し。glm-5.2 の KV の配置は sizing の仮定と違う(トークン・層あたり 656 対 576 バイト)が、agent は実際のプールをエンジンのログから読み戻すので容量は正しいまま(推測) | ソース。配置の差と容量への影響は推測 |
| 11 | ollama 0.34.0 | ollama の全ビルド | `OLLAMA_FLASH_ATTENTION=1` は `--flash-attn on` を渡し、ollama 自身の GPU ごとの FA 判定を飛ばす(`llm/llama_server.go:597-610`)。判定は CUDA compute < 6.0 と == 7.2 で FA を切る(`ml/device.go:301-330`) | そういう GPU では、警告無しに attention が遅く走りうる | 扱い無し | ソース。遅くなることは推測 |
| 12 | ollama 0.34.0 | granite4-350m(`internal_only`、CI のルーティング用) | タグの family は `granite`(一覧に無い)。カタログは `attention_arch` を hybrid_mamba としているが dense(28 層すべて `head_count_kv=4`) | `kv_bytes_per_token_fp16` が無いのでコンテキストウィンドウは価格付けされず、モデル自身の 32k は 200,704 未満なので `OLLAMA_CONTEXT_LENGTH` を渡さない。#8 の VRAM の既定が効く | 扱い無し(#8 の例外どおり)。`attention_arch` を読むのは生成する docs の表だけで、`internal_only` のモデルはその表に出ないので、ラベルの誤りに製品上の影響は無い。起票しない | ソース(`catalog-tool layout --tag granite4:350m --arch-scalars` で `architecture: granite`、`full_attention_layers: 28` を読み直した) |

### 補足

- **#1 の一覧はテストにだけ写した。** `internal/catalog/ollama_single_request_families_test.go` の写しを、`internal/catalog/max_parallel_integration_test.go` が pin されたタグの `server/sched.go` から読み直して照合する。`.github/workflows/catalog-sources.yml` が回し、`internal/runtime/ollama_version.go` の変更でも起動する(required ではない)。pin を上げて一覧が変わると、ここで落ちる。
- **#1 の副作用。** 速度の計測のキー(`cmd/waired-agent/inference_bench_cache.go` `benchCacheKey`)は要求した `NumParallel` を含むので、これらのビルドで 2 を求めていたホストは、上げた後に 1 回測り直す。
- **#2 は、カタログの選択肢と実際の起動が食い違う唯一の行。** 他の行は、agent が求めたものをエンジンが変えるか、agent が先に断るかのどちらかで、利用者が選んだ値が黙って別の値になるのはこれだけ。
- **#4 と #5 の関係。** gateway がコンテキストウィンドウを超える要求を断るので、ollama の切り詰めには届かない。届くのは、コンテキストウィンドウの中で始まった生成が上限に当たる場合で、そこでは llama.cpp が shift できないので `length` で止まる。
- **#7 の読み方。** 以前の理解(「KV キャッシュが入らないと ollama が黙って f16 に落とす、あるいはスロットを減らす」)は、v0.34.0 のソースでは支えられない。減るのは #1 の一覧、落ちるのは無し。verify の検出は残してよいが、それが守っているのは「今は起きない」経路。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1423
- https://github.com/waired-ai/waired-agent/issues/846
- https://github.com/waired-ai/waired-agent/issues/1303
- https://github.com/waired-ai/waired-agent/issues/1441
- https://github.com/waired-ai/waired-agent/pull/1428
- https://github.com/waired-ai/waired-agent/pull/1439
- https://github.com/ollama/ollama/tree/v0.34.0
- https://github.com/vllm-project/vllm/tree/v0.29.0
- docs/decisions/20260917/1050-ollama-one-slot-builds-capped-in-catalog.md
- docs/decisions/20260917/0320-ollama-mtp-draft-is-written-per-host.md
- docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md
- docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md
- docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md
