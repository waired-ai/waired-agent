---
status: accepted
---

# 段下げは自動選定と同じ梯子を歩く (20260820 11:30)

## Status
Accepted

## Context

`router.LighterCandidate` は #133 の「ベンチがフロア未満 → 軽いモデルを勧める」
提案を作る。実装は「baseline より軽い候補のうち**最も重いもの**」を返しており、
順序は `footprintCmp`（第一キー `EstimatedWeightGB`、品質項なし）だった。

出荷カタログ（ollama バリアント）でこれが逆転する:

| tier | 重み GB | model/variant |
|---|---|---|
| 90 | 22.6 | qwen3.6-35b-a3b/mtp-q4-gguf（`min_engine_version` 0.30.0） |
| 89 | 23.9 | qwen3.6-35b-a3b/q4-gguf |
| **83** | **81.0** | **qwen3.5-122b-a10b/q4-gguf（レビュー機の active）** |
| 73 | 24.0 | qwen3.5-35b-a3b/q4-gguf |

v0.0.3-rc2 のオーナーレビュー（waired-ai/waired#1223）で 122b が 30 tok/s、
提案されたのは tier 73 の qwen3.5-35b-a3b。0.1 GB のために 16〜17 tier 落ちる。
実機のホストプロファイルと出荷カタログで再現済み（エンジン 0.32.13 / 0.29.0 の
両方で同じ誤選択）。

**同じフローの反対側は既に tier の梯子を歩いていた。** CLI が「これ以上軽いものは
無い」を判定する `isLightestOfferedModel`（cmd/waired/init_modelselect.go）は、
自身のコメントで *"An ORDERING, not a floor … `quality_tier` survives as the
internal ranking (#518)"* と述べ `bestVariantTier` を比較している。`RankModels` の
主ソートキーも `quality_tier` 降順であり、逆方向の `UpgradeCandidate` は
「ランク順で最初に条件を満たす候補」を返す。段下げだけが重みで並んでいた。

## Decision

**1. 段下げはランク順で最初に条件を満たす候補を返す。**
「baseline より厳密に軽い」という受け入れ判定と「active と同じモデルは除く」
（waired-agent#754）はそのまま。選択だけが「最も重い」から「ランク順で最初」に
変わる。`RankModels` が tier 降順なので、これは「軽い候補のうち最も tier が高い
もの」と同義。`footprintCmp` は受け入れ判定側で引き続き必要なので変更しない。

一歩ずつであることは変わらない。受諾後の再計測が、まだフロア未満ならさらに次の
一歩に繋がる。各段で baseline のフットプリントが必ず下がるので、受け入れ集合は
厳密に縮み、鎖は必ず止まる。旧規則ではこの鎖が 81.0 → 24.0 → 23.9 GB と進み、
0.1 GB の「一歩」にダウンロードと再起動を丸ごと費やしていた。

**2. tier 差は提案文に出さない。**
waired-agent#834 の Fix direction は「大きな降格が見えるよう tier 差を提案文に
出すことを検討」と書いているが、これは
`docs/decisions/20260808/0452-model-size-class-replaces-the-quality-number.md`
の Alternatives considered に**逐語で棄却済み**の案である:

> 粗いスケールではなく相対表現。ベンチマークの切替プロンプトで「品質が上がる/
> 下がる」を出す案。消す理由が「測定値ではない」である以上、比較として出すのは
> 同じ主張を弱く言い直すだけになる。棄却

裁定は `docs-site/TRANSLATION.md`（`~~quality score (quality_tier)~~` …
**ユーザー向け文面から撤去（#537）**）に記録済みで、`init_benchmark.go` には
**まさにこのプロンプトから** "very low quality" を撤去した経緯コメントが残って
いる。オーナー確認（2026-08-20）: `small`/`medium`/`large` はそもそも重みの
大きさなので、より軽い退避先が下のクラスに落ちるのは自明であり注記に値しない。
そして決定 1 が大きな降格そのものを消すので、この項の動機は解消する。

`quality_tier` は内部に残る（決定 0452 の「変わるのは**描画するかどうか**だけ」）。
`LighterCandidate` の `Reasons` 文字列は tier を含んだままにする — 唯一の製品
呼び出し元 `recommendationFromBench` はモデル ID しか読まず、この文字列が届くのは
管理 API の JSON とログだけで、描画される面ではない。

**3. 最軽量モデル分岐の文言は、廃止された床を主張しない。**
`tinyBenchmarkDisableFlow` のゲートは `isLightestOfferedModel`（順序判定）だが、
読まれる文は "sits below the bar Waired uses for coding — not recommended on any
computer" と**床**を主張していた。#522（オーナー決定 2026-08-08）は
`InstallQualityFloorTier` を選定の床から外している。テストフィクスチャのコメント
は既に訂正済み（"It used to say 'below the install quality floor', which #522
removed — the branch is chosen by an ordering now, not a threshold"）で、製品
文字列と関数 doc だけが取り残されていた。オーナー承認済みの文面:

```
   interactive floor. %s is the smallest model Waired offers, so there is
   nothing lighter to switch to after it.
```

## Consequences

- **提案先が変わるので、既存の却下記録は一度だけ効かなくなる。**
  却下は `DismissalKey(active variant SHA, target variant ID)` で記録される。
  target が変われば鍵が変わるため、旧提案を却下したホストは新しい提案を一度
  聞かれる。これは正しい挙動 — 却下されたのは別の提案である。
- **提案先が active より高い tier になり得る。** レビュー機では tier 83 →
  tier 90。製品文は "the lighter model should run more smoothly on this
  hardware" で品質を主張していないので、文言は既に新しい挙動と整合している。
- **フィクスチャの共単調性がこの欠陥を隠していた。** `fixtureCatalog` と
  `siblingVariantCatalog` はどちらも「重いほど tier が高い」ので、2 つの規則が
  常に同じ答えを返す。逆転を持つ `invertedLadderCatalog` と、出荷カタログ
  ×レビュー機ホスト×エンジン 2 版のテーブルを追加した。
- **最小の重み減少量は導入しない。** 旧規則にもそのような規則は無く、
  「軽い側の軸を予測 tok/s にすべきか」は #761（needs-design）と #466 の範囲。
- `proto/hostfit.InstallQualityFloorTier = 30` は本リポで参照ゼロだが、公開
  proto の const であり `proto-guard` が削除を禁じるため触らない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/834
- https://github.com/waired-ai/waired-agent/issues/761 / #466（軽い側の軸を
  予測 tok/s にする案、needs-design）
- https://github.com/waired-ai/waired-agent/issues/522 / #537 / #518
- docs/decisions/20260808/0452-model-size-class-replaces-the-quality-number.md
- docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md
- docs-site/TRANSLATION.md
