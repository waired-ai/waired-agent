# hybrid-attention モデルの KV/token を GGUF から測る (20260803 13:27)

## Issue

waired-agent#448 の検証。`kv_bytes_per_token_fp16` は waired-ai/waired#1031 で
**ルーティング入力**になった(`hostfit.ServingWindowKVMB` /
`OllamaWindowResidentMB` が、ノードが名乗る serving window を値付ける)ので、
過小申告はホストに「保持できない窓」を宣言させる。

qwen3.5-4b が 12288 を名乗っていたが、これは 2b 兄弟の値。実測すると 32768。

hybrid-attention(Gated DeltaNet + full attention)モデルの KV は
**パラメータ数の関数ではない** — 伸びるキャッシュを持つのは full-attention 層
だけで、linear/Mamba 層は固定サイズの再帰状態を持つ。この性質を知らずに
「サイズが近いから兄弟と同じだろう」と書くと #448 の転記ミスになる。

## Learnings

### 1. GGUF ヘッダの per-layer 配列がそのまま答え

llama.cpp は hybrid モデルの `<arch>.attention.head_count_kv` を
**層ごとの配列**で書く。ゼロの要素が linear 層なので、**非ゼロの数がそのまま
full-attention 層数**になる。間隔の推定は要らない。

```
qwen35.block_count             = 32
qwen35.attention.head_count_kv = [0,0,0,4, 0,0,0,4, ...]   → 非ゼロ 8 個
qwen35.attention.key_length    = 256
```

```
2 (K と V) × 8 層 × 4 KV heads × 256 head_dim × 2 bytes = 32768
```

`head_dim` は `hidden_size / n_heads` で割り出さず **`attention.key_length` を
読む**。qwen3.5 系は hidden_size がファミリ内でばらつくのに key_length は 256
で一定なので、割り算だと 4b だけ別の値になる。

### 2. 罠: `-mtp-` ビルドは head_count_kv を**スカラ**で出す

`qwen3.6-*-mtp-*` の GGUF は配列ではなくスカラを書く。これは
「**配列が無い**」であって「**全層が full attention**」ではない。素朴に
`block_count × head_count_kv` すると 27b-mtp が 266240 という嘘の値になる
(実際は非 mtp と同じ 65536)。mtp ビルドは非 mtp と同じ attention 幾何を持つ
ので、配列が無ければ**同ファミリの非 mtp ビルドを読む**。

### 3. `/api/ps` の `size` は KV の測定器として間違っている

`size` には **compute buffer が含まれ、それも `n_ctx` でスケールする**
(このボックスで 8k → 628 MiB、32k → 2312 MiB)。2 つの `num_ctx` で `size` を
差分すると **104,623 B/token** が出た。真値 32768 の **3.2 倍**で、しかも
もっともらしい桁に見える。

正しくは **runner ログの `llama_kv_cache:` 行**を読む。KV 割り当てを厳密に
述べており、hybrid なら full-attention 層数まで無料で付いてくる:

```
llama_kv_cache:       size = 1024.00 MiB ( 32768 cells,  8 layers, 1/1 seqs), K (f16): 512.00 MiB, V (f16): 512.00 MiB
llama_memory_recurrent: size =   50.25 MiB (     1 cells, 32 layers, ...)
```

`1024 MiB / 32768 cells = 32768 B/token`。**2 つの `n_ctx` で回して
per-cell が一致すること**が、「KV 項を読んでいる」ことの証明になる
(相関するだけの何かを読んでいないことの区別)。`llama_memory_recurrent` が
両方で不変なのは、linear/Mamba 層の状態が文脈長に依存しないことの直接確認。

`OLLAMA_KV_CACHE_TYPE=f16` で回すこと。manifest の値は fp16 基準。

### 4. 測定に GPU を使うことは避けられない

ここの ollama は **Vulkan バックエンド**を使うため、`CUDA_VISIBLE_DEVICES=""`
では GPU から降りない:

```
llama_prepare_model_devices: using device Vulkan0 (NVIDIA RTX PRO 4000 Blackwell)
load_tensors: offloaded 34/34 layers to GPU
```

「CPU only のつもりの probe」が黙って VRAM を数 GB 取る。共有マシンでは先に
確認する。

### 5. 測定結果(バンドルカタログの hybrid 全ファミリ)

| model_id | full-attn / 全層 | KV heads | 導出 | manifest(修正前) |
|---|---|---|---|---|
| qwen3.5-0.8b | 6 / 24 | 2 | 12,288 | 12,288 ✓ |
| qwen3.5-2b | 6 / 24 | 2 | 12,288 | 12,288 ✓ |
| **qwen3.5-4b** | **8 / 32** | **4** | **32,768** | **12,288 ✗** |
| qwen3.5-9b | 8 / 32 | 4 | 32,768 | 32,768 ✓ |
| qwen3.5-27b | 16 / 64 | 4 | 65,536 | 65,536 ✓ |
| qwen3.6-27b | 16 / 64 | 4 | 65,536 | 65,536 ✓ |
| qwen3.5-35b-a3b | 10 / 40 | 2 | 20,480 | 20,480 ✓ |
| qwen3.6-35b-a3b | 10 / 40 | 2 | 20,480 | 20,480 ✓ |
| qwen3.5-122b-a10b | 12 / 48 | 2 | 24,576 | 24,576 ✓ |
| qwen3-coder-next-80b-a3b | 12 / 48 | 2 | 24,576 | 24,576 ✓ |

122b は 70 GB あるので pull せず、レジストリ blob への **ranged GET** で
ヘッダだけ取った。ずれていたのは 4b の 1 件だけ。

4b は実エンジンでも確認済み: `n_ctx` 8192 と 32768 の両方で
`llama_kv_cache` の per-cell が **32768 ちょうど**。

### 6. スコープ外: 非 hybrid の 4 件

同じ導出を dense / sliding-window 系に当てると 4 件ずれる
(gpt-oss-20b, gpt-oss-120b, qwen2.5-coder-7b, qwen3-coder-30b-a3b)。
ファミリごとの規約決定が要るので #448 では触っていない。
`internal/catalog/scoring/catalog_kv_test.go` は
`attention_arch == "hybrid_mamba"` にスコープを切ってある。

## Refs
- https://github.com/waired-ai/waired-agent/issues/448
- `internal/catalog/scoring/catalog_kv_test.go` — 導出表と、新しい hybrid が
  層数を書かずに追加されたら落ちる完全性チェック
- `internal/catalog/scoring/scoring.go` — `KVBytesPerTokenFP16` の式
- docs/reference/models.md — 生成物(`make catalog-docs`)
