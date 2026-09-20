# NVIDIA のユニファイドメモリ機は 2 つの形で報告する — 数値を返す方が危険 (20260921 03:00)

## Issue

waired-agent#459 の Ask 2・3（GB10 を `ClassUnified` にし、予算と帯域を与える）
の実装中に、**「`nvidia-smi` が memory.total を答えないこと」を検出条件に
してはいけない**ことが上流の実機計測から分かった。この repo には GB10 も
N1X も無いので、根拠はすべて外部の一次情報と実機報告。

## Learnings

### 1. 同じ問題が 2 つの形で出る。2 つ目は「もっともらしい数値」

| 機械 | OS | `nvidia-smi --query-gpu=memory.total` | 実際のプール |
|---|---|---|---|
| DGX Spark (GB10) | Linux | **`[N/A]`** | 121.7 GiB |
| RTX Spark N1X | Windows on Arm | **`8128 MiB`**（カーブアウト） | 46477 MiB |

N1X は **5.7 倍の過小報告**で、しかも数値として通ってしまう。unsloth#11208
（MERGED、実機計測）の根本原因がそのまま警告になっている —
*"Every correction was gated on the CLI answering nothing. That holds on a
DGX Spark … and fails here … **A wrong number is not a missing one.**"*

**同じ形が AMD Strix Halo の Windows にもある**（カーブアウト 0.5 GiB 対
プール 96 GiB）。つまり「統合機では、GPU が報告する数値は予算ではない —
無いか、カーブアウトか」が 3 ベンダに共通する 1 つの事実。

### 2. `[N/A]` を条件にすると、統合でないものを拾う

`nvidia-smi` のセンチネルは `[N/A]` だけではない。この repo の
`nvidiaSMIUnavailable` は**括弧で囲まれた形**すべてを拾うので
`[Insufficient Permissions]` も含む。そのうえ:

- **MIG の親機**と一部の **vGPU ゲスト**も `memory.total` を答えない。
  unsloth の 47 ホスト行列がそれを明示的に分けている — *"the discrete cards
  whose capacity nvidia-smi also refuses to print (MIG parents, some vGPU
  guests) … are **correctly not flagged unified**"*。

→ **権限の問題がハードウェアのトポロジとして読まれる**経路になる。

### 3. 逆向きの「VRAM ≒ RAM なら 1 プール」も偽

nvitop#208 が 90 % 閾値で提案しているが（本人が *"has not been
independently validated"* と自認）、**32 GB の RAM に 32 GB の RTX 5090** を
挿した機械が満たす。24 GB + RTX 4090 も同じ。採れない。

### 4. NVIDIA の統合部品は compute capability でも PCI ID でも特定できない

- **cc `12.1` は GB10 と N1X の両方**（NVIDIA の CUDA GPUs ページは 12.1 に
  GB10 しか載せていないが、N1X の実機報告が 12.1 を示している）。
  プールは 128 GB 対 約 45 GiB で別物。
- **`pci.ids` に GB10 の GPU デバイス ID が無い。** あるのは PCIe ホスト
  ブリッジ `22ce` / `22d0` だけ。N1X は逆に ID が複数あるらしい
  （NVIDIA/NemoClaw#10076 が「単一 ID 決め打ちで弾かれる」と報告）。
- **`CPU.Model` は空。** aarch64 Linux の `/proc/cpuinfo` に `model name` 行が
  無く、`defaultCPU` はその行しか読まない。Grace / GB10 / Jetson が該当。

残るのは製品名だけ。**ただし `pci.ids` で `GB10` は `GB100`(B200) /
`GB102`(B100) / `GB10B`(Jetson Thor) の接頭辞**なので、完全一致でなければ
**B200 を 1 プールと誤認する**。

同じ立場（CUDA を呼べずシェルアウトする道具）の gpustat#179 が独立に
同じ形に到達していて、`\b(gb10|jetson|orin|xavier|tegra)\b` の word boundary で
回避し、`('NVIDIA A100-SXM4-40GB', NOT_SUPPORTED) -> unified False` を
テストに置いている。

### 5. Windows は表を引かずに N1X を捕まえる

`carvedFromSystemRAM`（#1462、sv-evox2 で実測検証）の片側不等式
`adapterBytes <= installed − visible` は、N1X でも成り立つ
（カーブアウト約 7.9 GiB ≤ 搭載 − 可視 約 9.8 GiB）。**名前を一切使わない。**
OS ごとに証拠が違い、問いは 1 つ、という #1462 の設計がそのまま効く。

### 6. Windows の DWORD 折り返しは、実在のカードでは踏めない

`readAdapterVRAMMB` は `qwMemorySize`(QWORD) が無いとき
`MemorySize`(DWORD) に落ち、その値は **4 GiB で折り返す**。折り返した
小さな残差が `carvedFromSystemRAM` を通れば**ディスクリート機を統合と
誤認する**（危険な向き）。報告のうちは無害だが、方針につなぐ以上は
確かめる必要があった。**実際の製品サイズで数えたところ踏めない**:

| カード | uint32 残差 | 結果 |
|---|---|---|
| 4 / 8 / 12 / 16 / 20 / 24 / 32 / 48 GiB | **0.00 GiB** | `readAdapterVRAMMB` が 0 を飛ばす |
| 6 / 10 GiB | 2.00 GiB | 不等式で落ちる |
| 11 GiB | 3.00 GiB | 不等式で落ちる |
| **4.5 GiB** | 0.50 GiB | **通る** — が、そんな製品は無い |

→ **ガードを足さなかった。** 出荷されているどのサイズも 0 に折り返す
（飛ばされる）か、数 GiB の残差を残す（不等式で落ちる）。

## Refs
- https://github.com/waired-ai/waired-agent/issues/459
- https://docs.nvidia.com/dgx/dgx-spark/known-issues.html （"iGPUs do not have dedicated framebuffer memory"）
- https://www.nvidia.com/en-us/products/workstations/dgx-spark/ （273 GB/s / 256-bit / "coherent unified system memory"）
- https://github.com/unslothai/unsloth/pull/11208 （N1X 実機: カーブアウト 8128 MiB 対プール 46477 MiB）
- https://github.com/unslothai/unsloth/pull/10703 （GB10 実機: `NVIDIA GB10, [N/A]`、MIG 親機を分ける行列）
- https://github.com/vllm-project/vllm/pull/57378 / https://github.com/Syllo/nvtop/pull/511 （NVML は NOT_SUPPORTED）
- https://github.com/wookayin/gpustat/pull/179 （word boundary + 名前の肯定を要求）
- https://github.com/XuehaiPan/nvitop/pull/208 （90 % 閾値の提案。未検証）
