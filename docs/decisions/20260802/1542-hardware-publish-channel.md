---
status: accepted
---

# ホストプロファイルの公開チャネルは inference-status のまま広げる (20260802 15:42)

## Status
Accepted

## Context

`HardwareSummary` は InferenceState probe push のライダーとしてしか送られて
おらず、probe ループは probe 対象のエンジンが無いと**何も push せずに
return** する (`cmd/waired-agent/inference_probe.go`)。その結果、エンジンを
まだ決めていない / インストール中 / 起動に失敗したホストは、自分が何者かを
control plane に伝える経路を一本も持たなかった。ブラウザのセットアップ
ウィザードが動くのは正にその窓で、control plane は保存済みの
`InferenceState.Hardware` に対してモデルカタログを採点するため、全モデルが
`runnable` / `unknown_hardware`、推奨なし、順序なしで提示されていた
(waired-ai/waired#987 はこれをブラウザ側で*無害*にしたが、窓は短くしていない)。

issue #387 は候補を 3 つ挙げ、setup-progress への相乗りを第1候補としていた。

## Decision

**既存の inference-status push を、エンジン不在ホストにも広げる**
(#387 の第3案)。エンジンが無い間は `type=none` / `reachable=false` /
endpoint・models なし、hardware だけを載せた InferenceState を
60 秒ごとに push する (`runHardwareOnlyReport`)。

第1案 (setup-progress 相乗り) を採らなかった理由:

- `setupReconciler.snapshot()` は desired state も executor lease も無い間
  `nil` を返す。ウィザードが `setup-desired` を POST するのは操作者が
  エンジンとモデルを**選んだ後**なので、blind な窓のあいだこのチャネルは
  無音であり、狙った問題を解けない。
- 解こうとすると snapshot() の nil 条件を緩める必要があり、onboarding を
  一切していないホストまで恒常的に push し始める。
- 加えて proto の additive フィールド追加 → タグ → CP 側の保存/参照、と
  2 リポジトリ 3 PR を要する。

第2案 (enrollment 時) は、既に enroll 済みのホストと、ハードウェア構成が
後から変わったホストを取りこぼす。

採用案は proto にも control plane にも変更が要らない。CP はカタログ採点で
`Device.InferenceState.Hardware` を直接読んでおり、`type=none` は
`IsValidInferenceType` が既に受理する。

あわせて、boot 時に 1 回焼き付けていたサマリを getter 越しの参照に変え、
Profiler の TTL (5 分) で再検出されるようにした。GPU やドライバを後から
入れたホストが daemon 再起動まで古い答えを報告し続ける問題への対応。

## Consequences

- エンジン不在ホストの device 行に `inference_state` が入る。管理 UI の
  表示は変わらない — `deriveInferenceState` は `type === "none"` を
  reachability 判定より手前で中立バッジに落とすため。
- ピアの network map にも `reachable=false` のエントリとして載るが、
  aggregator の到達性集計は `Reachable && !stale` を要求するので寄与は
  ゼロ。CP の `inferenceStateContentEqual` は Hardware を比較しないので、
  内容不変の再 push は map 再送を発火しない。
- `--disable-inference` と共有 OFF (`IsShared() == false`) は今日どおり
  完全に無音のまま。とくに後者は「共有スイッチが公開する範囲」を広げない
  ための意図的な選択で、共有 OFF ホストはウィザードから見て未知のまま
  残る (この fix の前と同じ)。切り離すかどうかは別の判断とする。
- ハード変更がピアに伝わるのは次回 map 取得時になる (上記のとおり
  Hardware は content 比較の対象外)。カタログを採点する CP 側は保存値を
  直接読むので即座に反映される。

## Refs
- https://github.com/waired-ai/waired-agent/issues/387
- https://github.com/waired-ai/waired/issues/987
- https://github.com/waired-ai/waired/issues/986
