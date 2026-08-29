---
status: accepted
---

# 排他クレームはエンジン種別を問わない (20260830 02:35)

## Status

Accepted。`docs/decisions/20260812/0331-one-exclusive-engine-measurement.md`
が置いた「エンジンを独占する計測は同時に1つ」を、その決定の当時は2つだった
計測が3つになったことに合わせて広げる。元の決定は生きている。

## Context

`claimEngineForBench` は `engineQuietForBench` と同じ「nil 端の理屈」を採って
いた:

```go
if p == nil || p.ollama == nil || p.servingEngine() != catalog.RuntimeOllama {
    return func() {}, true   // 無条件で渡す
}
```

`engineQuietForBench` にとってこの理屈は**正しい**。あれが守っているのは
ollama の pull レジストリと serve-env reconcile — 走っているベンチの下で
エンジンを stop→start してしまうもの — で、どちらも ollama のものだから、
vLLM ホストには待つ相手がいない。false を返せばそのホストのベンチを永久に
止めてしまう(20260809/1726 §4)。

クレームが答えているのは**別の問い**だ: 「他の計測が今エンジンを使っているか」。
20260812/0331 を書いた時点でこのホストの計測は2つ(install 時の host-speed と
boot/setup benchmark)で、どちらも ollama 経由だったので、ollama で絞っても
穴は開かなかった。

**waired-agent#1127 がそれを終わらせた。** prefill 計測は vLLM でも走り、
一度に数分エンジンを飽和させる。そして waired-agent#1150 が boot ベンチを
その隣で再試行ループにする。

今日ぶつかっていないのは、boot ベンチが同期で prefill ループより先に終わる
からにすぎない。ループにした瞬間、**vLLM ホストで3本が同時に走り、互いの
混雑を測る**。20260812/0331 が記録したとおり、その誤りは
`spread_pct` では判別できない(汚染 2.70% 対 クリーン 1.78%)。

## Decision

`claimEngineForBench` から ollama の条件を外す。nil provider だけが granted
を返す — 掴む state が無く、断ればフィクスチャの都合でベンチを止めることに
なるため。

`engineQuietForBench` は**そのまま**。守っている対象が本当に ollama 限定
だから。2つが別々の答えを返すのは矛盾ではなく、別の問いに答えているという
ことで、テストは両方をその形で pin する。

## Consequences

- vLLM ホストで初めて、install 時の host-speed 計測・boot ベンチ・prefill
  計測が互いを排除する。今まで存在しなかった保護
- 断られた側は既存の 425 の扉から降りる。boot ベンチは
  `notReadyBenchResult`、prefill 計測はゲートを保持したまま次の tick を待つ
- `#1150` のループは、断られたことを**判定として扱わない**ので、
  相手が離したあとに測り直す
- ループが 15 秒ごとに WARN を撒かないよう、ループの1ラウンドは
  `engineExclusiveHeld` を先に読んで静かに降りる。これは journal の量の話で、
  排除そのものはクレームが行う
- `TestClaimEngineForBench_AVLLMHostIsAlwaysHandedTheEngine`(「今日の挙動の
  記録」と明記されていた)を反転し、両エンジンでの排除を pin するテストと、
  `engineQuietForBench` が**動いていない**ことを pin するテストに置き換えた

## Refs

- https://github.com/waired-ai/waired-agent/issues/1150
- https://github.com/waired-ai/waired-agent/issues/1127
- https://github.com/waired-ai/waired-agent/issues/703
- docs/decisions/20260812/0331-one-exclusive-engine-measurement.md
- docs/decisions/20260809/1726-benchmark-yields-to-engine-restarts.md
- docs/decisions/20260830/0230-measure-once-per-selection-not-on-a-schedule.md
