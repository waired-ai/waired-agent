---
status: accepted
---

# 推奨モデルは「重みが VRAM に載る」で決める (20260801 13:18)

## Status
Accepted

## Context

「このホストは既定でどのモデルを指すべきか」の実装が 2 つあり、食い違っていた。
`proto/hostfit` が作られた原因(waired-ai/waired#942)と同じ形の再発である。

- エージェントの `router.RankModels` は #624 のコーディング・コンテキストゲートを
  適用していた(ネイティブ窓 + ホスト側の bounded-spill 判定)。
- CP の `recommendedModel` はそのどちらも適用していなかった。理由はコード自身の
  doc コメントにある — #624 のフロアは「serve 時のチューニング入力が要る」。
  これはホスト側半分については真だが、**ネイティブ半分については偽**で、
  そちらは manifest の比較だけで済み、CP は同じ manifest を持っている。
- 結果、CP は roofline の decode 見積りだけで判断していた。roofline は MoE に
  構造的に盲目である: MoE はトークンあたり **active** な重みしか読まないため、
  35B/3.3B-active のモデルは重みの 2/3 がシステム RAM にあっても ~81 tok/s と
  予測される。フロアを通過し、最高 quality tier を持ち、16GB カードの既定になった。

rc7 レビュー(waired-ai/waired#986)の実測: 22.6GB のモデルが 16GB カードで
重みの 37.7% をシステム RAM に落とし、~30k トークンのコーディングプロンプトの
prefill が 388 tok/s。最初の 1 トークンまで 60〜90 秒。**そのマシン自身の
エージェントなら決して選ばないモデル**を、ウィザードが既定にしていた。

## Decision

roofline に spill cap を足す(waired-ai/waired-agent#320 item 1 の当初案)のではなく、
オーナー提案(waired-ai/waired#988)の簡素化ルールを採り、**両側が呼ぶ述語 1 つ**に畳む。
「オペレータが暗算できるルール」であることが選定理由。

容量ゲート(`OllamaFit` / `Fits`)は**一切変更しない** — #229 の単調性も CP/agent の
一致もそのまま。推奨は独立したポリシー層 `hostfit.OllamaRecommend` とする。

| クラス | 容量(不変) | 推奨(新設) |
|---|---|---|
| CPU-only | RAM ≥ min_ram_gb | 制約なし(`BandwidthSystemRAMGBs` はマージンを持たない上界なので注記のみ) |
| discrete | RAM のみ | **weights + margin ≤ `OllamaVRAMBudgetMB`**。KV の spill は許容 |
| unified | weights + KV + margin ≤ pool | 同左 + #251 の published-peak 除外を維持 |

- **margin は既存の `OllamaVRAMOverheadMB`**(discrete 1024 MiB + 40 MiB/GB、UMA 1024 MiB)。
  新定数は入れない。これは倹約ではなく設計上必須で、#621 の serve 時 clamp が引くのと
  同一の数字であるため、「ウィザードは薦めたのにチューナが警告する」ズレが生じ得ない。
- discrete で KV ではなく weights を見るのは、外れ方が違うから。載らない KV は窓が
  clamp されるだけ(コンテキストのトークンを失う)だが、載らない weights は
  **あらゆるプロンプトのあらゆるトークン**で RAM から読み直される。
- **ネイティブ context floor (200k) も `proto/hostfit` へ移し、CP と共有**する。
  ホスト側半分はエージェント専用のまま。

### 実装で判明し、当初案から変えた点

1. **roofline は annotate-only に降格しない。** residency は「どこに載るか」で
   モデルを分けるが、**何も載らないホストでは何も分けられず**素通りする。それが
   まさに #229 が書かれた状況で、81GB モデル 2 つを保持できない 24GB カード上では、
   3.3B-active の MoE が実用的に decode でき 122B-active の dense はどのカードでも
   できない、と知っているのは roofline だけ。よってパスは重ねる —
   residency → roofline、各段とも「何も残らないなら素通り」。
2. **推奨ゲートはハードウェアについて単調でない。** weights がカードに載ることを
   要求するため、システム RAM より小さいカードでは最良の resident モデルが
   `InstallQualityFloorTier` を下回りうる。8GB ラップトップ + 4GB カードは
   tier 27 止まりで under-spec 判定になり、**同じラップトップからカードを抜くと
   qwen3.5-4b が入って動く** — #229 の失敗モードが 1 階層上で再発する。
   tier が下がるのは waired-ai/waired#988 item 5 が受容した取引だが、
   ローカル推論を失うのは受容範囲外。よって `PickInput.NoRecommendGate` を追加し、
   `SelectInstallModel` は under-spec と結論する前にこのゲートを降ろす。
   既存の `NoContextFloor` 救済より**先**に降ろす: residency を諦めると速度を失い、
   context floor を諦めると ~200k のコーディング窓そのものを失うので、安い方が先。

## Consequences

- 16GB カード上の spilled MoE(qwen3.6-35b-a3b 級)が推奨から消える。
  24GB カードでは 22.6GB → 23482 MiB ≤ 24564 MiB で旗艦は残る。
- 非 MTP タグ(23.9GB → 24773 MiB)は 24GB カードから落ちる。これは #624 の
  bounded-spill ゲートが逆方向から出していた結論(「アンカーの 11.5% expected は
  通り、非 MTP の ~25% は通らない」)と一致する — 簡素化ルールが、置き換える対象の
  較正を、その較正が測られたホスト上で再現している。
- discrete では推奨集合は縮む方向にしか動かない(weights が resident なら roofline は
  元々 no-claim で通していた)ので、「推奨されたのに壊れている」ホストは新たに生じない。
- 推奨されないモデルは**提示され続ける**(gray / 注記)。隠すのは #229 が除いたバグ。
  `ReasonWeightsSpill` / `ReasonTooSlow` の文言は presentation 契約
  (waired-ai/waired-agent#321)で決める。
- カード付きホストがカード無しより低い tier を選ぶケースは残る(例: 32GB RAM +
  8GB カードで tier 42 対 tier 89)。resident な小型モデルの方が実機として速いので
  意図どおりだが、単調でないことは `TestSelectInstallModel_ASmallCardMustNotMakeAHostUnderSpec`
  が上限として固定している。
- `UpgradeCandidate` の spill cap は二重化した。residency ゲートが先に候補を落とすので、
  cap はゲートを降ろした経路のための第 2 層になる。

## Refs
- waired-ai/waired#988(決定 + wire-contract テーブル)、waired-ai/waired#986(rc7 レビュー)
- https://github.com/waired-ai/waired-agent/issues/320
- https://github.com/waired-ai/waired-agent/pull/348
- `proto/hostfit/hostfit.go`、`internal/router/model_picker.go`、`internal/router/install_picker.go`
- 関連: #229, #251, #264, #517, #621, #624
