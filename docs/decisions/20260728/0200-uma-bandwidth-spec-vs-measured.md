---
status: accepted
---

# UMA メモリ帯域は「仕様ピーク」と「実測」を別フィールドで持つ (20260728 02:00)

## Status
Accepted

## Context
`proto/hostfit` の roofline はトークンあたり active bytes ÷ メモリ帯域で
decode 速度を予測する。分子は manifest 由来で正確だが、分母は unified
ホストでは定数 `BandwidthUnifiedGBs = 120` ひとつで、実際の母集団は
**68〜819 GB/s** に広がっている(M1 base 68.25 / M3 Ultra 819)。

単一の値はこの範囲のどちら側の bound にもならないため、
`EstimateOllamaDecode` は `ClassUnified` で `UpperBound` を立てず、
`model_picker.go` の `narrow(!UpperBound || MeetsSpeedFloor)` を素通り
していた。速度は計算され表示されるが、何も決めない。#229 が 24GB Mac の
誤推薦に「遅いかも」を付けただけで推薦自体を残したのはこのため。

除外を許すには**上限**が要る。ここで、帯域の値には出どころが 2 通りあり、
**互いに逆向きの bound になる**という問題が出た:

* **仕様ピーク**(部品の公称値) — その個体が超えられない値なので **上限**。
* **実測**(#252 が計画している DMI/WMI/sysctl 由来や CPU 側ベンチ) —
  UMA では CPU コアが引ける量までしか測れず、GPU が同じプールから引く量に
  届かないので **下限**。

## Decision
`signer.HardwareSummary` に **`memory_bandwidth_spec_gbs` のみ**を追加し、
実測値は将来 **別フィールド** `memory_bandwidth_measured_gbs` (#252 が所有)
として持つ。1 フィールド + `is_spec` bool のような判別子方式は採らない。

`ClassUnified` は spec 値がある時だけ `UpperBound` を立て、無い時は定数へ
フォールバックして annotate-only のまま(= #251 以前の挙動)。

仕様ピークに de-rate はかけない。ピークは意図的に上限であり、実効値へ
引くと上限の意味が壊れて除外が過剰になる。「ピークですら遅い」が除外に
必要な唯一の主張。

## Consequences
* proto は additive-only なので、この形は後から直せない。判別子方式だと
  #252 が実測値を載せた瞬間に `is_spec=false` となり、**情報を足したのに
  除外能力を失う**退行が起きる。2 フィールドならそれが起きない。
* 両方を同時に持てる: spec で除外し、measured で注記を精緻化できる。
* consumer が bool を読み落として bound を反転させる経路が存在しない。
* 代償はフィールド数が 1 つ増えること。additive-only の下では
  「後から分けられない」方が高くつくと判断した。
* #252 が実測値を実装する際は、単発測定ではなく **N サンプルの中央値 +
  ばらつき**で publish すること(boot ベンチの `benchSampleCount=3` /
  `medianFloat` / `spreadPercent` と同じ規律、OS を問わず)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/251
- https://github.com/waired-ai/waired-agent/issues/252
- https://github.com/waired-ai/waired-agent/issues/229
- docs/knowledges/20260728/0200-apple-silicon-bandwidth-table.md
