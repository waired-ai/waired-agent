---
status: accepted
---

# host-fit の判定は proto/hostfit に 1 つだけ置く (20260727 12:40)

## Status
Accepted

## Context
「このマシンでこの variant を serve できるか」の実装が 2 つあり、食い違っていた。
agent は `hardware.Profile` から(`router.hostFits` / `ollamaFitsVRAM`)、control plane は
broadcast された `signer.HardwareSummary` から別実装で判定していた。CP 側には
**VRAM の項が無く**、128GB RAM + 24GB カードのホストに 62GB モデルを `runnable` と答え、
カタログ順の先頭 runnable = wizard の既定にまでしていた(waired-ai/waired#942)。
同じホストで agent 自身の picker はそれを拒否する。

CP 側のヘルパーには "reproduces hardware.Profile.EffectiveVRAMMB()" という doc comment が
付いていた。2 リポジトリにまたがる約束をコメントで守ることはできない、という実例。

waired-ai/waired#946 の P4(fixture の出所断絶)と同型で、対象が fixture ではなく判定本体。
#180 / PR #206 で「fixture は producer から導出する」規律は入ったが、判定が二重実装である
限り両者は独立に drift する。

## Decision
- 判定を **`proto/hostfit`**(両リポが既に import しているモジュール)に置く。
  `Host` は判定が読む 5 つの事実だけを持ち、producer のどちらの型でもない。
  各サイドが adapter を 1 つだけ持つ — CP は `hostfit.FromHardwareSummary`、
  agent は `hardware.Profile.HostFit()`。
- **共有するもの**: 実効 VRAM、ollama の RAM ゲートと GPU 常駐チェック、overhead モデル、
  `MinVLLMVRAMMB` / `InstallQualityFloorTier`(どちらも CP に手写しがあった)。
- **共有しないもの**: engine version フロア、vendor support matrix、tensor-parallel 集約、
  #624 context floor、disk pre-flight。CP は入力を持たず、いずれも serve 時ポリシー。
  そのため `VLLMFit` は budget を引数で受ける(agent は TP 集約、CP は実効 VRAM)。
- **挙動は現行ルールの逐語移植**。CPU-only は常駐チェックを飛ばし、小さい GPU を持つ
  ホストの方が厳しく判定される非対称はそのまま残した。実際には後者の方が速く動くので
  これは本物の policy question だが、実装統一と規則変更を同じ PR に混ぜるとどちらも
  レビューできなくなる。#229 に分離。

## Consequences
- 「片方だけ直る」が起きなくなる。#229 の判断が付いたときも直す場所は 1 箇所。
- producer↔consumer の契約テスト(`TestHardwareSummaryFor_SurvivesTheRoundTrip`)が
  publish→decode の往復で fit が読む事実を落としていないことを固定する。
  producer から `usable_vram_mb` を落とすと「CP は 16384、agent は 12288 を見る」で赤くなる。
- CP は proto タグを bump して自前の `variantFit` / `effectiveVRAMMB` を捨てる。
- 新しい fit 規則を足すときは proto の additive-only 規律に乗る(= tag を切る)。
  判定の変更が公開契約の変更として扱われるのは、この分野では望ましい摩擦。

## Refs
- #228 / PR #232(proto/hostfit)/ 本 PR(router 側の採用)
- #229 — GPU ホストの方が CPU-only より厳しい件
- waired-ai/waired#942 / waired-ai/waired#946 / #180 / PR #206
