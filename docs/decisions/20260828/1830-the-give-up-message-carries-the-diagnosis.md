---
status: accepted
---

# give-up メッセージは診断を運ぶ、置き換えない (20260828 18:30)

## Status

Accepted。waired-ai/waired-agent#1069 / #1075 / #1076。
`docs/decisions/20260828/0100-engine-power-and-the-vllm-port.md` の続き ——
あちらは vLLM が起動に失敗したときに**諦める**ことを決めた。ここで決めるのは
**諦めたあと、面が何を言うか**。

## Context

#1026 の修正を実機で検証していて、その修正自身の穴が出た。

塞がったポートに vLLM を向けると、`runtimes[vllm].last_error` の先頭には
#1026 が足した名指しの一文が出る。ところが**4 回目の失敗でラッチが立つと、
その一文が消える**。`waired status` も `waired runtimes ls` も `runtimes
status` も後者を引用するので、**諦めたホスト —— 人が実際に見つける状態 ——
では「4 回失敗しました」と Python のトレースバックだけが残り、どのポートで
どの設定かを言うものが何も無い**。

機構は上書き 1 か所ではなく、**順序の無い 2 人の書き手**だった。

- `LatchFailed` は両エンジンとも `a.state = Health{...}` の丸ごと代入。
- `SetStartFailureReason` は `LastErr` への唯一の read-modify-write で、
  **vLLM だけに存在し、呼び出し元 1 つ、テスト 0**。
- `OnStartFailed` は `runStart` の defer から `go cb(...)` で飛び、
  `SetStartFailureReason` は bootstrap の goroutine が `EnsureRunning` から
  抜けたあとに呼ぶ。**latch が後なら診断が消え、先なら診断が二重になる。**
- しかも正しい寿命を持つ `giveUpErr`（`Stop` で消えず `ClearFailure` まで
  生きる）には診断文が**一度も入らない**。だからウィザードの
  `setupEngineHealth` は give-up 文しか出せなかった。

同じ検証で 2 件が併発した。**#1075**: `bootstrapVLLM` がアダプタを作る前に
断つと記録がログ 1 行しか残らず、`subsystem_state` は `ready` と言う。
**#1076**: `waired doctor` は理由を一切言わず（`AgentState` に欄が無い）、
かつ準備完了行のエンジン名がリテラルの `ollama` だった。

## Decision

**1. 書き手を 1 人にする。** give-up 文の組み立ては `engineGiveUpMessage`
一箇所に集約し（従来は 4 か所の `fmt.Sprintf`）、**ストライク処理が
ラッチする前に診断して先頭に畳む**。これで `state.LastErr` と `giveUpErr` の
両方に理由が乗り、レースが消える。診断が空なら出力は従来と**バイト同一**。

**2. 診断の入力は `detail`。** アダプタ API の追加もファイル再読み込みも
要らない —— `startupExitError` / `servingExitError` / 起動待ちの deadline 枝 /
`markUnhealthy` は、コールバックを撃つ前に `engine.log` の tail を畳んで
いる。`LastEngineLogSpawn` に通すのは vLLM では banner が tail に入るから、
ollama では**ログが spawn ごとに truncate される**ので無害。

**3. `SetStartFailureReason` はラッチ済みなら no-op。** ラッチが自分で診断を
畳むようになったので、bootstrap 側の prepend が後から重なると同じ文が 2 回
出る。役割分担は「ラッチは自分の文を書く / これは budget を超えずに終わった
bootstrap の隙間を埋める」。ollama にも同メソッドを足した。

**4. `ollamaStartupDiagnosis` は腕 1 本で出す。** 双子が課す基準は
「**このプロジェクトが名前の付いたホストから実際に採取したエンジンの文**」。
入れたのは**ポート衝突の腕だけ**で、その文字列は本変更のために実機で採った
（sv-mag、python の listener で :9475 を塞ぎ、同梱 ollama を起動）:

```
Error: listen tcp 127.0.0.1:9475: bind: address already in use
```

vLLM の Python OSError と違い **ollama 自身がアドレスを名乗る**。それでも
`addr` は config 由来のものを使う —— エンジンが「そう言われた」値のほうが
権威がある。文面は 1 つのビルダ（`enginePortBusyDiagnosis`）に集約した:
vLLM 側の文言は docs-site に逐語で引用されているので、コピーが 2 つあると
片方を直したときに黙ってドキュメントと食い違う。

この腕が撃つのは **ollama 以外**が握っている場合だけ。別の ollama は
`EnsureRunning` が `/api/version` を先に叩いて adopt するか、版と
`inference.ollama_port` を名指しして断る。何も言えずに残っていたのが
非 ollama の場合で、これは #1026 が vLLM で塞いだのと同じ穴である。

落としたものと理由:

- **`$HOME is not defined`**（#22 → PR#24）—— 実機で採った本物だが、
  **原因が二重に直っている**（LaunchDaemon の plist が HOME を出し、
  `ChildBaseEnv` が起動元が渡さなかったときに注入する）。両方が同時に退行した
  ときしか撃たない。それは**人に見せる文ではなくテストの仕事**。
  リリース前なので古い agent との互換も考えなくてよい（オーナー指摘）。
- `signal: killed`（macOS の署名検証）—— 二重に成立しない。AMFI は `exec` で
  殺すので **engine.log に 1 バイトも書かれず**（しかも `openEngineLog` は
  直前に前回分を `.1` へ回している）、終了ステータス側の文字列を見ても
  **OOM kill と区別不能**だと monorepo の決定記録 2 本が明言している。
- `no Metal device` —— 手書きテストフィクスチャにしか存在しない。実機の
  macOS 事案で engine.log を採ったら答えは `$HOME` で、PR#24 は
  "Not Metal / tart / OOM" と書いている。
- `CUDA error: out of memory`（#1038）と llama-server の segfault（#29）——
  どちらも実在するが **500 レスポンス本文**であり engine.log ではない。
  既に `OnFitFailure` と `markUnhealthy` が持っている。

Windows の言い回し（`Only one usage of each socket address`）は Go の `net` が
OS から受け取る固定文言なので**入れてはあるが、Waired の Windows ホストでは
まだ観測していない**。#1085 で追跡する（CLAUDE.md §Cross-OS parity の
「理由を書いた OS ラベル issue で覆う」）。darwin は Linux と同じ POSIX の
文言なので今回の採取で覆われている。

**5. 断った bootstrap は状態になる（#1075）。** 3 つの拒否を provider に
記録し、`subsystemState` に腕を足して **`engine_failed` + 理由**にする
（値を拒否ごとに分けない —— 面に伝えるべきことは 3 つとも同じで、
`engine_failed` だけが「待つのをやめて理由を出す」読み手を既に持っている）。
記録は **`servingAdapter()` が nil のときだけ読む**: 生きているアダプタの
読みのほうが常に具体的で、しかも配信中の状態（`ready`）はどのエンジン腕にも
当たらないので、古い記録が残ると**健康なホストでだけ**間違う。

`Status()` が配信エンジンの runtimes[] 行を**合成する**。registry は
アダプタと同時にしか vllm を持たないので、断った時点では行が無く、
`waired status` の ⚠ も `runtimes ls` も `engineFailureDetail` も tray も
言うことが無かった。合成行 1 つで全部が動き、**ワイヤに新しい欄は要らない**。
`endpointState` が記録済みの `ready` を素通ししていたのも同時に止まる。

**6. doctor が理由とエンジン名を言う（#1069 後半 + #1076）。**
`EngineProvenance` の 4 文字列タプルを構造体にして**配信エンジン**について
答えさせ、`management.AgentState`（localhost API であって proto ではない）に
`engine_name` と `engine_failure_reason` を足す。理由は**先頭 1 行**だけ ——
`last_error` は give-up 文 + 生エラー + 4KiB のログ tail で、残りは
`waired runtimes status` が全文を出す。

## Consequences

- 諦めたホストの `last_error` / `giveUpErr` / ウィザードのエンジン行 /
  `waired doctor` が、同じ名指しの一文を持つ。
- ollama ホストの出力は**今日と変わらない**（診断表の腕が `$HOME` 1 本で、
  それ以外は空文字を返すため）。将来 ollama 側に腕が増えれば自動的に乗る。
- `AgentState` に欄が 2 つ増えた。古い daemon は送らないので、doctor は
  従来どおりの文言に落ちる。
- `engine=ollama` のリテラルが消えた。**古い daemon は名前を送らないので、
  その場合は接尾辞ごと出さない** —— 推測して名乗るよりよい。
- `subsystemState` の腕が 1 つ増えた。既存の表テストの
  「アダプタ無し → ready」行は**反転していない**: その行は新しい事実を
  セットしないため。
- **`SetStartFailureReason` に初めてテストが付いた**（従来 0 本）。
  `onVLLMEngineStartFailed` も同様。

## Refs

- waired-ai/waired-agent#1069, #1075, #1076, #1026, #310, #22
- `docs/decisions/20260828/0100-engine-power-and-the-vllm-port.md`
- waired-ai/waired#1283 (L75)
