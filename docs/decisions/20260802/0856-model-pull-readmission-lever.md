---
status: accepted
---

# 失敗した pull の再許可は世代レバーだけで行う（時間駆動の自動再試行は作らない） (20260802 08:56)

## Status
Accepted

## Context

pull admission は desired model の**値**ごとに 1 回（`modelApplied`）。唯一の
再許可はエンジンの absent→present 遷移だが、`engineObserved` は最初のフレームで
false なので、**daemon 起動時に既にエンジンがあるホストでは構造的に発火しない**。
初回セットアップ以降のすべてのホストがこれに当たる。結果、一度失敗した DL は
daemon 再起動まで赤いまま残った（#136、rc7 実機で確認）。

一過性の失敗そのものは #306（PR #378）が `runPullJob` の内側で有限リトライを
入れて解決済み。ただし **disk_full は意図的に再試行しない** — 自分では解消しない
条件に多 GB を 3 回使っても、ウィザードが見せるべき正直なエラーが遅れるだけだから。

残るのは「10 分後に容量を空けた人間の意図」で、これをどう受けるかが本件。

## Decision

**CP が bump する世代カウンタ `desired_model_gen` だけを再許可の入口にする。**
agent 側に時間駆動の自動再試行は作らない。

実装上の 3 点:

1. **クリアは engine ブロックの外側**に置く。`appeared` の判定は
   `d.engine != ""` の中にあり、エンジン指示の無いホスト（セットアップ済みで
   モデルだけ変えるケース）では丸ごとスキップされる。リトライはそこで死んではならない。
2. **判定は「値」ではなく「前進」**。CP は map frame ごとに同じ指示を再送するので、
   値での判定は多 GB の DL を毎フレーム再キューする。`modelGenActed` を同じ
   クリティカルセクションで進めることで冪等になる。
3. **`modelGenActed` は永続化しない。** インメモリなのは `modelApplied` 自身と
   同じ理由で、さらに積極的な理由がある — 再起動は**どのみち再許可する**（それが
   この issue が置き換えようとしている唯一の復旧手段）。永続化しても得るものは無く、
   クラッシュ前に押されたリトライを握り潰す危険だけが増える。

## Consequences

- 無人のマシンは自力で回復しない。これは受け入れる: 従量制回線で数十 GB を
  誰にも頼まれずに落とし直すリスクのほうが大きく、無人ホストには
  `waired models pull` がある。
- `SetupProgress.ModelGen` のエコーが必須になる。押した直後はステップがまだ
  `failed` のままなので、「まだ拾われていない」と「拾って再び失敗した」を
  エコー無しでは区別できず、リトライボタンは回りっぱなしか死んだままになる。
- `desired_model_gen` を bump しない CP（=現行）に対しては挙動がビット単位で不変。
  エコーも `omitempty` の 0 で wire に出ない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/136
- https://github.com/waired-ai/waired-agent/pull/411 （onboarding-v3 wire）
- https://github.com/waired-ai/waired-agent/issues/306 （有限リトライ、disk_full 除外）
- waired#986 (rc7 レビュー, F06), waired#1002 (レーン L25)
