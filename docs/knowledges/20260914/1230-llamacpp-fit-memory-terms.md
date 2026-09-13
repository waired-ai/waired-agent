# llama.cpp の fit が数えるメモリは GGUF ヘッダから導ける (20260914 12:30)

## Issue

waired-agent#1337（L113）。製品の VRAM 見積りは「重み + KV×0.5 + (1024 + 40×W) MiB」に `OllamaSpillCalibration = 3.0` を掛けて spill を予測していた。24 GB 級 GPU × `qwen3.8-27b` MTP-Q4 × 200,704 トークン × KV q8_0 で、予測 11% に対し llama.cpp の fit は 66 層のうち 13 層を CPU に置いた。何が欠けているかを、fit と buffer のログ行と GGUF ヘッダで項ごとに突き合わせた。

## Learnings

### 1. ollama 0.31.1 以降は配置を llama.cpp の fit が決める

ollama は `-ngl` を渡さず（`llm/llama_server.go`、NumGPU の既定 -1）、llama.cpp の `--fit` が no_alloc の dry run で各バッファを測り、デバイスごとに

```
model + context(KV + recurrent state) + compute + extra model(MTP draft) ≤ free − target
```

を満たすまで**後ろのブロックから**デバイスに詰める（`common/fit.cpp`「filling dense layers back-to-front」）。だから CPU に残るのは前のブロック。MoE は expert テンソルだけを先に CPU へ出す。`target` は既定 1 GiB で、ollama は projector をオフロードするとき `LLAMA_ARG_FIT_TARGET = ceil((projector + 1 GiB) / MiB)` に上書きする（`mmprojFitTargetMiB`）。projector は target の中にロードされるので、「projector 込みの重み + 1 GiB」で数えれば同じ不等式になる。

ログで読む行（`--log-verbosity 4 --no-log-prefix`、ollama の engine.log に入る）:

```
common_params_fit_impl: projected to use 21723 MiB of device memory vs. 23002 MiB of free device memory
common_params_fit_impl: cannot meet free memory target of 1936 MiB, need to reduce device memory by 658 MiB
load_tensors: offloaded 63/66 layers to GPU
load_tensors:   CPU_Mapped model buffer size =  1403.76 MiB
load_tensors:        CUDA0 model buffer size = 14617.70 MiB
llama_kv_cache: size = 3528.00 MiB (200704 cells,  16 layers,  1/1 seqs), K (q4_0): 1764.00 MiB, V (q4_0): 1764.00 MiB
llama_memory_recurrent: size =  748.12 MiB (     1 cells,  64 layers,  1 seqs  4 rs_seq), R (f32):   28.12 MiB, S (f32):  720.00 MiB
```

`M` は `block_count + 1`（出力層）。MTP の第 2 コンテキストは主コンテキストの後にもう一度 `llama_context` / `llama_kv_cache` を出すので、最初のものが主。

### 2. 項ごとの導出（すべてログと 0.01 MiB 単位で一致）

| 項 | 導出 | 27B の値 |
|---|---|---|
| KV | `kv_bytes_per_token_fp16 × cells × factor`。factor は ggml のブロック: q8_0 = 34/64、q4_0 = 18/64 | q8_0 6,664 / q4_0 3,528 MiB @200,704 |
| 再帰状態（1 シーケンス） | GGUF の `ssm.*` から。n_linear = (block_count − nextn) − full、R = n_linear·(conv_kernel−1)·(2·group_count·state_size + inner_size)·4、S = n_linear·time_step_rank·state_size·(inner_size/time_step_rank)·4 | R 5.625 + S 144 = 149.625 MiB |
| 再帰状態（MTP） | × (1 + draft 数)（`n_rs_seq = 4`） | 748.125 MiB |
| draft の KV | full-attention 1 層分、**KV 種別に関係なく f16** | 4,096 B/token（784 MiB @200,704） |
| 入力層（CPU 固定） | `token_embd`（と `per_layer_token_embd`）のテンソル表のバイト合計。型はビルドごとに違うのでヘッダを読むしかない | Q4_K 682.03 / Q3_K 521.00 / Q2_K 397.85 MiB = `CPU_Mapped` |
| デバイス上の重み | テンソル合計 − 入力層 −（draft が動かなければ nextn ブロック）+ projector | UD-Q3: 12,526.9 − 521 − 334.7 + 888 = `CUDA0` 11,671 + 888 |

GGUF のヘッダとテンソル表はファイルの先頭にあるので、レジストリの blob を先頭数 MB だけストリームで読めば全部取れる（`catalog-tool layout --tag`）。79 GB のモデルでも数秒。

### 3. MTP の draft はタグの params が決める

GGUF に nextn ブロックがあっても、ollama が draft を動かすのは**タグ（またはリクエスト）が `draft_num_predict` を持つときだけ**（`server/routes.go` の `modelOptions`: 設定が無ければ `DraftNumPredict = 0`）。library の MTP タグは params レイヤに `draft_num_predict`（qwen3.8-27b 4、qwen3.6-27b 3、qwen3.6-35b-a3b 2）を持ち、hf.co の UD ビルドは持たない。後者は nextn テンソルもロードせず、再帰状態は 1 倍、draft コンテキストも無い。「MTP ビルドかどうか」は variant_id の接頭辞でも GGUF の `nextn_predict_layers` でもなく、タグの params で読む。

### 4. 実測で決まる項

compute バッファ（主・draft）とプロセスのデバイスコンテキストはヘッダから導けない。24 GB 級 CUDA で、主 compute は full offload で 1,060 MiB @200,704、full-attention 層の KV が CPU に出ると 1,500〜1,532 MiB に増える。nvidia-smi の空きと fit の free の差は非 MTP で約 395 MiB、MTP でさらに約 600 MiB。別バックエンドの値は waired-agent#1337 の計測を参照。

### 5. `/api/ps` は配置の証拠にならない

`size` / `size_vram` は ollama が buffer 行を自前で解析した値で、draft コンテキストと `CPU_Mapped` を数えない。MTP-Q4 で実 GPU 使用 21.2 GB を 14.97 GB と報告し、入力層が 3 分の 1 を占めるモデルを 100% GPU と報告する（`docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md`）。配置は `load_tensors` の行で読む。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1337
- `docs/decisions/20260914/1200-vram-sizing-follows-the-fit.md`
- `docs/knowledges/20260914/0120-input-layer-tensors-stay-on-the-cpu.md`
- `docs/knowledges/20260803/1327-hybrid-attention-kv-from-gguf.md`
- ggml-org/llama.cpp `common/fit.cpp`、`gguf-py/gguf/constants.py`（GGML_QUANT_SIZES）
- ollama v0.33.3 `llm/llama_server.go`、`server/routes.go`、`api/types.go`
