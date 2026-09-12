---
status: accepted
---

# こちらの都合で落とすエンジンは、走っているターンを待つ (20260912 11:30)

## Status

Accepted。`docs/decisions/20260813/2123-model-swap-applies-in-process.md` が
記録した「in-process 切替」の**代償**を後から埋めるもの。2123 の記述
(「常用経路は再起動しない」= エージェント・management API・gateway・mesh が落ちない、
bounce するのは `ollama serve` だけ)は正しいまま、その bounce が誰を巻き込むかを
決めていなかった。`docs/decisions/20260904/0215-a-hangup-is-not-the-engines-failure.md`
が引いた「inbound がもう居ないならエンジンの失敗ではない」の線を、
リクエストの反対側へ延ばす。

## Context

0.0.3-rc6 の実機検証(waired-ai/waired#1349、追試 waired-agent#1304)で、
モデル切替が実行中のターンを切ることが 2 つの OS で観測された。

- Linux: `ollama pull completed` → `reconciling ollama serve env … switch:true` →
  実行中の 27b ターンが `engine_truncated_stream`(`unexpected EOF`)、
  ゲートウェイの再描画リトライは `dial tcp 127.0.0.1:9475: connection refused` で落ちた。
- Windows: 35k トークンのターンが keepalive を流している最中に NAVI から切替 →
  200 のストリームの中に `upstream_error … wsarecv: An existing connection was
  forcibly closed by the remote host` が出て、gateway は `engine_request_failed 502`。

コードを読むと、`reconcileEngineServe` のログ行と `p.ollama.Stop(ctx)` の間には
**何も無い**。`engineOpMu` は `startEngineAndBootstrap` との排他(#304)であって、
リクエストとは無関係。in-flight を数えるカウンタ(`internal/inference` の
`inflightCounter`、gateway が `engine_inflight` として記録している同じもの)は
既に在るのに、bounce は一度も読んでいなかった。

2123 の「その間は旧モデルが応答を続ける」は**重みのダウンロードを待っている窓**に
ついての記述で、bounce そのものは誰も待たない。#1304 がこの 2 文を不一致として
挙げたのは、記述が足りていなかったからである。

## Decision

### 1. こちらが決めた bounce は、エンジンが空くのを待ってから落とす

待つのは `servingInFlight()` —— **自分のターンとピアに配信中のターンの両方**。
どちらも bounce で切れるし、どちらも bounce を頼んでいない。

待つのは**こちらが決めた bounce だけ**:

| bounce | 待つ | なぜ |
|---|---|---|
| 操作者のモデル切替(`swapPending`) | する | #1304 そのもの |
| residency の変更(`engineRespawnPending`) | する | 同じ形 |
| CP 由来の並列度変更(`ServeInputsEqual` 差分) | する | 同じ形 |
| クラッシュ回復(`engineRecoverPending`) | **しない** | エンジンは既に死んでいる。待てば停止が延びるだけ |
| `waired inference engine stop`(park) | 対象外 | 操作者が「今止めろ」と言っている |

`deferRetuneWhilePulling` が引いた線と同じ形にし、同関数の
「**操作者のモデル切替は待たせない**」とは衝突しないことを明記する。
あれが切替を待たせないのは**本人が頼んでいないダウンロード**の後ろであり、
ここで待つのは**本人が投げたターン**である。

### 2. 切替の待ちは Active を倒す前に置く

`reconcileEngineServe` は swap のとき `activatePreferredIfNeeded` で
Active を新モデルに倒してから bounce する。Active は各面が
「新しいモデルが今答えている」と読む事実で、tray のカタログ行は
それが倒れた瞬間に `(switching…)` を落とす(`internal/gui/tray` の `applyCatalog`)。
先に倒してから待つと、**真の文(旧モデルがまだ答えている)の前に偽の文を置く**ことになる。

他の bounce は `ServeInputsEqual` の早期 return の後で待つ。bounce するか否かが
そこまで確定しないので、先に待つと空振りの reconcile が予算を捨てる。

### 3. 予算は 10 分(オーナー裁定 2026-09-12)

待ちは無制限にできない。ここは新規の到着を止めない —— admission は gateway 側で、
止めれば truncation を避けるために 503 を返すことになる —— ので、
負荷のある機械では in-flight が 0 に落ちないことがあり、切替が永久に適用されない。

30 秒も候補だったが却下。レビュー機の Claude Code 初回ターンは
**最初の 1 バイトまでに 34〜84 秒**かかっており、短い予算は
この仕組みが守ろうとしているターンをちょうど切る。

`inference.engine_drain_budget_ms` / 環境変数 / フラグで変更可能。
**0 は #1304 以前の挙動**(待たない)。

待っていること自体は tray の `(switching…)` が出し続ける
(`SwapPreferredModel` は bounce より前に preference を公開する)ので、
10 分の待ちは面の上で「切替中」と読める。45 秒の `armSwitching` grace は
**エージェント再起動**の窓で、本件とは別物。

### 4. 予算切れで切ったターンは、正直な名前で終わらせる

予算が尽きても bounce はする。切れたターンは `engine_truncated_stream` /
`engine_request_failed` / `mid_stream_truncate` ではなく **`engine_restarted`** で
記録し、読む人には「Waired がエンジンを再起動した。選んだモデルで戻ってくる。
もう一度送ってほしい」と出す。ソケットのエラー文字列は同じ出来事を
OS の言葉で言い直しているだけで、読む人が使えるものが無い。

判定は**この要求についての事実**として行う: provider が意図的な停止を**数え**、
gateway は脚がディスパッチする前に一度読み、失敗したらもう一度読む。動いていれば
「この脚の下でエンジンが抜かれた」。猶予窓は要らないし、たまたま bounce の近くで
起きた失敗は入らない。クラッシュ回復は数えない —— 自分で死んだエンジンは、
誰の指示でもターンを終えていない。

**時刻ではなく数** にしたのは様式の問題ではない。「停止に時刻を刻み、脚の開始時刻と
比べる」は自明な実装だが**答えられない**: どちらの値も `time.Now()` から来るのに、
**時計の分解能は OS の性質**である。Windows では壁時計だけでなく**単調時計も
15.6 ms 粒度**なので、マイクロ秒差の 2 回の読みが同値になり、脚の下で起きた停止が
「後ではない」と比較される。この形は CI の Windows / macOS レグが 2 度捕まえた
(UnixNano 版と `time.After` 版)。手元の Linux 機はどちらも通した。数には失う
分解能が無い。同じ形は `infruntime` の `ProcessGeneration` が pull 経路で既に使っている。

### 5. 透過的な再ディスパッチはしない

「コミット前なら新エンジンで透過的に再試行する」は #1304 の期待に挙がっていたが、
採らない。ゲートウェイが保持しているボディは `rewriteModelField` 等で
**切替前の engine model** を名乗っており、そのまま再 POST すると
切替が置き換えたはずのモデルを新しいエンジンにロードさせる。正しくやるには
選択からやり直す必要があり、それは別の仕事。ドレインで切らないことと、
切ったときに正直であることで、まず閉じる。

## Consequences

- 操作者の切替が、実行中のターンの分だけ遅れる(最大 10 分)。tray はその間
  `(switching…)` を出す。NAVI の面は別レーンの担当なので、
  「切替が最大 10 分かかり得る」は申し送る。
- `engineReconcileInFlight` が最大 10 分立つ。`engineIsQuiet` はその間 false を
  返すので、host-speed 計測と boot benchmark はその窓を見送る。どちらも
  「次の起動で測り直す」設計なので待ちは失敗ではない。
- `engine_truncated_stream` を grep する人が、truncation だけを見るようになる
  —— 0215 が `client_disconnected` について達成したのと同じこと。
- ドレインは新規の到着を止めないので、飽和した機械では予算が尽き得る。
  そのときに起きることは (4) が引き受ける。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1304
- `docs/decisions/20260813/2123-model-swap-applies-in-process.md`
- `docs/decisions/20260904/0215-a-hangup-is-not-the-engines-failure.md`
- `docs/knowledges/20260912/1100-claude-code-gives-up-on-a-silent-leg.md`
