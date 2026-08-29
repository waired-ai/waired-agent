---
status: accepted
---

# ベンチマークは選択ごとに1回、周期ではない (20260830 02:30)

## Status

Accepted。waired-agent#1150 の「再試行にしてほしい」という要求と、
waired-agent#202 の「周期的な合成ベンチはやめろ」という反対を、同時に満たす形。

## Context

`RunBootBenchmark` はブート末尾で `EngineReady` に**一度だけ**訊き、答えが
false ならそのデーモンの生涯で二度と走らなかった。vLLM のエンジンは daemon より
1分ほど遅れて上がるので、ほぼ毎回負ける。

実測(1台の vLLM ホスト、7日間):

```
     65 inference boot benchmark not run: no model selected yet
     14 inference boot benchmark not run: the engine is busy
      5 inference boot benchmark completed
```

同じ7日間で `msg` に "cache" を含む行は **0本**。5回の完走はすべて
`Cache: nil` を渡す明示ベンチ経路(ウィザード / `waired runtimes benchmark`)で、
**ディスクキャッシュの唯一の書き手であるブート経路は一度も完走していない**。

決定的な証拠は同一ブートの2行:

```
09:15:42.612  boot benchmark not run: no model selected yet   engine=vllm
09:16:15.724  prefill measurement: rung completed             engine_kind=vllm
```

33秒後に、#1127 のポーリングループが**同じエンジンで**完走している。

## 対立する2つの要求

- **#1150**: 一度きりをやめて再試行にする
- **#202**: 定期的な合成ベンチはやめるべき。モデルを VRAM に固定してしまうし、
  忙しいホストでは混雑を測ることになる。実トラフィックのカウンタから
  推定するほうがよい

どちらも正しい。#202 が反対しているのは**周期的に測り直すこと**であって、
**測れるようになるまで待つこと**ではない。

## Decision

ループにする。ただし**測る条件は「選択が変わったとき」だけ**にする。

`bootBenchSelectionKey` = `(ModelID, VariantID, EngineKind, EngineVersion)`。
この4つのどれかが変わるまで、ループは何度訊いても仕事をしない。

- `measured` と `failed` が**判定**。鍵を立てて、その選択では二度と測らない
- `engine_not_ready` と `skipped` は判定ではない。エンジンに届いていないので、
  このホストについて何も言っていない
- **失敗も判定に含める**のは #203 の線引きを引き継ぐため。アクセラレータの
  OOM や warm-up のタイムアウトは**このホストについての言明**で、15秒ごとに
  やり直せば答えられない機械のエンジンを飽和させるだけになる。
  `speedMeasuredFor`(#1127)が同じ扱いをしている

つまり、**測る回数は今までと同じ「選択ごとに1回」で、変わったのは
「いつ測れるか」だけ**。#202 が心配する周期的な負荷は発生しない。

同期の1回は残す。キャッシュヒットはマイクロ秒で答えるので、最初の probe tick が
fail-safe ではなく実測の capacity を広告できる。負けたホストでは同じ速さで
戻ってきて、#633 が pin した not-ready の1行を残し、あとはループが引き取る。

### deps は毎回読み直す

一度きりの実装はブート時に1回だけ読んでいた。新規インストールではその時点で
まだ何も存在しない — 選択がコミットされていないので model id も variant id も
variant digest も空(digest が空だと**無言でキャッシュが無効になる**)、
`probeTargetForActive` は vLLM で配信することになるホストに対して
"ollama" を返す。probe ループは同じ組を tick ごとに読み直している(#948 / #656)。
ベンチだけがブートのスナップショットに取り残されていた。

## Consequences

- ループの定常コストは poll そのものだけ。`EngineReady` (アダプタの health +
  state ファイル) と、選択が変わっていなければそこで終わる
- ブートのベンチにぶら下がっていた **depth sweep** も判定に付いてくる。
  今までは `!bench.Failed` に直結していたので、レースに負けたホストは
  long-context の値も永久に得られなかった
- トグルの読みもループの中(毎tick、`EngineReady` 経由)に移った。今までは
  ブート時に1回読んで、その1回がベンチ・depth sweep・#1127 の速度計測を
  全部囲っていたので、**起動後にローカル推論を ON にしたホストは3つとも
  一切走らなかった**
- #202 は閉じない。実トラフィックのカウンタから推定するという提案は、
  合成ベンチを完全に置き換える別の設計として生きている

## Refs

- https://github.com/waired-ai/waired-agent/issues/1150
- https://github.com/waired-ai/waired-agent/issues/202
- https://github.com/waired-ai/waired-agent/issues/203
- docs/decisions/20260809/1726-benchmark-yields-to-engine-restarts.md
- docs/decisions/20260830/0235-the-engine-claim-is-engine-agnostic.md
