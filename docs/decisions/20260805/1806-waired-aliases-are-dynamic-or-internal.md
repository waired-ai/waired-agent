---
status: accepted
---

# 静的な `waired/*` 別名は退役し、残るのは動的解決か internal_only だけ (20260805 18:06)

## Status
Accepted

## Context

bundled マニフェストは 13 個の静的な `waired/*` 別名を宣言していた。棚卸しの結果、
**うち 11 個は消費者がゼロ**だった。

| 別名 | 所有 manifest | 消費者 |
|---|---|---|
| `waired/medium` | qwen2.5-coder-14b-instruct | なし |
| `waired/flagship` | gpt-oss-120b | なし |
| `waired/oss-small` | gpt-oss-20b | なし |
| `waired/moe-small` | qwen3-coder-30b-a3b-instruct | なし |
| `waired/dense-large` | qwen3.6-27b | なし |
| `waired/moe-mit` | glm-4.5-air-106b-a12b | なし |
| `waired/moe-mid` | qwen3-coder-next-80b-a3b-instruct | なし |
| `waired/moe-coding` | qwen3.6-35b-a3b | なし |
| `waired/moe-large` | qwen3-coder-480b-a35b-instruct | なし |
| `waired/moe-dual-gpu` | deepseek-v4-flash | なし |
| `waired/moe-frontier` | glm-5.2 | なし |
| `waired/small` | qwen2.5-coder-3b-instruct | openclaw `modelRefs()`、docs-site |
| `waired/tiny` | granite4-350m (`internal_only`) | e2e ハーネス `WAIRED_TINY_ALIAS`、`installtest-macos.sh` |

11 個が現れるのは 3 か所だけ — 宣言している manifest 自身、その宣言が解決することだけを
確認する手書きテスト表（`internal/catalog/manifest_test.go`）、生成物
`docs/reference/models.md` の `waired 別名` 列。**自分の存在を自分でテストしていた。**
private リポジトリ側も Go / TypeScript コードからの参照はゼロで、開発者向けドキュメントの
記述だけだった。

この名前空間はサイズ名ショートカットを提示するコーディングエージェント統合のために作られた
（`docs/decisions/20260516/2133-*`、private）。当時の消費者は openclaw と opencode の 2 つ。
opencode 統合は `e8918b8`（#333 / #355）で削除され、残る openclaw が allowlist するのは
`waired/default` / `waired/coding` / `waired/small` の 3 つだけになっていた。

## Decision

**静的な `waired/*` 別名は `internal_only` なモデルにのみ残す。** それ以外は退役させ、
製品が使い続ける名前はルーターがリクエスト時に解決する。

* 11 個の装飾別名を `model_aliases` から削除する
* `waired/small` を動的別名にする（`waired/default` / `waired/coding` と同じ機構、#632）
* `waired/tiny` は静的のまま残す

### なぜ付け替えではなく退役なのか

当初 #521 は `waired/flagship` を「実際に最上位のモデル」へ付け替える計画だった。
2 つの理由で成立しない。

**1. 付け替え先が存在しない。** 現在の flagship（gpt-oss-120b）は 80 GB VRAM ＝ H100 1 枚。
tier 上位の候補は deepseek-v4-flash が 192 GB、qwen3-coder-480b が 560 GB、glm-5.2 が
560〜1130 GB。桁が違う。private の `docs/quickstarts/README.md` は `waired/flagship` を
「H100 80GB ×1 で動く構成」として案内しており、付け替えるとその環境では 465 GB の pull が
始まって失敗する。**解決に成功してしまう分、「見つからない」より悪い壊れ方**になる。

**2. 名前そのものが #518 の規則に違反している。**
`docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md` は、系列をまたぐ配置には
能動更新の独立リーダーボードの出典を要求する。`flagship`（我々の最上位）・`moe-frontier`
（最前線）・`moe-large` はまさにその種類の主張であり、出典を持たない。とりわけ `flagship` は
**同じ PR で「推薦対象から外す」と決めた gpt-oss に付いていた**。

`moe-mit`（ライセンス）・`moe-dual-gpu`（必要 GPU 数）・`dense-large`（アーキテクチャ）は、
manifest が `license` / `min_vram_mb` / `param_count` として既に構造化データで持つ事実を
名前に二重符号化したもの。カタログが動けば名前だけが古くなる。

### `waired/small` が動的になる理由と、その定義

`waired/small` は唯一実消費者を持つサイズ名であり、同時に**次に壊れる名前**でもあった。
指している qwen2.5-coder-3b は #522 で退役する。openclaw が全ユーザーの設定に書き込むため、
消すことも、退役するモデルを指し続けることもできない。

役割名に変える。**このホストが既に持っている中で最も小さいモデル**、無ければコーディング既定
（`waired/default` と同じ解決）へフォールバックする。

「収まる中で最も小さいモデル」ではない点が意図的である。**ダウンロードされていないモデルは
速い応答ではなく、数 GB の待ち時間**であり、そこへ解決させると「軽い選択肢」が「遅い選択肢」に
なる。モデルを 1 つしか持たないホストではフォールバック先と同じ答えになる。

`manual_only` なモデルは候補から除く。これは製品がユーザーの代わりに答える経路であり、
指名ではないため（#521）。

### `waired/tiny` が残る理由

granite4-350m は `internal_only` で、`BundledManifests()` は返さない。この別名は e2e ハーネスと
installtest が名前で pin する**テスト用ハンドル**であって、カタログの約束ではない。
これが残る規則の形になっている: **静的な `waired/*` 別名は、我々が提供しないモデルにのみ許される。**

## Consequences

* **退役した 11 個は解決しなくなる。** 消費者はゼロだが、`waired/flagship` は private の
  quickstart に記載があるため、そちらを `gpt-oss-120b` 直指名に直す follow-up が要る。
  明示的に `ErrModelNotFound` で失敗する方が、巨大モデルへ黙って redirect されるより良い
* **`model_id` とベンダー形式別名は全て残る。** `openai/gpt-oss-120b` / `Qwen/Qwen3.6-27B` /
  `zai-org/GLM-5.2` など、人が指名する経路は失われない
* **`proto/catalog/retired.go` には何も入らない。** あの表はモデルの退役用で、同ファイルの
  コメントが「`waired/*` 別名はここに属さない」と明記している。モデルは全て生きたまま
* `catalog-tool docs` の別名表は 3 行とも「動的」になる
* **別名の重複を検出するガードを追加した**（`CheckNameUniqueness`）。`LookupByAlias` は
  ファイル名順の先勝ちなので、2 つの manifest が同じ名前を主張すると片方が無言で到達不能に
  なる。12 個を一度に動かす作業で気づいた欠落であり、`quality_tier` の一意性ガードには
  対応物があったのに名前側には無かった

## Alternatives considered

* **`waired/flagship` を glm-5.2 に付け替える** — 名前の意味は正しくなるが、H100 でピン済みの
  環境が 560 GB のモデルを引きに行って壊れる。また glm-5.2 は `waired/moe-frontier` も持つため、
  2 つの `waired/*` 名を持つ唯一の manifest になる
* **`waired/flagship` を gpt-oss-120b に据え置く** — 既存のピンは動き続けるが、「我々の最上位」
  という名前が「推薦しないモデル」に付いたまま出荷される
* **`waired/small` も退役させる** — openclaw が全ユーザーの設定に書いているため、
  再 link されるまで壊れたままの参照が残る
* **`waired/small` を「収まる中で最小」にする** — 未ダウンロードのモデルに解決しうるので、
  軽い選択肢を求めた要求が最も遅い応答になる
