---
status: accepted
---

# ベンチマークはエンジンの再起動に道を譲る — 静穏ゲートと世代グレースで (20260809 17:26)

## Status

Accepted。#582 が挙げた 2 案のうち、案(2)「再起動をまたいだ run は測定失敗では
ない」を採り、案(1)「ベンチ実行中は serve-env reconcile を保留する」は採らない。
案(2) の「再起動とクラッシュをどう見分けるか」は、
docs/decisions/20260805/1311-engine-bounces-and-in-flight-downloads.md が
ダウンロード側で確立した方法——**エラーを分類せず、自分が止めた回数を数える**
——をそのまま使う。

## Context

ブート末尾（`bootstrapAfterEngineStart`）は、#496 のホスト速度計測（probe モデル
~1.0 GB）と運用者のモデルを、ほぼ同時に dispatch する。`endPull` は**最後の 1 本**が
抜けたときにだけ保留中の serve-env reconcile を発火し、reconcile は `ollama serve` を
stop→start する。つまり **後に完了した方**がエンジンをバウンスする。

運用者のモデルが先に完了すると、activate → `EngineReady()` が true → `waired init` の
POST `/inference/benchmark` が warm-up を始め、その 2.4 秒後に probe 完了の reconcile が
エンジンを落とす。warm-up は `EOF` を受け取り、`benchOutcomeFailed` → 503 →
`waired init` は exit 3。これは `install.sh` の `WAIRED_INIT_LOCAL_AI_DOWN` /
`install.ps1` の `$WairedInitLocalAIDown` が分岐する値なので、**エンジンが健全な
ホストでインストールが失敗**する（#601 / #582、routing sentinel の run
31294752083・31295726777・31260375030）。

probe が先に完了する順（対照 run 31273048503）なら、reconcile は benchmark 開始前に
終わっていて緑になる。**どちらが先かは帯域のコイントス**で、CI 構成（350M pin <
probe 1.0GB）は悪い側を引きやすい。

計測側（`awaitQuietEngine`）には同じ教訓が既にコメントとして書かれている——
「計測は operator のモデル DL が終わる 400 ms 前に始まり、それが起こした reconcile の
3 ms 後に死んだ」。boot benchmark には同じ保護が無かった。

## Decision

### 1. 静穏ゲートを benchmark 側に置く（案(1) は採らない）

`RunBootBenchmark` に `EngineReady`（#203）と同じ形の 2 枚目のゲート
`BenchDeps.EngineQuiet` を足す。pull が in-flight、または reconcile が pending/実行中
なら **待たずに** `benchOutcomeEngineNotReady` を返す。

**なぜ reconcile 側の保留ではないか。** 保留だけだと、pull が続いている最中に
benchmark が走り出せてしまう。`awaitQuietEngine` の doc が記録しているとおり、
インストール中の計測は contention の計測であり、3 サンプルの中央値では補正できない。
それは *失敗* の代わりに *間違った数値* を出すので、より悪い。判断は「測ってよい
ホストか」を知っている側、すなわち benchmark 側に置く。

**待たない**のも意図的。not-ready は既存の 425 扉（#576/#581）で、`waired init` は
3 秒ごとのポーリング、setup reconciler は再 kick を既に実装している。ベンチ側に
新しい待ちループを作ると、CLI が状況に応じた待ち文言を出せなくなる。

### 2. 取りこぼす窓は、エンジンのプロセス世代で受ける

ゲートを通った直後（実測 3 ms）に pull が完了して reconcile が来る窓は残る。
`BenchDeps.EngineGen`（= `engineProcessGen`）を warm-up の前に読み、失敗時に読み直す。
**世代が動いていれば、止めたのは我々**——ホストの速度についての言明ではないので、
その試行は計上せず再試行する。エラーテキストで分類しないので、後から追加される
バウンス経路も自動的にカバーされる（`runPullJob` と同じ性質）。

再試行の先頭では静穏ゲートを再評価する。エンジンがまだ戻っていなければ 425 で返し、
待つのは CLI のポーリングに任せる。

上限は `benchEngineBounceGrace = 2` — `enginePullBounceGrace` と同じ根拠で、ブート
末尾が 1 回で与えうる再起動が最大 2 回だから。グレースを使い切ったら正直に
`failBench`（503 → exit 3）に落とす。**再起動ループに陥った本当に壊れたエンジンの
シグナルを消さない**ことが、この上限の存在理由。

### 3. not-ready の形は 1 か所で作る

2 つのゲートは `notReadyBenchResult` を共有する。返す `Outcome` は既存の
`benchOutcomeEngineNotReady` そのもので、新しい値を作らない——`RunBenchmark` の
ok=false、management の 425 マッピング、そして **single-flight で join した呼び出し側**
（#595、`TestRunBenchmark_JoiningANotReadyJobAlsoAnswers425`）が全部この値に
乗っている。#601 の exit 3 の最終段は CLI が失敗 run に join することなので、
ここが同じ扉を通らなければ CI シグナルは残る。

### 4. vLLM ホストは常に「静穏」と答える

`engineQuietForBench` は、ollama アダプタが無い / serving engine が ollama でない
場合に true を返す。ガード対象の pull レジストリと serve-env reconcile はどちらも
ollama のものなので、false にすると **vLLM ホストの benchmark を恒久的に止めてしまう**。

## Consequences

- インストール中の benchmark 要求は、pull が捌けるまで 425 を返す。`waired init` の
  transcript には対照 run と同じ「Waiting for the AI engine to load the model before
  benchmarking…」が出て、その後に実測値が出る。
- 1 回の benchmark run は、最大 `benchEngineBounceGrace + 1` = 3 回 warm-up を投げうる。
  無罪リトライは backoff を置かない（待つべきものは静穏ゲートが表現している）。
- **カバーしないもの**: probe pull と運用者の pull が並行に走ること自体は変えていない
  （#579）。ブート末尾の順序入替は waired#1099「モデル選択の前に figure を出す」意図と
  衝突しうるので、別の設計が要る。
- 測定の途中サンプルが再起動で落ちた場合は従来どおり「完了したサンプルで中央値を出す」
  （`measureOllamaNative`）。再起動前に取れたサンプルは健全なエンジンのものなので、
  ここは変えていない。
