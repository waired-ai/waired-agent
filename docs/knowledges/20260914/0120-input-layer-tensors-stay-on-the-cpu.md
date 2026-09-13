# 入力層のテンソルは空き VRAM に関係なく CPU に置かれる (20260914 01:20)

## Issue

`qwen3.8-flash-next`（llama.cpp のアーキテクチャ `qwen4exp`）を 128 GB のユニファイドメモリ機で serve すると、`load_tensors: offloaded 49/49 layers to GPU` なのに `CPU_Mapped model buffer size` が 26.8 GiB あり（blob 73.44 GiB の 3 分の 1）、llama.cpp の auto-fit は空きを 42.6 GiB 残したまま「no changes needed」と言う（#1307 のエンジンログ）。メモリ圧ではない。カタログの `estimated_weight_gb` はこの 3 分の 1 を GPU に要る量として数え、`/api/ps` は数えない（`docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md`）。

## Learnings

### 1. llama.cpp は入力層のテンソルを常に CPU に置く

`src/llama-model.cpp`（`load_tensors`）: 「there is very little benefit to offloading the input layer, so always keep it on the CPU」。`dev_input` は CPU デバイスに固定され、`-ngl` や `--fit` の対象外。`src/llama-arch.cpp` のテンソル表で `LLM_TENSOR_LAYER_INPUT` に分類されるもの（`token_embd`、`per_layer_token_embd`。演算は `GGML_OP_GET_ROWS`）がこれに当たる。表引きなので GPU に載せても速くならない、という設計判断で、空き VRAM の量は見ていない。

### 2. `qwen4exp` の PLE テーブルが大きい

`src/models/qwen4exp.cpp`: PLE（n-gram ハッシュ埋め込み）は、各トークンについて直前数トークンの n-gram をハッシュし、共有テーブル `per_layer_token_embd` から `ple_n_heads` 行を引いて（`llm_graph_input_ple`）、線形注意の層の小さな key / value / conv1d に通す。テーブルは `[ple_head_dim, n_rows]` の 1 枚で `TENSOR_READ_LAZY`（mmap、必要な行だけページイン）。層ごとの `ple_key` / `ple_value` / `ple_conv1d` は繰り返し層なので GPU に載る。大きいのはテーブルだけで、UD-Q2_K_XL では 26.8 GiB。

### 3. 何に数えられ、何に数えられないか

| 見る先 | この 26.8 GiB |
|---|---|
| `load_tensors: offloaded N/M layers` | 数えない（入力層は層数に入らない） |
| `common_params_fit_impl` の projected | 数えない（device の予算だけ） |
| `/api/ps` の `size` / `size_vram` | 数えない |
| `CPU_Mapped model buffer size` | ここに出る |
| 物理メモリ | ページキャッシュに要る。収まらないと SSD を読み続ける（#837 と同じ症状の別経路） |

### 4. waired への含意

- `estimated_weight_gb` は「GPU に要る量」と「システム RAM に要る量」を 1 つに潰している。ディスクリート GPU では VRAM を過大に、RAM を過小に見積もる。ユニファイドメモリ機では同じ DRAM なので合計だけが問題。
- 推奨は device 側（`estimated_weight_gb` − host 側）で、容量は合計に加えて「host 側 ≤ システム RAM の予算」で判定する（#1337 の追記、決定記録 `docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md` 決定 11）。
- 常駐の witness は `/api/ps` ではなくエンジンのログ（`CPU_Mapped model buffer size`、`offloaded N/M layers`、fit の行）。
- 値は GGUF のヘッダから取れる（入力層に分類されるテンソルの合計）。blob 全体を取得しなくてよい。テンソル型の読み取りも同じ手口（HTTP の Range で先頭 16.8 MB だけ。waired-ai/waired#1357）。

## Refs

- #1337（L113、VRAM 見積りの較正）/ #1305（サイズと digest）/ #1349（カタログ）/ #1307（エンジンログ）/ #837
- waired-ai/waired#1357（Stage 0 §6 と 2026-09-13 のコメント）
- llama.cpp `src/llama-model.cpp`（`load_tensors`）、`src/llama-arch.cpp`（`LLM_TENSOR_LAYER_INPUT`）、`src/models/qwen4exp.cpp`（PLE）
- 関連: `docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md`、`docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md`、`docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md`
