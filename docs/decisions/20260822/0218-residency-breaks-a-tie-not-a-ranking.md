---
status: accepted
---

# 常駐はタイブレークであってランキングではない (20260822 02:18)

## Status
Accepted

## Context

waired-agent#880: アイドル失効したピアは温かいピアより 17〜56 秒遅く最初の
トークンを返す（#861 実測）のに、ピア選択にはその差を表現できる項が無い。

`probe_client.go` には `ModelResident` が既に宣言されていて「Nothing scores on
it yet; see #880」とあるので**1行の配線に見える**が、実際は交わらない2つの層に
またがっていた:

- **常駐は probe 時にしか来ない**。`/healthz` の応答なので `ParallelProbe` の中。
- **順位は router 時にしか無い**。`sortMeshCandidates` の
  public / silent / priority / score / errorRate / rttBucket / loadFraction /
  deviceID は、すべてスナップショット由来で Selector だけが持つ。

#949 でワイヤに載った `ResidencyIdleTimeout` はこれを埋めない。doc に明記の
とおり **push-only で `effectiveInferenceState` が served NetworkMap から消す**
ので、ピアには届かない。しかも載っているのは「アイドル保持時間の**設定**」で
あって「いま常駐しているか」ではない、別の事実である。

## Decision

**probe 層に渡すのは「順位段」1ビットだけにする。**

`meshCandidate.rankTier` / `Candidate.RankTier` — ソート直後に
`assignRankTiers` が振る、ソート済みスライス上の連番。**同一 tier =
「上のキーが全部同値で、恣意的な deviceID の接尾辞しか差が無い」**。

`bestSettledReady` は、先頭の ready 候補が**既知で cold** のときに限り、
**同一 tier 内**を探して既知で warm な候補に差し替える。それ以外は今日と同じ。

- **nil は今日の位置のまま**。「見ていない」は「cold」ではない
  （`docs/decisions/20260820/0130-model-residency-is-a-setting.md`）。昇格も
  降格もさせない。
- **tier をまたがない**。quality / priority / errorRate / 距離 / 負荷 の
  どれかで勝っているピアは、cold のままでも勝ち続ける。
- **tier が揃うまで決めない**。同順位のピアがまだ probe 中なら
  `decided=false` を返す。この関数の唯一の契約は「答えがもう変わらなくなって
  から決める」ことで、cold な先頭に確定してしまうとそれを破る。
  逆に **warm な先頭・沈黙している先頭は即決**する — 隣が何を言っても改善
  しないので、待つのは無意味なレイテンシ。

置き換えている相手は `deviceID` 昇順、つまり**リクエストから見れば恣意的な
値**である。よくあるメッシュ（同じモデルを動かす idle な LAN 機 2 台）は、
実際に全キーで同値になって deviceID まで落ちてくるので、この tie-break は
空振りしない。

## 却下した案

**probe 層で「warm な ready をすべての cold な ready より優先」** — 順位段が
無い probe 層で書ける最も素直な形だが、quality / priority / rtt / load を
**全部上書き**する。しかも warm なピアを選び続けると そのピアだけが warm に
保たれ、他が冷えていく ratchet になる。#880 自身が「スコアリング式は主張
しない」と明記しているのに、事実上いちばん強いキーを1つ足すことになる。

**ピアの常駐を NetworkMap に載せる** — 順位段を作らずに `sortMeshCandidates`
から直接読めるようになるが、proto 変更 + capability ゲート + CP 追随が要る
（`peer-entry-fields-need-capability-gate`）。得られるものは tie-break 1つで、
釣り合わない。

## Consequences

- `RankTier` は**プロセス内限定**。ワイヤ項目ではなく、スコアでもなく、
  1回の SelectK のスライス上でしか意味を持たない。`==` 以外で比較しない。
- 手組みの `Candidate` は tier 0 一色になる = 全員同一 tier = **tie-break が
  効く**。テストのフェイクにとって寛容な既定であり、偶然のランキングを
  作らない。
- `sortMeshCandidates` にキーを足して `sameRankExceptDeviceID` に足し忘れると、
  Selector が**別順位と判定した候補が同一 tier に入り**、常駐がそのキーを
  上書きし始める。コンパイラは気づかないので、両方の関数が読むフィールド名を
  突き合わせるテストを置いた（`TestSameRankExceptDeviceID_ListsEverySortKey`）。
- 前提として waired-agent#965 を同 PR で直した。vLLM ホストは `ModelResident`
  を "not observed" で返していたので、この tie-break から恒久的に外れる —
  しかも `--gpu-memory-utilization` でプロセス終了までプールを握るので
  **構造上いちばん温かいホスト**である。

## Refs

- waired-ai/waired-agent#880, waired-ai/waired-agent#965
- waired-ai/waired-agent#861（17〜56 秒の実測）
- `docs/decisions/20260820/0130-model-residency-is-a-setting.md`
