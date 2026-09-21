# ハイブリッドモデルの KV キャッシュは 1 本とは限らない (20260906 21:00)

## Issue

カタログは 1 モデルにつき `kv_bytes_per_token_fp16` を 1 つ持ち、`hostfit` の
容量判定・推奨判定・エンジン起動時のコンテキストウィンドウ決定は全部この 1 つの
数から予算を組む。`internal/catalog/scoring/catalog_kv_test.go` の
`hybridArchConfigs` は、その数を層数から再導出できることを保証している。

L100 で入れた `qwen3.8-flash-next` (qwen4exp) を実機で 262,144 トークンで立てたら、
エンジンが **KV キャッシュを 2 本**確保した。表が導出しているのは 1 本目だけで、
2 本目 (9216 B/token) には項が無かった。waired-agent#1255 はこの穴として起票され、
「MTP head かもしれない・導出できない・38% 過小」と書かれた。**3 つとも誤りだった。**

## Learnings

### 1. 実測の内訳

Strix Halo のホスト (Windows / Strix Halo / 128 GB UMA / ollama 0.33.3)、`num_ctx` 262,144:

```
llama_kv_cache:      6144.00 MiB (262144 cells, 12 layers)  K (f16) 3072.00  V (f16) 3072.00
llama_kv_cache:      2304.00 MiB (262144 cells, 12 layers)  K (f16)  768.00  V (f16) 1536.00
llama_memory_recurrent: 112.57 MiB (1 cells, 48 layers)
```

3 本ある。1 本目が通常の attention、2 本目が **QSA (Qwen Sparse Attention) の
indexer キー**、3 本目が線形層の再帰状態でコンテキスト長に依存しない。

2 本目は K と V で幅が違い、両方とも fp16 で、両方とも正確に導出できる:

| | 式 | B/token | @262144 |
|---|---|---|---|
| K | 12 層 × 1 ヘッド × `indexer_head_dim` 128 × 2 B | 3072 | 768 MiB |
| V | 12 層 × 1 ヘッド × `head_dim` **256** × 2 B | 6144 | 1536 MiB |

**9216 が「導出できない」と読めたのは、K と V が同じ幅だと仮定して
`2 × L × h × d × 2` に当てはめようとしたから。**幅が違うのがこのキャッシュの
特徴なので、`L × (128 + 256) × 2 B` と書けば一致する。

層数 12 は full-attention 層数と同じ。llama.cpp が indexer のレイヤーフィルタを
attention と同一の述語から作っているため (`src/llama-model.cpp`、
コメントは "QSA runs on the dense-attention layers only")。

### 2. V の側は確保されるだけで、書かれも読まれもしない

pin 中の b10760 のソースで 3 方向から確認した。

- **モデルに value 射影が無い。** `src/llama-model.h:559-562` のレイヤー構造体は
  `index_q_proj` / `index_k_proj` / `index_q_norm` / `index_k_norm` の 4 本だけで、
  `index_v_proj` は存在しない。しかも `index_k_proj` は `{n_embd, idx_dim}` で、
  キーは 1 ヘッド。
- **グラフが触らない。** `src/models/qwen4exp.cpp:586,589` は indexer のコンテキストに
  対して `cpy_k` と `get_k` しか発行しない。`cpy_v` / `get_v` は同じファイルの
  712 / 745、つまり通常の attention にしか出てこない。
- **それでも確保される。** `llama_memory_hybrid_idx` は汎用の `llama_kv_cache` を
  使い回す。`src/llama-kv-cache.cpp:231` の `has_v = !is_mla` は indexer に対して真。
  `src/llama-memory-hybrid-idx.cpp:51-52` が上書きするのは `n_head_kv_arr`→1 と
  `n_embd_head_k_full`→128 の 2 つだけで **V 側の幅は上書きしない**ため、V は
  モデルの `head_dim` 256 のまま確保される。K と V の幅が違う理由がこれである。

上流も同じ結論で、修正 PR が出ている: **ggml-org/llama.cpp#28330**
(ggml-org/llama.cpp#28296 に対する修正、2026-09-06 時点で open)。曰く
"`llama_kv_cache` by default also allocates memory for V cache (unless the model is
a MLA model), which results in wasted memory."。indexer のキャッシュを MLA として
見せることで `has_v` を偽にする。**b10760 にも master にも入っていない**(両者この箇所は同一)。

無駄になっている量は 6144 B/token = **全 KV の 18.2%**。262,144 で 1,536 MiB、
コーディングエージェントの床 200,704 で 1,176 MiB。

### 3. だからカタログは 27648 を書き、33792 を書かない

```
24576  attention                    ← 従来の導出
+3072  indexer のキー               ← 本物。モデルの config から導出できる
=27648  ← 注記する値
+6144  indexer の V                 ← 上流の過剰確保。#28330 で消える
=33792  ← 今日エンジンが実際に持っている量
```

`kv_bytes_per_token_fp16` に 33792 を書くと、今日のエンジンとは一致するが
2 つを失う。層数から再導出できなくなる (`hybridArchConfigs` が存在する理由そのもの)
のと、#28330 が着地した瞬間に今度は過大見積もりになるのと。上流のバグを前提に
数字を合わせると、バグが直ったときに一緒に直す人が要る。

代わりに払っているのは、**#28330 が入るまでの間だけ 6144 B/token 少なく見積もる**
こと。実効では `servingKVCacheDivisor`(q8_0 の KV)で半分になり、200,704 の床で
約 588 MiB。このモデルは `min_ram_gb` 128 なので、影響するのは 128 GB 以上の
ホストのコンテキストウィンドウの段だけ。

### 4. 次に pin を動かす人が見ること (済 — 2026-09-21)

**この節は役目を終えた。**ここに書いた 3 つの手順は
waired-ai/waired-agent#1472 (ollama 0.34.2 / llama.cpp b10969) で実行され、
結論が出ている。手順そのものは記録として残すが、やることはもう無い。

- #28330 は `311d4211b` として閉じ、**b10889 が初出**。0.34.2 の同梱は b10969 で、
  これを含む。
- 8192 cells で立て直した実測: 2 本目は `24.00 MiB (8192 cells, 12 layers), K (f16):
  24.00 MiB, V (f16): **0.00 MiB**`。**V 半分は消えた。**割り戻すと attention
  24,576 B/token + indexer key 3,072 B/token = **27,648 B/token** で、カタログの注記と
  一致する。注記は変えていない。
- HIP の decode 崖 (llama.cpp#27856) も閉じている。修正は #27466 (`f8dbcd6`) で
  **b10720 が初出**なので、**b10760 の時点で既に入っていた** — この節を書いた時点の
  「open」という認識のほうが古かった。

実測の全文は `docs/knowledges/20260920/1400-engine-pins-0342-and-uv-01217.md` §7。

**追記(2026-09-22)。** pin は 0.34.0 (b10760) に戻った(waired-ai/waired-agent#1505。
0.34.2 は `hf.co/` のタグを新しく pull できない)。そのため、配信されるエンジンは
このモデルで再び V 半分を確保し、33,792 B/token になる。カタログの注記は、この節の結論どおり
27,648 のまま動かさない。次に b10889 以降のエンジンへ上げたとき、何も触らずに差が消える。

### 5. 一般化: 起動ログの `llama_kv_cache` 行を数える

新しいアーキテクチャを採るとき、「KV キャッシュは 1 本」は前提にできない。
`hybridArchConfigs` に行を足す前に、実機の engine.log で `llama_kv_cache:` と
`llama_memory_` で始まる行を**数える**。2 本以上あったら、それぞれの
`(cells, layers)` と `K (f16) / V (f16)` から B/token を割り戻し、モデルの
config.json のどのフィールドの積になるかを突き合わせる。割り戻せない残りが
あるうちは、その数字はまだ理解されていない。

K と V が同じ幅とは限らない、というのがこの回の具体的な教訓である。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1255
- https://github.com/waired-ai/waired-agent/issues/1192
- https://github.com/ggml-org/llama.cpp/pull/28330
- https://github.com/ggml-org/llama.cpp/issues/28296
- https://github.com/ggml-org/llama.cpp/pull/27742
- internal/catalog/scoring/scoring.go (`KVBytesPerTokenFP16ForConfig`)
- internal/catalog/scoring/catalog_kv_test.go (`hybridArchConfigs`)
- docs/knowledges/20260906/1700-what-to-recheck-about-amd-backends.md
- docs/knowledges/20260803/1327-hybrid-attention-kv-from-gguf.md
