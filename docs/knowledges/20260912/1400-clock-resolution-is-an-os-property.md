# 時計の分解能は OS の性質 — 2 点の順序を時刻で決めない (20260912 14:00)

## Issue

waired-agent#1304 で「この脚が走っている間に、この機械が自分のエンジンを止めたか」
を判定する必要があった。素直な実装は「停止に時刻を刻み、脚の開始時刻と比べる」で、
**手元の Linux では常に緑、CI の Windows / macOS レグでだけ落ちた**。2 回落とされた:

1. `time.Now().UnixNano()` を `atomic.Int64` に入れて比較。`UnixNano()` は
   **単調時計の読みを捨てて壁時計だけ**にする。壁時計の分解能は Windows で
   最大 15.6 ms。
2. `atomic.Pointer[time.Time]` に入れ、`After` で比較。単調時計は保持されたが
   **それでも落ちた**。

## Learnings

**Windows の Go は単調時計(`nanotime`)も既定 15.6 ms 粒度**。`time.Time` が
単調読みを持っていても、マイクロ秒差の 2 回の `time.Now()` は同値になり、
`After` が false を返す。macOS の CI ホストでも同じ形で落ちた。
**「`UnixNano()` を避ければ安全」は誤り。**

**手元では再現しない。** Linux の時計は細かいので、2 回の `time.Now()` を
並べるテストも、それを 1000 回回すループも通る。**直しを戻す変異でも噛まない** ——
`docs/decisions` にある「変異させてテストが噛むか確かめる」型の検証が、
ここでは何も言わない。ローカル緑が本当に何も言っていない数少ない場面。

### 順序を判定したいならカウンタ

`atomic.Uint64` を 1 つ増やし、読み手は「前に読んだ値」と比べる。分解能が無いので
OS に依存しない。このリポジトリでは `internal/runtime` の
`OllamaAdapter.ProcessGeneration` が pull 経路で既にこの形を使っている
(「waired がエンジンを再起動した」の猶予判定)。#1304 の
`agentInferenceProvider.engineStops` も同じ形に落ち着いた。

```go
before := deps.Count()
// … 仕事 …
if deps.Count() > before { /* その間に起きた */ }
```

**時間の長さ(経過時間・予算・タイムアウト)は時計で良い。** 危ないのは
**2 点の順序**だけ。

**How to apply:** 「2 つの時刻の順序」で製品の判断をゲートしそうになったら、
まずカウンタで書けないか考える。書けないなら、そのテストは
**Windows と macOS の CI でしか嘘を暴けない**ことを PR 本文に書く
(CLAUDE.md §Cross-OS parity の「1 OS だけ違う」が、パスではなく時計から来る形)。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1304
- `docs/decisions/20260912/1130-a-bounce-we-chose-waits-for-the-turns-on-it.md`
