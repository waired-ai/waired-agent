---
status: accepted
supersedes:
  - docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md
---

# NVIDIA のユニファイドメモリ機は、部品名で名指して方針まで通す (20260921 03:00)

## Status

Accepted。waired-agent#459 の Ask 2・3。
`docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md`
の **§4 と §6 を部分的に**改める。

- **§4**（「今回は報告だけを事実にし、方針は動かさない。**方針の一般化は
  #459 に残る**」）は、その #459 の側で**解決した**。報告と方針を 2 枚に
  分ける設計そのものは残る — 変わるのは、方針が報告を読むようになったこと。
- **§6** の「**`GPU.Model` は使わない**」は、**NVIDIA のユニファイド部品に
  限って**改める。ほかのベンダ、および NVIDIA のディスクリート部品については
  不変。

§1・§2・§3・§5・§7 は不変。決定 `20260829/1100` §2 が退けた
「正規表現で識別子っぽさを推測する」ことの禁止にも触れない —
ここで引くのはベンダが公開した部品名の**完全一致**であって、推測ではない。

## Context

DGX Spark（GB10）を実コードに通すと 4 つの面が同時に壊れていた。実 `hostfit`
に 122 GB のプロファイルを通した結果（`hostfit` 側は 1 行も変えていない）:

| | 変更前 | 変更後 |
|---|---|---|
| `Class()` | `ClassDiscrete` | `ClassUnified` |
| `EffectiveVRAMMB` | **0** | 122880 |
| **出荷 variant の推奨** | **17 / 17** | 12 / 17 |
| decode 推定 | `0.0 tok/s` / 上限なし | `6.8 tok/s` / 上限あり |
| `MemoryBandwidthSpecGBs` | 0 | 273 |

**17 / 17 が核心**。`OllamaRecommend` のディスクリート枝は
`need > 0 && have > 0 && need > have` で、`have == 0` だと**制約が一切
掛からない**。入らないものを含めて全部「推奨」になっていた。

二重計上は GB10 では**潜在**にとどまる（`[N/A]` が残す 0 が偶然隠している）。
表に出るのは **RTX Spark N1X**（Windows on Arm、cc 12.1、ユニファイド）で、
そこでは `nvidia-smi` が 8128 MiB という**もっともらしい数値**を返す:

| | 変更前 | 変更後 |
|---|---|---|
| `TotalMemoryMB`（54 GB 機） | 59328（= RAM 51200 + カーブアウト 8128） | 51200 |
| `EffectiveVRAMMB` | 8128 | 51200 |

## Decision

### 1. 検出条件は「値が無いこと」ではなく、公開された部品名

当初は `memory.total == [N/A]` を条件にしようとした。**却下**。上流が実機で
両方向の反例を出している:

- **取り逃がす**: N1X は `[N/A]` ではなく**カーブアウトの数値**を返す。
  *"Every correction was gated on the CLI answering nothing … A wrong number
  is not a missing one."*（unsloth#11208、実機計測）
- **踏み込みすぎる**: MIG の親機や一部 vGPU ゲストも答えない。この repo の
  センチネル判定は `[Insufficient Permissions]` も含むので、**権限の問題が
  ハードウェアのトポロジとして読まれる**。

逆向きの「報告 VRAM ≒ RAM 総量なら 1 プール」（nvitop が 90 % で提案）も
**却下**: 32 GB の RAM に 32 GB の RTX 5090 を挿した機械が満たしてしまう。

### 2. 鍵は `ComputeCap` でも PCI ペアでもない。どちらも部品を特定しない

- **`ComputeCap`**: `12.1` は **GB10（128 GB / 273 GB/s）と N1X（約 45 GiB）の
  両方**。ここで引くと、#1455 が潰した「別の機械が同じ鍵を名乗る」欠陥
  （L4 と RTX PRO 4000 Blackwell、帯域 2.24 倍差）を NVIDIA 側で再生産する。
- **PCI ペア**: `pci.ids` に **GB10 の GPU デバイス ID が無い**。あるのは
  ホストブリッジ `22ce` / `22d0` だけ。
- **`CPU.Model`**: aarch64 Linux に `/proc/cpuinfo` の `model name` 行が無く、
  **空**。#251 が Apple と AMD に使った鍵は原理的に使えない。

残るのは NVIDIA 自身のライブラリが返す製品名（`--query-gpu=name` /
`nvmlDeviceGetName`）で、両 OS で同じ文字列。

### 3. 「do not parse」は消費者への規約。生産者は引いてよい

§6 の禁止は広すぎた。`uma_bandwidth.go` 自身がこう書いている —
*"The rule binds **CONSUMERS** of the published summary, and this is the
**producer side** turning a string into the number it then publishes."*
#251 が `CPU.Model` で帯域表を引くのを許したのと同じ carve-out。
この package が publish するのは `UnifiedMemory`（真偽値）なので、
**消費者は何も解析しない**。

外し方も安全側 — 改名されたら取りこぼして未知に倒れ、今日の挙動に戻る。
`ComputeCap` の外し方は逆（将来のディスクリート部品を誤って統合と呼ぶ）。

### 4. 完全一致。接頭辞・部分一致は実在のハードウェアで誤る

`pci.ids` で `GB10` は 3 つの別コードネームの接頭辞:
`GB100 [B200]` / `GB102 [B100]`（どちらも**ディスクリートの HBM 機**）/
`GB10B [Jetson AGX Thor]`。部分一致なら **B200 を 1 プールと誤認する**。
`appleUnifiedBandwidthGBs` が既に批准している規律と同じ。

### 5. 予算の規則は (OS, ベンダ) の表であって、機械の名簿ではない

`defaultUMA` の Linux 版と Windows 版は「ほぼ同じ関数の 2 コピー」で、既に
drift していた（Windows は CPU 文字列だけで立て、Linux は実 VRAM 読みを要求）。
3 ベンダ目を 2 コピーに足すのはその drift を増やすので、規則を untagged の
`unifiedBudgetFor(goos, prof)` に寄せた。行は**プールの縛られ方 1 つにつき
1 行**で、知らない機械はベンダの行を通って流れる:

| | 予算 | カーブアウト |
|---|---|---|
| NVIDIA（全 OS） | RAM − OS 取り分、**上限なし** | 0 |
| AMD / Windows | RAM − OS 取り分、BIOS の 96 GiB で clamp | 0 |
| AMD / Linux | カーブアウト読み値（加算的） | 同左 |

NVIDIA に上限が無いのは、これらの部品では CUDA がプール全体を
アドレスするため。**機種ごとの予算定数ではない** — オーナー裁定
`20260920/2100`（「予算を切り詰めるのではなくて、例外処理を充実させる」）が
退けたのはそちらで、ここで直しているのは予算の値ではなく**クラス**。

### 6. AMD は広げない

Linux の AMD APU は Strix Halo 以外も `integrated` を**報告**するように
なっているが、方針は家族名のまま。Linux の AMD の予算規則はカーブアウト
読み値で、参照機以外のどの APU でも測っていない。**測定の仕事であって
設計の仕事ではない。**

## Consequences

- GB10 の鍵は `unified-nvidia-gb10`。`unified-nvidia-sm121` ではない
  （§6 が当初書いていた綴り）。N1X が同じ `sm121` を名乗るため。
- 表に無いユニファイド部品は**検出されても帯域が 0** になり、`hostfit` は
  母集団定数へ落ちて annotate 専用のまま（#251 / #273 の規則どおり）。
  N1X が実際にこの状態で、Windows では**表を一切引かずに**
  `carvedFromSystemRAM` の算術が検出する。
- 参照機（Strix Halo）の答えは 1 バイトも動かない。Windows の 96 GiB、
  Linux のカーブアウト加算、`catalog_admission_test.go` の admission は不変。
- **実機が無い。** GB10 も N1X もこの repo には無く、根拠は上流の実機計測と
  ベンダの公開資料。だから未知は今日の挙動に倒す設計を崩していない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/459
- docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md
- docs/decisions/20260920/2100-memory-estimate-stays-failure-made-safe.md
- docs/decisions/20260820/0005-windows-apu-carve-out-is-not-additive.md
- docs/knowledges/20260921/0300-nvidia-unified-parts-two-shapes.md
