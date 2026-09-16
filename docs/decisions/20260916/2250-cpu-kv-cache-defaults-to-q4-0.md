---
status: accepted
supersedes:
  - docs/decisions/20260727/1715-ollama-kv-quant-only-when-it-buys-ctx.md
  - docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md
---

# CPU だけのホストも KV キャッシュの既定は q4_0 (20260916 22:50)

## Status
Accepted。オーナー裁定 2026-09-16(実装セッションでの判断)。

次の 2 つの記録の一部を置き換える。

- `docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md`: 決定 1 の「CPU-only は `f16` 固定(#29)」。ほかの決定はそのまま有効。
- `docs/decisions/20260727/1715-ollama-kv-quant-only-when-it-buys-ctx.md`: 「量子化 KV は文脈長を買えるときだけ要求する」規律。2355 が GPU のあるホストで外し、この記録が CPU だけのホストでも外すので、残る部分は無い。`f16` のときに `OLLAMA_FLASH_ATTENTION` を出力しない扱いは、ピンとして `f16` を指定した場合の実装に残る。

## Context

2355 の決定 1 は、CPU だけのホストの KV キャッシュを `f16` に固定していた。根拠は #29 で、CI の CPU ランナーで llama-server が segfault していた。当時は、使われることの少ない「CPU + フラッシュアテンション + 量子化 KV」の経路が疑われていた。ただし原因は確認されていない(1715 の Consequences に「消える保証はない」とある)。1715 は、`f16` では 2 スロット分のコンテキストウィンドウが入らない窮屈な CPU ホストで `q8_0` を残していた。2355 の決定 1 はこの例外を消していた。

ollama 0.34.0 で、CPU だけで計測した(2026-09-16)。

- 条件: 14 スレッドのハイブリッド CPU(P コア 6 + E コア 8)の開発機。CUDA / Vulkan / MLX のライブラリを外し、8 スレッドに固定した。コンテキスト長は 32,768 トークン。量子化 KV はフラッシュアテンション付き。
- 安定性: `q8_0` と `q4_0` をそれぞれ 202 リクエスト流し、失敗もクラッシュも 0 件だった。内訳は、Qwen3.5 の 0.8B・4B・9B で 2 並列のツール付きチャットを流した分と、0.8B で 150 回ずつ繰り返した分。1 リクエストあたりのクラッシュ率は、95% の信頼度で各 1.5% 以下。
- メモリ: KV の大きさは `f16` 比で `q8_0` が 0.53 倍、`q4_0` が 0.28 倍だった(4B・9B で 1,024 / 544 / 288 MiB)。価格付けの係数と一致する。
- 速度(各 1 回の測定で、ほかの作業と並走したので幅がある): 量子化 KV にすると、長いプロンプトのプリフィルが小さいモデルで遅くなった。0.8B の 14,834 トークンでは 233 から 138〜140 tok/s、4B の 10,955 トークンでは 61.7 から 41.2〜50.4 tok/s。9B の 7,235 トークンでは差が無かった。デコードは同じか、少し速くなった。

## Decision

**CPU だけのホストの KV キャッシュも、GPU のあるホストと同じ段(`q4_0` → `q8_0` → `f16`)に載せ、既定は `q4_0` にする。** build の `kv_cache_types` が `q4_0` を許さなければ `q8_0`、一覧を持たない build では `q8_0` になる。量子化 KV にはフラッシュアテンションを付ける。利用者は CPU だけのホストでも `q4_0` / `q8_0` / `f16` を選べる。

オーナーは、長いプロンプトのプリフィルが小さいモデルで遅くなることを知ったうえで、この形を選んだ。ほかの選択肢(`f16` を既定にして窮屈なときだけ量子化する、`f16` 固定のまま)は採らなかった。

## Consequences

- `hostfit.OllamaDefaultKVCacheType` はどのホストでも `q4_0` を返す。`hostfit.KVCacheChoices` は CPU だけのホストにも段全体を返す。serve tuning(`planOllamaKV`)、容量と推奨の価格付け、コンソールの KV の選択肢は、どれもこの 2 つを読むので揃って変わる。
- CPU だけのホストでは KV の見積りが小さくなる。そのため、提供できるコンテキストウィンドウが広がり、推奨されるモデルが重くなることがある。CPU では重いモデルほど遅い。
- nightly の「install+inference (linux)」レグの `WAIRED_OLLAMA_KV_CACHE_TYPE=q8_0` のピンは、`q8_0` の段を実エンジンで確かめるために残す。routing-sentinel レグは利用者と同じ設定(`q4_0`)を通る。
- #29 の segfault が再発したら、この記録を見直す。

## Refs

- waired-ai/waired-agent#29 / #1348
- `docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md`
- `docs/decisions/20260727/1715-ollama-kv-quant-only-when-it-buys-ctx.md`
- `proto/hostfit/kvcache.go`、`cmd/waired-agent/inference_ollama_tuning.go`
