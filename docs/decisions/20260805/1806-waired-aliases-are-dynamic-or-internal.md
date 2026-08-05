---
status: accepted
---

# `waired/*` 別名は `waired/default` 1 つに畳む (20260805 18:06)

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
| `waired/small` | qwen2.5-coder-3b-instruct | openclaw allowlist、docs-site |
| `waired/coding` | （動的、#632） | openclaw allowlist、docs-site |
| `waired/default` | （動的、#632） | `cmd/waired/infer.go:38` の既定値、`models_checkagent.go:92`、`internal/agentgrade/fixture.go:91`、`docs-site/src/data/model-catalog.ts:37`、`cmd/waired/link_helper.go:70`、openclaw allowlist、docs-site |
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

**`waired/*` は `waired/default` 1 つに畳む。** 静的な別名が残るのは `internal_only` な
モデルの場合だけ。

* 11 個の装飾別名を `model_aliases` から削除する
* **`waired/coding` と `waired/small` も退役させる**（`legacyModelRefs()` 経由で
  openclaw のユーザー設定から削除。`waired/auto` を退役させたのと同じ経路）
* `waired/default` は動的解決のまま残す
* `waired/tiny` は静的のまま残す

この名前空間は、**モデル名すら見せずに済ませることを目指した初期の構想の名残**である
（オーナー判断、20260805）。想定利用者が LLM のモデル名を当たり前に知っている層に
変わった今、抽象化そのものに需要がない。

ただし `waired/default` が残る理由は「やさしさ」ではない。**モデルを切り替えても
クライアント設定を書き換えずに済む間接参照**であり、`waired infer` の既定値・
agentgrade ハーネス・docs-site のカタログ表の `(default)` 印・openclaw プラグインが
実際に依存している。symlink が残るのと同じ理由で残る。

対して `waired/coding` は `waired/default` と**完全に同一の解決**をしていた
（`DynamicCodingAliases` に両方が入り、分岐は 1 本、どちらも `DefaultModelID` へ）。
生成ドキュメントも「動的: waired/default と同じ解決」とそのまま出力していた。
つまりピッカーが同じモデルを 2 つの名前で並べていただけで、間接参照としての
価値を何も足していない。

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

### `waired/small` を役割名に作り替えず退役させた理由

検討の途中では `waired/small` を「このホストが持っている中で最も小さいモデル」という
役割名に作り替える案を実装していた。退役に切り替えた理由は 2 つ。

1. **需要の根拠がない。** 消費者は openclaw の allowlist だけで、製品コードから
   読む場所は 1 つも無い。「軽い方が欲しい」を名前で表現する必要があるのは、
   モデル名を知らない利用者を想定していた頃の前提である
2. **ほとんどのホストで `waired/default` と同じ答えになる。** 差が出るのは
   モデルを 2 つ以上ダウンロードしているホストだけで、ピッカーに 2 行目を
   増やす価値がその差に見合わない

軽いモデルが欲しい場合は `waired models ls` に出る model_id を直接指名する。
その経路は全モデルについて残っている。

### 退役の経路

`legacyModelRefs()`（`internal/integration/openclaw/openclawjson.go`）に移す。
`mergeConfig` が re-link 時に、`removeManagedKeys` が uninstall 時にユーザーの
`openclaw.json` から該当キーを削除する。**`waired/auto` を `waired/default` へ
改称した #422/#478 と同じ経路**であり、そのときと同様、ルーターは退役した名前を
解決しない（生かしておくと誰も設定から消さないため）。

### `waired/tiny` が残る理由

granite4-350m は `internal_only` で、`BundledManifests()` は返さない。この別名は e2e ハーネスと
installtest が名前で pin する**テスト用ハンドル**であって、カタログの約束ではない。
これが残る規則の形になっている: **静的な `waired/*` 別名は、我々が提供しないモデルにのみ許される。**

## Consequences

* **退役した 13 個は解決しなくなる。** 装飾 11 個の消費者はゼロ。`waired/coding` と
  `waired/small` は openclaw のユーザー設定に残るが、re-link で削除される。
  `waired/flagship` は private の quickstart に記載があるため、そちらを
  `gpt-oss-120b` 直指名に直す follow-up が要る。明示的に `ErrModelNotFound` で
  失敗する方が、巨大モデルへ黙って redirect されるより良い
* **openclaw のピッカーは 1 行になる。** `modelRefs()` が返すのは `waired/default` のみ
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
* **`waired/small` を「このホストが持つ中で最小」という役割名に作り替える** — 実装まで
  したうえで退役に切り替えた。上記のとおり需要の根拠が無く、ほとんどのホストで
  `waired/default` と同じ答えになる
* **`waired/small` を「収まる中で最小」にする** — 未ダウンロードのモデルに解決しうるので、
  軽い選択肢を求めた要求が最も遅い応答になる
* **`waired/coding` を残す** — `waired/default` と同一解決なので、ピッカーに同じモデルが
  2 行並ぶ状態が続くだけ
* **`waired/default` も退役させる** — 間接参照としての機能は実在し、`waired infer` の
  既定値と docs-site のクライアント案内がこれに依存している。消すと
  「モデルを切り替えたらクライアント設定も書き換える」が常態になる
