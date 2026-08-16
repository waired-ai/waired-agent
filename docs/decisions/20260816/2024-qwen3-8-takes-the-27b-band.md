---
status: accepted
---

# 27B 帯は qwen3.8-27b に交代し、qwen3.6-27b は自動選定から外す (20260816 20:24)

## Status
Accepted

## Context

Qwen が 2026-08-14 に `Qwen/Qwen3.8-27B` を Apache-2.0 で公開した。カタログの
27B 帯は `qwen3.6-27b` が埋めている。`scripts/catalog-radar/prompt.md` が
「the catalog carries one generation per band on purpose」「a candidate is
interesting when it is worth replacing a generation we carry — a newer
generation of a family」と定めているとおり、同じ系列の次の世代が出た帯である。

マニフェストを書く過程で、想定していなかった事実が 4 つ出た。いずれも
この決定の形を変えている。

### 1. `catalog-tool compute` がこの世代の config を読めていなかった

Qwen は qwen3.5 / 3.6 / 3.8 を vision-language モデルとして配布しており、
`config.json` はデコーダを `text_config` の下に入れ子にする。
`scoring.ArchConfig` はフラットな構造体なので、`catalog-tool compute --repo`
はアーキテクチャのフィールドをすべて 0 として読み、`attention_arch=standard` /
`kv_bytes_per_token_fp16=0` を返していた。**出荷中の `qwen3.6-27b` が持つ
`hybrid_mamba` / `65536` を、今日のツールは再現できない。**

`kv_bytes_per_token_fp16` は waired-ai/waired#1031 でルーティング入力になって
いる（`hostfit.ServingWindowKVMB` / `OllamaWindowResidentMB` がノードの名乗る
serving window を値付ける）ので、0 は表示上の問題ではない。

### 2. `Qwen/Qwen3.6-27B-AWQ` は存在しない

出荷中の `qwen3.6-27b` の vLLM variant が指す HF リポジトリは実在しない。
`resolve/main/config.json` への HTTP ステータスで区別できる（実在は 307、
不在は 401）:

```
Qwen/Qwen3-32B-AWQ      -> 307   (実在)
Qwen/Qwen3.6-27B-AWQ    -> 401
Qwen/DoesNotExist-XYZ   -> 401
```

`GET /api/models?author=Qwen&search=27B` の結果にも無く、Qwen org は 27B 系列に
AWQ を一切出していない。`TestBundledManifests_AWQOrgConstraint` は `repo_id` が
`Qwen/` で始まることしか見ないので、誰も気づかなかった。

### 3. ollama 0.31.1 は qwen3.8 を pull できない

RTX PRO 4000 Blackwell 上の pin 版（0.31.1）で実測:

```
Error: pull model manifest: 412:
The model you are attempting to pull requires a newer version of Ollama.
```

upstream は v0.32.12（2026-08-14）でモデルを追加し、v0.32.13 で developer
instruction の扱いを直している。

### 4. ollama の 2 つの qwen3.8 タグは同一アーティファクト

`qwen3.8:27b-q4_K_M` と `qwen3.8:27b-mtp-q4_K_M` はレジストリ上で model blob を
共有し、違いは params レイヤの `draft_num_predict: 4` だけ。qwen3.6 では
非 mtp（17.42 GB）と mtp（16.84 GB）が別 blob だったので、そこが変わっている。

## Decision

**`qwen3.8-27b` を追加し、`qwen3.6-27b` を `manual_only` にする**（退役では
ない）。カタログに残り、id とエイリアスで解決し、pull でき、serve でき、
明示的な pin も動き続ける。自動選定・install pick・推奨バッジ・アップグレード
提案からだけ外れる。

退役にしないのは、`qwen3.5-27b` が `qwen3.6-27b` と併存しているのが今日の形
であること、および 18 GB を既にダウンロード済みのホストに何も再取得させない
ため。

派生する決定:

* **`ArchConfig` が `text_config` を読む。** 解決をデコード時に行い、呼び出し
  側が適用する `Resolve()` 型にはしない。`--config` 経路と
  `hfclient.FetchConfig` 経路の両方が構造体をそのままヘルパに渡すので、
  呼び忘れられる解決ステップは同じ無言のゼロを残す。修正後、
  `catalog-tool compute --repo Qwen/Qwen3.6-27B` は出荷中の値を警告なしで
  再現する。
* **`qwen3.6-27b` の vLLM variant を `Qwen/Qwen3.6-27B-FP8` に付け替える。**
  実在する公式量子化はこれだけ。FP8 は重み 30.9 GB なので 27B 帯の vLLM の
  下限は 24000 MB → 38912 MB に上がり、24 GB のカードには vLLM ビルドが
  無くなる。**これは後退ではない** — 24 GB のカードが昨日 vLLM を自動選択して
  いたとしても、解決先の重みは取得できなかった。実質は ollama へのフォール
  バックのままで、それを正直に言うようになっただけ。カバレッジの穴自体は
  #575 が追う。
* **ollama の pin を 0.32.13 に上げる。** これが無いと追加したエントリは
  waired がエンジンを入れる全ホストで pull できず、`qwen3.6-27b` を
  `manual_only` にした結果それらのホストは `qwen3.5-27b`（tier 67）へ
  **下がる**。floor は 0.32.12 ではなく 0.32.13 を採る: developer instruction は
  コーディングエージェントのシステムプロンプトが Anthropic→OpenAI 変換を
  通った先の姿そのもの。v0.32.14-rc0 は prerelease で、`version.AtLeast` は
  prerelease-blind（#804）なので採らない。variant の `min_engine_version` も
  同じ数字。
* **ollama variant は 1 本だけ載せる。** 同一 blob を 2 つの variant として
  並べても footprint も重みも同じで、agentgrade の測定回数だけが倍になる。
  mtp タグを採る（MTP ヘッドはどちらの重みにも入っており、params レイヤが
  自己投機デコードを有効にする分だけ得）。
* **quality_tier は 27B ブロックを 67〜72 に振り直す。** 新しい世代を自分の帯の
  最上段に置きつつ、他系列の rung（73 = qwen3.5-35b-a3b）を跨がないため。
  低→高で qwen3.5-27b(67) < qwen3.6-27b q4(68) < mtp(69) < fp8(70) <
  qwen3.8-27b mtp(71) < fp8(72)。既存の相対順序は 1 つも変わらない。
  `tier_override` も `benchmarks.json` の行も作らない: 許可ソース
  （LiveBench 2026-06-25 / SWE-rebench 2026-05-15..07-01）はどちらも 3.8 の
  公開前で引ける行が無く、同系列の世代交代は
  docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md が出典を
  要求する「系列をまたぐ配置」ではない。

## Consequences

* **`1427-quality-tier-is-a-curated-ladder.md` の「tier の絶対値は動かない」が
  失効した。** その決定自身が条件付きで書いている（「将来 tier-0 variant が
  `bundled/` に入るとこの不変性は失効する」）。入ったのが今回で、帯が詰まって
  いたため振り直しになった。
* **24 GB / 32 GB のカードに vLLM ビルドが無い。** `PickEngine` はそれらの
  ホストに ollama を名乗るので推論は失われない。`TestManualOnly_NoHostLosesItsPick`
  は強制エンジンではなく `PickEngine` が実際に名乗るエンジンで判定するように
  なった — 強制エンジンはホストが動かすものではない。#575 が穴そのもの。
* **`proto/hostfit` の size class 余裕チェックに例外表ができた。** FP8 の 27B は
  31,729 MiB で 32 GB 境界の 3.2% 下に入る。移す先が無い（Qwen が 27B 系列に
  int4 を出していない）ので `insideTheMargin` に理由付きで記録する形にした。
* **エンジン更新が全ホストに届く。** pin の引き上げは 0.31.1 → 0.32.13 の
  1 段ではなく 12 リリース分。定数の doc が要求する 2 点（release が
  sha256sum.txt を公開している / asset 名が変わっていない）は
  `TestPinnedReleasePublishesEveryAssetChecksum` を v0.32.13 に対して実行して
  確認済み。
* **agentgrade は 1 variant × 2 transport の実測が要る。** ci.yml の
  `agentgrade --check --require-pass` が塞ぐ。0.31.1 では pull すらできないので、
  測定ホストにも 0.32.13 が要る。

## Refs
- https://github.com/waired-ai/waired-agent/issues/823
- https://github.com/waired-ai/waired-agent/issues/575
- https://github.com/waired-ai/waired-agent/issues/518
- https://github.com/waired-ai/waired-agent/issues/520
- docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md
- docs/knowledges/20260803/1327-hybrid-attention-kv-from-gguf.md
- https://github.com/ollama/ollama/releases/tag/v0.32.12
- https://github.com/ollama/ollama/releases/tag/v0.32.13
