---
status: accepted
---

# 塞がっていたエンジンは、失敗した計測ではない (20260905 19:46)

## Status
Accepted

## Context

`hostSpeedStage` は 1 つのフィールドで、読み手が 2 人いる。

- **ウィザードの setup 行**(`cmd/waired-agent/host_speed_steps.go` →
  CP の `setupComplete`)。ここが要るのは「この boot でもう動かない終端状態か」。
  `running` のまま残る行は `setup_complete` を永久に拒否する(waired#1143)。
- **`waired init` step 6**(`cmd/waired/init_host_speed.go` の
  `hostSpeedStageGaveUp`)。ここが要るのは「20 分の予算内にまだ数字が届くか」。

`ensureHostSpeedMeasured` は `claimEngineExclusive` に振られただけで
`hostSpeedStageMeasureFailed` を立てていた。これは 2 人目にとって嘘で、
`hostSpeedStageGaveUp` の doc 自身が「a measurement still deferring behind a
busy engine — means keep waiting」と明記している場合そのものだった。結果:

1. init が待ちを打ち切る。
2. ウィザードに赤い失敗行が出る(`SetupErrorInternal`)。
3. 数字は publish されない。

機構は TOCTOU。`awaitQuietEngine` が `engineIsQuietAndUnclaimed` で
「静穏かつ未 claim」を確認した後、プローブモデルの取得を挟んで
`claimEngineExclusive` を取りに行く。その間に、完了した pull が起こす
serve reconcile の後ろから boot benchmark(`claimEngineForBench`)や
#1127 の prefill 計測が同じ `engineExclusive` を取る。
そして `startHostSpeedMeasurement` は 1 回しか試みなかった。

観測: `routing sentinel (linux, 350M)` が `main` の直近 16 run 中 3 回、
`no host-speed measurement published (#496)` で落ち、無変更の再 run で緑になる
(waired-agent#579)。

## Decision

**1. 一時的な事情に専用の stage を与える。**
`hostSpeedStageMeasureDeferred`(ワイヤ文字列 `"measure_deferred"`)。
setup 行は probe = `done` / measure = **`skipped`**、エラーコード無し。
`skipped` は `hostSpeedStageProbeFailed` 腕が同じ理由で既に採っている選択で、
CP の done/skipped 要求を満たすので、再試行中も `setup_complete` を塞がない。
`hostSpeedStageGaveUp` は未知の値に `false` を返すので、init 側は
**コードを変えずに** doc どおり「待ち続ける」になる。

**2. 振られたら戻る。**
`startHostSpeedMeasurement` を `measureHostSpeedWhenQuiet` にし、
`remeasureWhenQuiet`(waired-agent#821)/ boot benchmark の retry loop
(waired-agent#1150)と同じ形にした。総予算は増やさない ——
`hostSpeedSettleWait` (60 分) の期限を goroutine の先頭で 1 回だけ計算して
`awaitQuietEngine` に渡す(以前は呼ぶたびに 1 時間が始まっていた)。

再試行するのは claim を取り損ねた出口だけ。プローブモデルは 1 回目で
ディスクに乗るので次の pass は安い。「計測中に推論を配ってしまった」
(`servingAdmittedCount` の変化)は計測を実際に走らせて捨てた出口なので
`measure_failed` のまま、「adopted engine がモデルを抱えている」は
boot スコープの条件なのでそのまま。

**3. 予算を使い切っても `measure_failed` に昇格させない。**
`hostSpeedStageGaveUp` の doc が、この class について
「予算を使い切って手持ちで判断する。それがこのステップ全体の fail-open 端」
と明示的に指定しているため。init は 20 分使って訊く(既定オフ)——
オーナー裁定 2026-08-08、waired#1067 / waired-agent#585。

**4. ついでに、2 本のログ行が事実を言うようにした。**
`awaitQuietEngine` は `bool` ではなく 3 値(`quietWaitReady` /
`quietWaitBusy` / `quietWaitStopping`)を返す。「1 時間待って静穏にならなかった」と
「プロセスが先に終わった」は別の知らせで、後者を前者として印字していた。
`hostSpeedFallback` は、手持ちの測定が無いときに
"keeping the previous measurement" と言わない。

## Consequences

- ワイヤ影響なし。`host_speed_stage` は loopback の mgmt API にしか存在せず、
  `proto/` にも CP にも無い。旧 CLI は未知の値を「待つ」と読むので前方互換。
- エンジンが一時的に塞がっているホストで、ウィザードの速度行が
  赤い失敗から `skipped` に変わる。赤が出るのは実際に失敗した出口だけになった。
- 最悪の所要は「60 分の settle 予算 + 最後の pass の 16 分」。
  この goroutine の後ろには何も待っていない(`hostSpeedSettleWait` の doc)。
- 再試行の間隔は provider のフィールド `hostSpeedRetry`(既定
  `hostSpeedRetryPause` = 5 s)。package var にしないのは `remeasure` と同じ理由で、
  ループが呼び出しより長生きするため。

`docs/decisions/20260812/0331-one-exclusive-engine-measurement.md` が定めた
排他 claim そのものは変えていない。変えたのは、その claim に振られたときに
何と報告し、その後どうするか。

## Refs
- https://github.com/waired-ai/waired-agent/issues/579
- waired#1143 / waired#1067 / waired-agent#585 / waired-agent#821 / waired-agent#1150
- `docs/decisions/20260812/0331-one-exclusive-engine-measurement.md`
