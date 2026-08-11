---
status: accepted
---

# エンジンを独占する計測は同時に1つ (20260812 03:31)

## Status
Accepted

## Context

このホストにはエンジンを独占する計測が2つある。

- **host-speed 計測**（インストール時、#496）— `awaitQuietEngine` →
  `engineIsQuiet`
- **boot / setup benchmark**（#133 / #582 / #601）— `BenchDeps.EngineQuiet`
  → `engineQuietForBench` → 同じ `engineIsQuiet`

両方が同じ述語を引くのに、その述語が読んでいたのは pull / reconcile /
parked / health の4つだけで、**実際の推論要求も、もう一方の計測も見て
いなかった**。

`docs/decisions/20260811/2340-one-model-resident-at-a-time.md` で
`infruntime.MaxResidentModels = 1` を入れた結果、この盲点の代償が変わった。
エンジンが同時に1モデルしか保持しないので、計測中に届いた仕事は probe と
**競合する**のではなく**互いを追い出す**。sv-xps15 の実測で probe の再ロードが
約8秒、4B serving モデルの再ロードが約13秒。

sv-xps15 で実際に起きた並走（2026-08-11）:

```
17:19:01  waired init --non-interactive 再実行
17:19:57  benchmark が 4B を回す
17:20:41  host-speed が 39.473 s を publish   ← クリーン値は 12.017 s
```

3.3 倍の誤り。しかも**サンプル間のばらつきでは判別できない** — 汚染された
測定の `spread_pct` は 2.70%、クリーンは 1.78% で、競合が3サンプル全体に
一様にかかるため統計的なガードは効かない。数字そのものは
`waired#1140` が記録した「遅いホスト」の署名と区別がつかない。

benchmark 側は静穏でないと分かれば**すでに正しく譲る**
（`notReadyBenchResult` → 425 → init が再試行）。仕組みは既にあり、
信号が届いていなかった。

## Decision

**エンジンを独占する計測は同時に1つ**とし、次の4点で担保する。

1. **排他トークンを1つ置く**（`agentInferenceProvider.engineExclusive`,
   `atomic.Bool` の CAS）。host-speed 計測・benchmark・serving モデルの
   warm-up が取る。取得は必ず try で、**待たない** — 取れなかった側は
   それぞれ既存の譲り方を使う（benchmark は 425、計測は次のブート、
   warm-up は skip して計測側の defer で呼び直される）。
   状態が1つなので、片方だけがもう片方に盲目になりようがない。

2. **実際の推論トラフィックを静穏判定に入れる**。
   `inference.Server.InflightCount()`（peer + オーナーのローカル作業の両方）を
   既存の `localAdmissionRelay` 経由で読む。

3. **probe 区間をまたいだ汚染検知**。`inflightCounter` に累積カウンタ
   `admitted` を足し、`measureHostCutoff` の前後で読み比べて、動いていたら
   **publish しない**。probe は45秒以上走るので、その間に開始して終了した
   要求は 2 のゲージでは両端とも0に見える。既に `EngineGen` が自分の
   再起動に対してやっているのと同じ「事象を数える／症状を分類しない」形。

4. **`waired init` の step 6 が、自分が要求した新しい値を待つ**。
   `measured_at` 文字列の変化で判定し（CLI とデーモンの時計が別なので
   時刻演算はしない）、計測が値を出さずに終わった場合は新設の
   `host_speed_stage` で待ちを打ち切る。

トークンは**待つ側だけが読む**（`engineIsQuietAndUnclaimed`）。両方の計測が
保持中に静穏を再問い合わせする（benchmark は bounce-grace のリトライごと、
screen は `measureHostCutoff` の内側から）ので、述語がトークンを読むと
**自分自身で止まる**。

## Consequences

- インストール時の測定値が、同居する仕事ではなくホストを表す。汚染された
  読みは publish されず、保存済みの値が残って後のブートで再試行される。
- **`waired-agent#599` のオーナー裁定（再実行は計測をやり直す）を revert
  せずに済む**。引き金は順序で外れ、一般的な防御も入る。
- ピアに常時サーブしているホストは計測を延期しうる。`awaitQuietEngine` の
  `hostSpeedSettleWait`（60分）で打ち切って「今回のブートでは測らない」に
  倒れる既存動作の範囲内。
- step 6 は、計測が始まらないホストで最大 `hostSpeedAskWait`（20分）待って
  から手持ちの値で判断する。#623 の進捗行が経過を出すので沈黙にはならず、
  タイムアウト時はローカル推論を on のままにする fail-open。
- 誰も計測していないときの通常経路は何も変わらない。トークンは CAS 1回、
  カウンタは atomic の読み取り2回。

## Refs
- https://github.com/waired-ai/waired-agent/issues/703
- https://github.com/waired-ai/waired-agent/issues/599
- docs/decisions/20260811/2340-one-model-resident-at-a-time.md
- docs/decisions/20260809/1726-benchmark-yields-to-engine-restarts.md
- docs/decisions/20260807/1700-host-speed-is-an-install-time-step.md
