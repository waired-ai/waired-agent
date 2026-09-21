---
status: accepted
supersedes:
  - docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md
---

# AMD のディスクリート部品は ISA ターゲットで名指し、事実はカーネルから読む (20260921 16:00)

## Status

Accepted。waired-agent#1485。
`docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md`
の **§6 を AMD について部分的に**改める。§6 は「Apple と AMD は `CPU.Model` が
部品を名指す」と書いていた。これは AMD が APU であることを前提にした文で、
ディスクリートカードには当てはまらない。APU については不変。

## Context

`ChipSlug` の AMD の枝は、常に `CPU.Model` を読んでいた。そのため:

- `discrete-amd-ryzen-9-7950x-16-core-processor` — `discrete` は GPU についての
  主張なのに、名前は CPU を名指している。
- `discrete-amd-intel-r-core-tm-i9-14900k` — AMD の GPU の鍵に `intel` が入る。
- RX 7900 XTX（960 GB/s）と RX 9070 XT（640 GB/s）が同じ鍵になる。

#1455 が NVIDIA について取り除いた欠陥（L4 と RTX PRO 4000 Blackwell が
同じ名前）が、AMD 側に残っていた。

同時に、Linux の AMD は rocm-smi でしか見えておらず、ROCm SDK の無い大半の
ホストでは GPU そのものが見えていなかった。

## Decision

1. **AMD のディスクリート部品の chip は ISA ターゲット**（`gfx1100`、`gfx1201`）。
   NVIDIA の compute capability（`sm120`）に当たる、アーキテクチャの粒度の
   名前である。ROCm と ollama が部品を指すのにも同じ綴りを使っている。
   ダイより細かい区別（7900 XTX と 7900 XT は同じ `744c`・同じ `gfx1100`）は、
   §6 のとおり PCI の組が鍵の横に並んで担う。
2. **APU は引き続き `CPU.Model` で名指す**。参照機の鍵
   `unified-amd-ryzen-ai-max-395` は変わらない。gfx ターゲットで名指すのは、
   何かが「ディスクリートだ」と言ったときだけ — 既知のディスクリートの読み、
   または APU のものでない ISA ターゲット。何も分からないときは今日の綴りを保つ。
3. **Linux の事実はカーネルの sysfs から読む。** 列挙・カーブアウト・GTT・
   GC の版・ISA ターゲット・KFD のプールは、すべて 0444 のファイルにある
   （docs/knowledges/20260921/1600-linux-publishes-amd-gpu-facts-without-root.md）。
   「内蔵か」は、カーネルが `AMD_IS_APU` を立てる GC の版の列を写して答える。
   答えるのは内蔵の側だけで、列に無い版は何も言わない。rocm-smi は、sysfs が
   何も見つけないときの予備として残す。
4. **Windows の ISA ターゲットは PCI の組から引く。** `pci.ids` のダイ名と
   LLVM の processor 表を合わせた表（`internal/hardware/amd_pci_gfx.go`）を使う。
   表に無い device ID は空のままにする。
5. **Strix Halo の判定は ISA ターゲット（gfx1151）が先、CPU 名は予備。**
   予算・帯域・バックエンド・どの GPU を使うかが、エンジン自身の許可リストと
   同じ鍵で決まる。読みがあれば、使われていない GPU の読みであっても、
   CPU 名に上書きさせない。
6. **Linux の Strix Halo の予算は KFD のプール。** カーネル 6.15 以降、
   APU で GTT がカーブアウトより大きければ、計算用の確保は GTT に置かれ、
   KFD はそれを報告する。これを BIOS の UMA 上限で抑えた値を予算とする。
   加算するのはカーブアウトだけ。KFD が無いときは今までどおりカーブアウト。

## Consequences

- AMD のディスクリート機の鍵が CPU の名前でなくなる。既存の store に AMD の
  ディスクリート機の記録は無いので、綴り替える記録は無い。
- rocm-smi の無い Linux の AMD ホストで、GPU が見えるようになる。内蔵 GPU は
  決定 20260921/1500 のとおり、Strix Halo 以外は使わない側に入るので、
  ホストの記述は悪くならない（Linux の検証機で確認済み: iGPU は gfx1036 として
  見えて使わない側へ入り、鍵は `discrete-nvidia-sm120` のまま）。
- Linux の Strix Halo のうち rocm-smi が無かったホストは、これまで CPU の
  クラスだった。これからはユニファイドのクラスになり、予算を持つ。
  実機では測っていない（#868 と計測 issue）。
- 表が 3 つ増えた（GC の APU の列、GC→gfx の対応、PCI→gfx）。どれも
  新しいシリコンが出ると古くなる。見直しは #1486 に加える。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1485
- docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md
- docs/decisions/20260921/1500-gpu-choice-follows-the-engine-default.md
- docs/knowledges/20260921/1600-linux-publishes-amd-gpu-facts-without-root.md
