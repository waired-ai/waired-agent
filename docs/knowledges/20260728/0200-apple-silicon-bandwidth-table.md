# Apple Silicon のメモリ帯域はチップ名表でしか引けない (20260728 02:00)

## Issue

`proto/hostfit` の roofline に「この個体のメモリ帯域」が要るが、
Apple Silicon はこの値をどの API からも公開していない。sysctl にも
IOKit にも `system_profiler` にも帯域の項目は無い。実測で代替しようと
しても、UMA では CPU 側からしか測れず GPU が同じプールから引く量に
届かないため、得られるのは下限であって除外に使える上限にならない。

## Learnings

### チップ名は 2 経路から同じ文字列が取れる

実機 (Mac mini M4 / Mac16,10 / 16 GB) で確認:

```
$ sysctl -n machdep.cpu.brand_string
Apple M4
$ system_profiler SPHardwareDataType -json | jq -r '.SPHardwareDataType[0].chip_type'
Apple M4
```

`Profile.CPU.Model` は前者(空なら後者、それも空なら `hw.model` の
`Mac16,10`)。表のキーは `CPU.Model` にする。`HardwareGPUSummary.Model`
には "do not parse" 規約があるので使わない — ただしこの規約は
**consumer 側**への約束なので、producer が文字列から数値を引いて
`memory_bandwidth_spec_gbs` として publish するのは規約に反しない。
むしろ consumer に文字列を解析させないための正しい形。

### ビン違いは chip_type から区別できない

同じチップ名で 2 つのメモリ構成が出荷されている:

| チップ | 下位ビン | 上位ビン |
|---|---|---|
| M3 Max | 300 GB/s (14-core CPU) | 400 GB/s (16-core) |
| M4 Max | 410 GB/s (14-core CPU) | 546 GB/s (16-core) |

`chip_type` はどちらも "Apple M3 Max" / "Apple M4 Max" で、両者を分ける
情報が無い。**表には上側を入れる**。この数値は上限としてしか使わないので、
高く見積もると除外が減る(= 注記で済む)だけだが、低く見積もると上位ビンの
個体から動かせるモデルを取り上げてしまう。誤差の許容方向が非対称。

### 現行定数 120 は「母集団の下限」ではなかった

`BandwidthUnifiedGBs` のコメントは長らく「M シリーズ base が ~120 で、
それより大きい部品はすべてこれを上回る」と書いていたが誤り:

* M1 base = **68.25** GB/s
* M2 base = **100** GB/s
* M3 base = **100** GB/s

いずれも 120 未満。#265 が「どちら向きに間違っているか」をコメントに
明示した際もこの数値自体は検証されていなかった。#251 で表を作る過程で
判明し、テストの `smallestUnifiedPartGBs = 120.0` ごと訂正した。

### 除外は表の副作用として落ちてくる

`model_picker.go` の絞り込みは元から
`narrow(!UpperBound || MeetsSpeedFloor)` で、`UpperBound` が立った瞬間に
除外が始まる。つまり #251 に必要だったのは
`EstimateOllamaDecode` で `UpperBound = (spec 値がある)` を立てることだけで、
**picker 側の変更は 1 行も要らない**。逆に「表だけ入れて除外はしない」を
選ぶ場合は `UpperBound` を意図的に抑制する必要がある。

### 実カタログでの効果

| host | 変更前 | 変更後 |
|---|---|---|
| 24GB Mac (M4, 120) | `qwen3.6-27b` 7.4 tok/s | `gpt-oss-20b` **49.6 tok/s** |
| 24GB Mac (未知チップ) | `qwen3.6-27b` 7.4 tok/s | 変更なし(annotate-only) |
| 48GB M4 Max (546) | — | 35b-a3b、dense 27B も 33.5 tok/s で**残る** |

3 行目が表を作る理由そのもの。定数 120 のまま除外を許すと、27B を
実際には快適に動かせる M4 Max から取り上げてしまう。

実機 16GB M4 での確認: `memory_bandwidth_spec_gbs=120` が出て、
`RankModels` は 3 候補を残し `qwen3.5-4b` (35.3 tok/s) を選ぶ。
カタログが空になることはない(`narrow()` は空になる場合フォールスルー)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/251
- docs/decisions/20260728/0200-uma-bandwidth-spec-vs-measured.md
- internal/hardware/uma_bandwidth.go
- proto/hostfit/hostfit.go
