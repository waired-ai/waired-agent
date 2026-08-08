---
status: accepted
---

# インストール時の下限は容量であって品質ではない (20260808 21:45)

## Status
Accepted

## Context

`InstallQualityFloorTier = 30` は #517 で入った。インストール時に自動選定した
モデルの `quality_tier` がこれ未満なら、そのホストは「推奨構成未満」として
**ローカル推論を切って**いた。定義文は「30 == qwen2.5-coder-3b-instruct、我々が
出荷する最小の実用コーディングモデル」と書いていた。

3 つの前提が、この床を支えられなくなった。

**1. アンカーが消える。** #522 が 2025 世代を退役させると `qwen2.5-coder-3b-instruct`
がカタログから無くなる。30 はどのモデルも指さない数字になる。

**2. 1 世代の中では tier はサイズの単調関数。** #518 が `quality_tier` を
「載せると決めた世代のパラメータ順を出典付き override で補正したもの」
（`10·log10(params) − 5·log10(footprint_mb)`）と再定義した。カタログは
qwen3.5/3.6 の 1 世代に固定されているので、その集合に対する「tier ≥ 30」は
**サイズの下限を遠回しに書いたもの**にすぎない。

**3. その線を引ける測定が存在しない。** 唯一の実測である agent-grade harness
（`internal/catalog/agentgrade.json`、worst-case failures / 72 trials）は、
現行世代の中で**サイズに対して単調ではない**:

```
qwen3.5-0.8b        3/72
qwen3.5-2b          4/72
qwen3.5-4b          3/72
qwen3.5-9b          2/72
qwen3.5-27b         0/72 pass
qwen3.5-35b-a3b     6/72   ← 0.8b より悪い
qwen3.6-27b         1/72
qwen3.6-35b-a3b     0/72 pass
```

35b-a3b (6/72) が 0.8b (3/72) より悪い。agentgrade は「ツールを呼べるか」の
床テストであってランキングではない。**「2b は弱すぎるが 4b は大丈夫」と言える
測定は無い。** 退役する側（qwen2.5-coder-3b は 12/72、0.5b は 48/72）との差は
歴然で、そこは退役が引き受ける。

さらに `waired#1056`（2026-08-03 批准、`waired`
`docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`）が
「拒否は確実な OOM に限り、それ以外は警告して選択に従う」と定めている。
品質床は soft な基準でローカル推論を落とす唯一のゲートとして残っていた。

`docs/decisions/20260808/1907-price-capacity-at-the-served-window.md` は
「`InstallQualityFloorTier`（30）の扱い。品質スコアの数値自体を廃止する方向で
検討中のため、今回は触らない（オーナー判断 2026-08-08）」と保留を明記していた。
本決定がその保留を解く。

## Decision

**品質床を廃止する**（オーナー判断 2026-08-08）。

`router.SelectInstallModel` から tier フィルタと `minTier` 引数を外す。残る
ゲートは 2 つだけで、どちらも既に `RankModels` が適用している:

- **容量** — `hostfit.OllamaCapacityFit`。確実な OOM。#552 以降は
  「製品が実際に提供するウィンドウ」で価格付けする
- **#624 ネイティブウィンドウ** — manifest 比較。`waired#1031` により
  stand-down 禁止（32k のモデルはどんなハードでもコーディングセッションを
  保持できず、ワイヤに「32k を提供する」と言う手段が無い）

`quality_tier` は**内部の序列としてそのまま残る** — ランキングのソートキー、
`tier_override` の唯一の記録、公開 proto フィールド。閾値としての役割だけが
無くなる。

自動インストールの**最下段は qwen3.5-2b**（オーナー判断）。追加の細工は要らない:
tier 降順ソートが両方収まるホストでは 2b (27) を 0.8b (12) より優先し、
0.8b しか収まらないホストは #552 の容量床が既に落としている。

## Consequences

**8 GB クラスのホストがローカル推論を得る。** これが実質的な変化。
`internal/router/install_picker_test.go` の `cpu-8gb` ケースが
`wantOK: false → true` に反転し、`internal/setup/modelselect_test.go` の
「8 GB は推奨構成未満」ケースも反転する。どちらも PR 本文で明示した。

**`BelowFloorModelID` が消える。** 「床未満だが動くモデルを opt-in として
提示する」経路は、床が無ければ通常の選定と同じクエリになる。
`BelowRecommendedSpec` は残るが、意味が「床を越えるモデルが無い」から
**「何も収まらない」**に狭まる。

**CPU 7 GB ちょうどの 1 バンドだけ 0.8b が選ばれる。** OS 取り分を引いた
5120 MiB は 0.8b のウィンドウを保持できるが 2b のは保持できないので、
そこでコーディングセッションに答えられる唯一のモデルが 0.8b になる。
6 GB ではどちらも保持できず推薦パスが空振りし、tier 順で 2b が best-effort
として返る。これは sag ではなく `waired#1056` 決定 3 の同じトレード
（ウィンドウを保持する小さいモデルが、切り詰める大きいモデルに勝つ）で、
`router.TestInstallPickIsMonotoneOnceRecommended` が VRAM 軸で既に記録している
`8 GB RAM + 2 GB カード → tier 52 が切り詰め / + 8 GB カード → tier 42 が保持`
と同じ形。RAM 軸の単調性は `router.TestInstallPickIsMonotoneInRAM` で新たに
固定した。

**`belowRecommendedSpecNeed` の基準が変わる。** #552 が入れた「そのモデルが
自分の rung でウィンドウを保持するのに要るメモリ」という算術はそのまま残し、
tier の述語だけを落とした（`smallestAboveFloorRAMGB` → `smallestServableRAMGB`、
`smallestAboveFloorReq` → `smallestVariantReq`）。`min_ram_gb` には戻さない —
それは手書きの意見であり、8 GB のホストに「4 GB 以上が必要」と言っていた。

**`proto/hostfit.InstallQualityFloorTier` の const は残る。**
`scripts/ci/protoguard` が `const removed` / `const value changed` の両方を
落とし、免除機構が無い。#522 の退役 PR が `Deprecated:` を付ける。
`proto/hostfit/serving_window_test.go` はまだこの const で変種を絞っており、
同じ PR で扱う。

**`PickInput.NoRecommendGate` に production の書き手が居なくなる。**
`SelectInstallModel` の stand-down 再帰が唯一の書き手だった。`RankModels` の
`narrow()` が空振り時に fall-through するので、tier フィルタが独立に集合を
空にすることが無くなった今、再帰は到達不能になった。フィールド自体は
`RankModels` が honour する正当なノブとして残す。

## Refs
- waired-ai/waired-agent#522（世代退役と床の再アンカー。本決定がその「再アンカー」を置き換える）
- waired-ai/waired-agent#518（`quality_tier` は生成世代の梯子であってベンチの合成値ではない）
- waired-ai/waired#1056 / `waired` `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`
- waired-ai/waired#1031（ネイティブウィンドウ半分は stand-down 禁止）
- `docs/decisions/20260808/1907-price-capacity-at-the-served-window.md`（保留項目を残した決定）
- `docs/decisions/20260808/0452-model-size-class-replaces-the-quality-number.md`（#537、数値をユーザー面から外した決定）
