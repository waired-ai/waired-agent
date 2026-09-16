# MTP 投機的デコーディングの実測 — vLLM 0.29.0 と ollama 0.34.0 (20260917 03:00)

## Issue

waired-ai/waired#1432 (vLLM の MTP)、waired-ai/waired#1433 (ollama の draft)、waired-ai/waired#1434 (プレフィックスキャッシュの retention interval) の実測記録。前回の pin 移動の記録 (docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md §11〜§12) は MTP を「エンジンの事実として測っただけで採用していない」と書いた。この記録はその続きで、採用の判断に使った数値を残す。判断そのものは 3 つの決定記録にある: docs/decisions/20260917/0310-vllm-serves-qwen35-with-two-mtp-draft-tokens.md、docs/decisions/20260917/0320-ollama-mtp-draft-is-written-per-host.md、docs/decisions/20260917/0330-no-prefix-cache-retention-flag.md。

MTP (multi-token prediction) はモデル自身が持つ追加の層で次のトークンを複数まとめて提案し、本体がそれを検証する投機的デコーディングの一種。vLLM では `{"method":"mtp","num_speculative_tokens":N}`、ollama では `draft_num_predict N` がその draft の長さで、以下では N と書く。

計測環境 (すべて 2026-09-16 / 17):

- vLLM 0.29.0: RTX PRO 4000 Blackwell 24 GB (24,467 MiB) の Linux 機。製品の adapter が出す argv の形で回した: gpu-memory-utilization 0.85、KV キャッシュ fp8、プレフィックスキャッシュ有効、`--enable-prompt-tokens-details`、max-num-batched-tokens 4096、max-num-seqs 16、tool parser `qwen3_xml`、max-model-len 262,144。モデルは Qwen3.5 0.8B / 2B / 4B bf16。
- ollama 0.34.0 CUDA: 同じ 24 GB のカード 1 枚。製品の serve 環境: `OLLAMA_CONTEXT_LENGTH` 200704、KV q4_0、flash attention、`OLLAMA_NUM_PARALLEL` 1、`OLLAMA_KEEP_ALIVE` -1。タグは `hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF:UD-Q3_K_XL` / `UD-Q2_K_XL` と `hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL` / `UD-Q2_K_XL` に、製品の RENDERER / PARSER の刻印と `PARAMETER draft_num_predict N` を足したもの。
- ollama 0.34.0 Metal: Apple M5 Pro 48 GB (Metal の予算 38,338 MiB) と Apple M4 16 GB (同 約 11.8 GiB)。serve 環境は同じ。

計測の前提はオーナーの規則どおり: 製品のエンジンは止め、他のエンジンプロセスは無し、load average を実行ごとに記録、3 回以上、コンテキストウィンドウ 200,704 は runner の `-c` から読み戻した。デコード速度はストリーミングで 400 トークン生成を 3 回行った中央値。「novel」は新しいコードを書かせる、「edit」はプロンプトに入っているコードを書き直させるプロンプト。vLLM のデコードのばらつきは 0.5% 未満。

手元の RTX 5080 16 GB での実行は放棄した (1 層が CPU に載ってホストの CPU を飽和させ、カードの 5 GB が実行の外で握られていた)。その数値はどこにも使っていない。

## Learnings

### 1. vLLM — MTP のデコード速度と受理長

デコード tok/s (novel / edit、3 回の中央値):

| build | N=0 | N=1 | N=2 | N=3 | N=4 |
|---|---|---|---|---|---|
| 4B | 66.4 / 65.9 | 102.5 / 106.7 | 116.0 / 140.3 | 128.2 / 165.5 | 128.4 / 184.1 |
| 2B | 144.4 / 142.6 | 190.7 / 205.6 | 218.1 / 257.3 | — | — |
| 0.8B | 321.5 / 318.0 | 430.5 / 429.8 | 515.2 / 535.7 | — | — |

- 4B の平均受理長: N1 1.95、N2 2.71、N3 3.47、N4 4.07。受理率は 0.95 / 0.85 / 0.82 / 0.77。
- 2 ストリーム同時の合計 tok/s (4B): 124 → 196 (N1) → 217 (N2) → 232 (N3)。
- 構造化 tool call (`read_file`) はどの構成でも返った。
- Model Runner V2 ("Using V2 Model Runner") は MTP でも維持される。

### 2. vLLM — MTP のメモリ: プロファイル行と予約の較正

起動時のプロファイル行 (N ごとに 1 回起動、util 0.85)。Model loading GiB / Available KV cache GiB / プールのトークン数 / CUDA graph GiB / アクティベーションのピーク GiB / attention のブロックサイズ:

- 4B: N0 8.61 / 8.90 / 567,464 / 0.06 / 2.20 / 1056、N1 8.86 / 8.56 / 479,581 / 0.12 / 2.27 / 1056、N2 8.86 / 8.47 / 469,207 / 0.18 / 2.36 / 1072、N3 8.86 / 8.47 / 463,793 / 0.18 / 2.37 / 1072、N4 8.86 / 8.44 / 456,474 / 0.18 / 2.40 / 1088。
- 2B: N0 4.25 / 13.26 / 2,257,989、N1 4.38 / 13.01 / 1,875,258、N2 4.38 / 12.93 / 1,844,333。
- 0.8B: N0 1.72 / 16.20 / 2,758,256、N1 1.77 / 16.05 / 2,313,885、N2 1.77 / 15.94 / 2,273,296。
- 同じ構成の再起動は今回は同じプールを報告した (前回の pin 移動の記録は 30〜40% の揺れを見ている; 0500 §8)。

proto の予約はこれで較正した: **256 MiB + draft 1 トークンあたり 128 MiB**、それに MTP 層の KV キャッシュのトークンあたりの価格 (`mtp_kv_bytes_per_token_fp16`: 4B 4096、0.8B / 2B 2048。1 層 × KV ヘッド数 × head_dim 256 × 2 × 2 バイト) をコンテキストウィンドウの全トークンに掛ける。

- ハイブリッドモデルのプールのトークン数は max-model-len に依存する (同じ 1.91 GiB で、53,248 なら 93,184、84,992 なら 99,157)。
- 24 GB での提供コンテキストウィンドウは変わらない (262,144 の上限に掛かる)。上限を外した見積りは下がる (4B で 518,144 → N2 の 432,128)。

### 3. vLLM — 16 GB 級の予算での見積りの検証

同じカードで 16 GB 級の予算を模した (util 0.5664 = 0.85 × 16,303 MiB)。4B で見積りが決めたコンテキストウィンドウ 84,992 (N0) / 53,248 (N1) / 46,080 (N2) はすべて起動し、プールは 135,791 / 93,184 / 82,106。予約を掛けない N1 を 84,992 で起動してもプール 99,157 で起動したが、予約を掛けない N2 は 84,992 を 82,106 のプールに求めることになる。つまり予約が無ければ、16 GB 級の予算で 4B の 2 トークン draft は入らないコンテキストウィンドウを約束していた。

### 4. vLLM — MTP の TTFT コスト

118,098 と 240,598 トークンの長いプロンプトを cold で送り、続けて同一のものを再送した (各 3 回、ばらつき 2.2% 以下)。最初のトークンまでの時間 (TTFT):

- 4B N0: cold 25.47 / 81.04 s、再送 0.42 / 0.80 s (未キャッシュ 882 / 886 トークン)。
- 4B N1: cold 27.50 / 88.93 s (+8% / +10%)、再送 0.73 / 1.33 s (未キャッシュ 1,938 / 1,942)。
- 4B N2: cold 27.54 / 89.03 s、再送 0.52 / 1.14 s (未キャッシュ 1,250 / 1,542)。
- 2B N0: cold 9.61 / 30.84、再送 0.14 / 0.34。N1: cold 10.77 / 34.97 (+12% / +13%)、再送 0.28 / 0.57。
- 0.8B N0: cold 7.23 / 25.99、再送 0.13 / 0.33。N1: cold 8.20 / 29.77 (+13% / +15%)、再送 0.25 / 0.53。

MTP では送るたびにブロック 1 つ分 (約 1,056 トークン) が余計に再計算される。

### 5. vLLM — エージェント形の再送とプレフィックスキャッシュの retention interval (#1434)

Qwen3.5-4B で、コーディングエージェントの会話の形を再生した。S1: 1 つの会話を 1 ターンずつ (assistant の返答 + 約 4k トークンの tool 結果) 約 52k と約 105k まで育て、各サイズで次のターン、同一の再送 3 回、古い tool 結果 1 つを "[output cleared]" に置き換えた再送 (会話の約 1/3 と約 2/3 の帯で各 3 位置。スクリプトの丸めで 3 つのうち 2 つが一致したので、帯ごとに実質 2 位置)。S2: 3 つの会話を交互に約 170k ずつまで育て (プールが持てる量より多い)、その後 6 ラウンド。

- MTP 無し、retention 未指定 (vLLM の既定 0 = プロンプト末尾と接合点のチェックポイントを残す): 次のターン 1.48 s (52k) / 2.24 s (105k)、再送 0.21 / 0.26 s、約 1/3 の置換 6.36 s / 13.58 s。S2 の再計算 2,089,548 トークン、全ミス 10 回、TTFT p50 57.9 s。
- MTP 無し、`--prefix-cache-retention-interval 1056`: S1 の全数値が未指定の 1% 以内。
- MTP 無し、None (dense): S1 は未指定の 1% 以内。S2 の再計算 2,452,812 (+17%)、全ミス 12 回、p50 57.9 s。
- MTP N1、retention 未指定 (vLLM は "Hybrid model with EAGLE speculative decoding: defaulting prefix_cache_retention_interval to dense checkpointing" と記録する): 次のターン 1.80 / 2.74 s (+22%)、再送 0.42 / 0.59 s、置換 7.01 / 15.05 s。育てるターンの TTFT 中央値 1.52 s 対 1.24 s、再計算 7,928 対 6,949 トークン。S2 の再計算 3,488,784 (+67%)、全ミス 18 回、p50 63.3 s (+9%)。
- MTP N1、明示的に 0: 次のターン 3.11 / 5.15 s、最初の再送 1.81 / 2.74 s、育てるターンの TTFT 中央値 2.42 s、再計算 14,264。§4 の長いプロンプトでは同一の再送が何も当たらなかった (118,098 のうち 118,098 トークンを再計算、毎回 27.5 s)。vllm-project/vllm#53504 と一致する。

#1434 が当初狙った sliding-window の build (gpt-oss-20b / 120b、vLLM で sliding-window を持つ唯一の build) は、waired-ai/waired-agent#1416 (merge 済み) のオーナー裁定で後継無しに退役している。

### 6. ollama — draft のデコード速度 (CUDA 24 GB)

デコード tok/s (novel / edit)。全行で全層が GPU に載った:

| tag | d0 | d2 | d3 | d4 |
|---|---|---|---|---|
| 35B-A3B UD-Q2 | 154.4 / 149.9 | 239.5 / 262.3 | 238.1 / 275.0 | 220.0 / 292.4 |
| 35B-A3B UD-Q3 | 141.4 / 137.3 | 223.6 / 237.0 | 225.2 / 250.2 | 222.4 / 242.9 |
| 27B UD-Q2 | 41.1 / 40.4 | 66.8 / 78.6 | 68.3 / 89.3 | 71.4 / 96.4 |
| 27B UD-Q3 | 35.9 / 35.0 | 65.7 / 72.5 | 70.5 / 84.6 | 71.7 / 91.1 |

llama.cpp の fit の予測が draft 2 で draft 0 から増える量: 35B q2 +1,061、35B q3 +1,158、27B q2 +1,682、27B q3 +1,682 MiB。見積りの増分は +1,091 / +1,188 / +1,683 / +1,682。

### 7. ollama — draft のデコード速度 (Metal) と、あふれるホストでの draft

48 GB の M5 Pro、すべて GPU 上:

| tag | d0 | d2 | d4 |
|---|---|---|---|
| 27B UD-Q2 | 20.7 / 19.9 | 20.4 / 23.1 | 17.2 / 24.0 |
| 35B-A3B UD-Q2 | 75.1 / 74.0 | 88.7 / 99.4 | 96.6 / 122.4 |
| 27B UD-Q3 | 17.5 / 16.7 | 19.3 / 21.1 | 18.7 / 23.3 |
| 35B-A3B UD-Q3 | 66.1 / 65.1 | 96.7 / 101.7 | 99.0 / 122.2 |

fit の予測が d0 → d2 で増える量: 27B q2 +2,495 MiB (モデル +335 の nextn の重み、再帰状態 150 → 449、draft の KV キャッシュ 784 MiB f16、draft の compute 1,077 MiB)。35B q2 +2,187 (nextn +308、再帰状態 63 → 188、draft の KV キャッシュ 392、draft の compute 1,361 — ollama がこの build に ubatch 2048 を選んだため)。見積りの増分: 27B +2,678 (約 180 過大)、35B q2 +1,694 (約 490 過小)。35B q2 d2 の合計では見積りが予測より 100 MiB 小さい。

16 GB の M4、27B UD-Q2 を 200,704 で: d0 は予測 13,397 MiB 対 空き 11,974、66 層のうち 38 層が GPU、4.0 tok/s。d2 は予測 15,892 対 空き 11,674、24 層、ウォームアップで 0.11 tok/s、400 トークンの生成は 30 分のタイムアウトに掛かった。**あふれるホストでは draft は速くならず、遅くなる。**

48 GB の Mac で 24 GB の予算を模した (`LLAMA_ARG_FIT_TARGET` を上げて使えるのを約 18 GB にした): 27B q2 は d0 が 66/66 層で 20.7 / 19.5 tok/s、d2 が 50/66 層で 9.7 / 9.7 tok/s。35B-A3B q3 は d0 41/42 で 51.7 / 50.3、d2 42/42 で 63.7 / 64.7。35B-A3B q2 は d0 41/42 で 73.9 / 68.3、d2 42/42 で 69.3 / 77.1 (MoE の expert の配置があるので "offloaded N/M" の読みは dense ほど当てにならない)。

### 8. ollama — draft の挙動 (ソースと実機で確認)

- ollama が MTP の draft を走らせるのは、タグか要求が `draft_num_predict` を設定したときだけ (`server/routes.go`、v0.34.0)。unsloth のタグはこれを公開していない。library の `-mtp-` タグは持つ (35b-a3b 2、27b 3 / 4)。
- `DraftNumPredict` は runner のオプション (`api.Runner`) なので、タグのパラメータを変えると次の要求でスケジューラが runner をロードし直す (`server/sched.go` の needsReload)。実機で確認: 再 pull + draft 2 の書き込み → 次の要求の runner は `--spec-draft-n-max 2` (新しい pid)。再 pull + 書き込み無し → 次の runner に draft は無い。
- スケジューラは qwen35 と qwen35moe (ほかに qwen3next、qwen3vl、mllama、lfm2、nemotron_h など) を `OLLAMA_NUM_PARALLEL` に関わらず 1 スロットで起動する (`server/sched.go` の load; waired-ai/waired-agent#1423)。unsloth の 2 ファミリはどちらも qwen35 / qwen35moe。
- ollama はタグの draft を落として場所を空けることはしない。llama.cpp の fit が層を CPU に移す。
- 存在するタグへの `ollama pull` はマニフェストを公開時のパラメータに戻す (書き込んだ draft は消える)。`ollama create <tag>` で `FROM <tag>` はパラメータをマージするので、公開されている stop パラメータは書き込み後も残り、PARAMETER を省いても既存の draft は消えない (消すのは再 pull だけ)。
- draft 2 を書いた状態で、unsloth の 2-bit の 2 タグはコーディングエージェントの 6 つの要求の形 (leading-system、no-system、trailing-system、double-system、system-after-tool-roundtrip、developer-turn) すべてに 200 を返した。対照も OK。
- runner のコマンドラインには `--spec-type draft-mtp --spec-draft-n-max N --spec-draft-backend-sampling` が載る。

### 9. VRAM の見積りとの差

- vLLM の MTP: 予約が無ければ、見積りは 16 GB 級の予算で 4B の 2 トークン draft に入らないコンテキストウィンドウを約束していた (84,992 対 プール 82,106; §3)。
- ollama の draft (CUDA): 見積りは fit の増分と 30 MiB 以内で一致する (24 GB のカード 1 枚)。前回の 2 GPU の WSL2 機での読み (0500 §12) は固定部分が約 333 MiB 高かった。
- ollama の draft (Metal): 27B は約 180 MiB 過大、35B-A3B q2 は約 490 MiB 過小。ollama が ubatch 2048 を選び (見積りは 512 を仮定)、draft の compute バッファがそれに比例するため。
- ollama の既存の見積りは Metal では draft 無しでも fit の予測 (fit target 込み) より 400〜1,100 MiB 上にあるので、合計は安全側に留まる。
- Qwen3.5 2B / 0.8B の vLLM のプールのトークン数は 24 GB で、見積りのトークンあたりの価格から出る値より約 2% 少ない (ハイブリッドのページのオーバーヘッド)。そこではコンテキストウィンドウが 262,144 で頭打ちなので害は無い。

### 10. 製品に入った形

判断は決定記録にある。ここでは読む場所だけ:

- vLLM の speculative の選択は `router.VLLMSpeculative` (ngram が on ならそれ、`vllm_disable_mtp` か serve-flag gate の外か MTP の事実が無ければ無し、そうでなければ `{"method":"mtp","num_speculative_tokens":N}`)。コンテキストウィンドウの価格付けは `hostfit.VLLMMaxModelLenFor` が draft の長さを取る。
- カタログの事実は `catalog.Variant.MTPLayers` / `MTPKVBytesPerTokenFP16` / `MTPDraftTokens`。
- 切る側は `agentconfig` の `vllm_disable_mtp` (環境変数 `VLLM_DISABLE_MTP`、フラグ `--inference-vllm-disable-mtp`)。
- ollama の draft は `hostfit.OllamaDraftTokens` がホストごとに決め、`download.Rendering.DraftNumPredict` が pull 時に書く。ロード後は `cmd/waired-agent` の `draftToRewrite` が runner の draft と規則を比べ、`restampDraft` が (tag, draft) ごとにプロセスで 1 回だけ pull し直す。runner の draft は `proclist.RunnerFlags.SpecType` / `SpecDraftTokens` が読む。
- vLLM の argv が `--prefix-cache-retention-interval` を名指ししないことは `TestVLLMCommandArgs` の "never pins the prefix-cache retention interval" が pin する。

### 11. 方法上の注意 — 踏んだ罠

- **`&` で終わる ssh コマンドは `&&` の連鎖全体を stdin が /dev/null の背景に回す。** `cat > file` が空のスクリプトを書いた。
- **再生スクリプトの置換位置の丸めで 3 サンプルのうち 2 つが同じ位置になった** (§5)。位置は丸める前に重複を確かめること。
- **1 層が CPU に載った llama-server は共有の開発機で約 9 スレッドの CPU を使った。** そのホストの計測は捨てた。
- **`ollama create FROM <tag>` は PARAMETER を省いても既存の draft を残す。** draft の変更を試すときは製品と同じ手順 (pull してから書く) で回すこと。

## Refs
- https://github.com/waired-ai/waired/issues/1432
- https://github.com/waired-ai/waired/issues/1433
- https://github.com/waired-ai/waired/issues/1434
- https://github.com/waired-ai/waired-agent/pull/1416
- https://github.com/waired-ai/waired-agent/issues/1423
- https://github.com/vllm-project/vllm/issues/53504
- docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md
- docs/decisions/20260917/0310-vllm-serves-qwen35-with-two-mtp-draft-tokens.md
- docs/decisions/20260917/0320-ollama-mtp-draft-is-written-per-host.md
- docs/decisions/20260917/0330-no-prefix-cache-retention-flag.md
