---
status: accepted
---

# ブート経路が「もう試すのをやめた」を自分で記録する (20260828 21:30)

## Status
Accepted

## Context

waired-agent#1093。ウィザードのエンジン行（`setupEngineHealth`）は
**give-up ラッチだけ**を読む。そのラッチは 4 打席で立つのに、ブート 1 回が
使うのは 3 打席しかない（`engineEnsureAttempts = 3` / vLLM の
`maxAttempts = 3` に対し `engineRecoveryMaxAttempts = 3` で比較は `n > 3`）。
`crashStrikes` は provider のメモリ上なのでデーモン再起動でゼロに戻る。

つまり **「再起動しただけのホスト」ではラッチが原理的に立たない**。ところが
この状態でウィザードのエンジン行は無言になるのではなく、
`case installed:` に落ちて **`SetupStatusDone`（緑）** になる。モデルが既に
ディスクに在れば pull 行も緑なので、**エンジンが死んだホストでウィザードが
「完了」と言う**。#330 が塞いだのと同じ穴が、別の入口から開いていた。

理由は同じ tick にちゃんと在る。両エンジンの bootstrap は諦める直前に
`SetStartFailureReason(hint)` を呼んでいて、その行のコメント自身が
「これらの試行は recovery budget を超えずに全部失敗しうるので、ラッチは
立たず give-up 文も組まれない」と書いている。書き手は穴を知っていて、
読み手がそこを見ていなかった。

## Decision

**ラッチには触らない。「ブート経路が持ち球を使い切った」を別の耐久的な事実
として記録し、`setupEngineHealth` が 3 つの give-up を順に読む。**

| 事実 | 意味 | stopped | needsRepair |
|---|---|---|---|
| give-up ラッチ | `EnsureRunning` が**断る** | true | **true** |
| ブートの持ち球切れ（#1093） | 今回は諦めた。次の契機では試す | true | false |
| bootstrap 拒否（#1075） | アダプタを作る前に断った | true | false |

検討して**採らなかった** 2 案を、なぜ駄目かごと残す。

### 却下1: ラッチ不在ならライブ health（`Health().LastErr`）に落ちる

起票時に自分で推した案で、契約に正面から反する。
`setup_desired.go` のインタフェース doc が
「モデル切替と再起動のたびに一瞬 unhealthy になる。それで行を赤くするのは
直そうとしているバグより悪い」と明記している（#330）。さらに `Stop()` は
give-up ガード無しで `Health` を丸ごと上書きするので、**寿命が短すぎて
`(true, "")`＝理由の無い赤い行**を再生産する。それは #310 が一度直した欠陥。

### 却下2: ブート経路でラッチさせる（打席を 1 つ増やす／閾値を下げる）

ラッチは「`EnsureRunning` が断る」という意味を持つ。ブートで立ててしまうと
**後から入ったエンジンを拾う唯一の経路**（`engine_bootstrap.go` の
「repeat は安いし、それが late install を adopt する」）が死ぬ。
「3 回で止まる」という今の挙動自体は正しいので、変えるべきは挙動ではなく
**記録の有無**だった。

### needsRepair をラッチ限定に据え置いた理由

`EngineNeedsRepair` は executor の presence gate を開け直す＝
**エンジンを入れ直す**。それは「起動すると必ず殺されるバイナリ」（#330 の
macOS 署名ケース）への答えであって、**ポートが塞がっている・venv が無い・
モデルが選ばれていない**のどれも入れ直しでは直らない。人が読む行のほうには
理由が出るので、面は塞がらない。

### 記録する文字列はアダプタから読み戻す

`hint` を組み直さず `Health().LastErr` を読み戻す。これでウィザードの行と
`runtimes[].last_error` が**構成上バイト同一**になり、
**新しいユーザー向け文言がゼロ**で済む（既存の give-up 見出しは
「自動再起動を無効にした」と言うので、まだ試す気のあるこの状態には使えない）。

### 寿命

**成功時ではなく、各試行ループの直前に消す。** 再試行中の正しい答えは
「まだ試している」であって前回の判定ではない。明示 start
（`waired inference engine start`）も消す — 人が何かを変えたのだから。

## Consequences

- ウィザードのエンジン行が、再起動しただけのホストで**緑を主張しなくなった**。
  実機の before は sv-evox2 で撮れている（`waired status` は
  `engine_failed` と理由、同じ瞬間に `status --observability` は
  `Engine: not ready (model=(unknown))`）。
- **同族の欠陥 3 件を同じ PR で直した**。どれも「理由はプロセス内に在るのに、
  それを見せると約束している面が読んでいない」:
  - #1106 `waired status --observability` の Engine 行。docs が
    `engine failed` という値と「理由は同じ行に出る」を約束していたのに、
    コードはその値を持っていなかった。**docs 側は無変更** — 約束を実装が満たす。
  - #1107 `waired status` が `installed=false` の行を丸ごと飛ばし、
    #1075 の合成拒否行の**最頻ケース**（venv 未作成）の理由を落としていた。
    ついでに warnings の map レンジ順（非決定）をソートに直した。
  - #1108 `engineFailureDetail` が `state=="failed"` だけを見ており、
    ラッチ後に Stop が挟まると `waired init` の 3 か所が無言になっていた。
- **#1109**: vLLM の give-up ラッチは `decideVLLMBootstrap` にラッチの腕が
  無かったため、`stop_first` → アダプタ作り直しで**どの契機でも消えていた**。
  ollama 側は明示的に断っているので片肺。`vllmBootstrapGaveUp` を足した。
  ここで訊くしかない — `EnsureRunning` の
  `ErrEngineUnrecoverable` ガードは差し替えの**後**に走る。
- **#1110**: 明示 start がアダプタの `ClearFailure()` は呼ぶのに
  provider の `crashStrikes` を戻していなかったので、ブート後の手動 start は
  1 打席しか無かった（docs は「3 回まで」）。`resetEngineStrikes` を足した。
- `setupEngineHealth` の戻り値が 2 → 3 になり、フェイクの
  `engineLatched` は `engineStopped` + `engineNeedsRepair` に置き換わった。
  **既存の表テストは 1 本も反転していない**: フェイクが
  `setupEngineHealth` を丸ごと差し替えるので、表からはこの穴が構造的に
  見えない。だから #1093 のテストは本物のアダプタを使う側に置いた。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1093
- https://github.com/waired-ai/waired-agent/issues/1106
- https://github.com/waired-ai/waired-agent/issues/1107
- https://github.com/waired-ai/waired-agent/issues/1108
- https://github.com/waired-ai/waired-agent/issues/1109
- https://github.com/waired-ai/waired-agent/issues/1110
- `docs/decisions/20260828/1830-the-give-up-message-carries-the-diagnosis.md`（#1069）
- waired#1283（L75 フォローアップ）
