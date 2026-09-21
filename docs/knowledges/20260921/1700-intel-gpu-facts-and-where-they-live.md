# Intel GPU の事実はどこにあるか — 内蔵か単体かは誰でも、メモリ容量は root だけ (20260921 17:00)

## Issue

waired-agent#1483 で Intel の検出器を足すにあたり、AMD と同じ 4 つの事実
（列挙・内蔵かどうか・部品の名前・GPU のメモリ）がどこから読めるかを調べた。
Intel の単体 GPU を積んだ実機は fleet に無いので、以下はカーネルと
ドライバのソースを読んだ結果で、実機では確かめていない（計測 issue を別に起票）。

## Learnings

### 1. 内蔵か単体かは、カーネルの PCI ID 表で決まる

`include/drm/intel/pciids.h`（Linux v7.0）は、i915 と xe が認識する
device ID をすべてプラットフォームごとにまとめている。
`drivers/gpu/drm/xe/xe_pci.c` では `DGFX_FEATURES`（`.is_dgfx = 1`）を持つ
記述子が単体 GPU で、DG1、DG2（Alchemist と、データセンタ向けの ATS-M）、
BMG（Battlemage）、PVC、CRI が当たる。それ以外のプラットフォーム
（TGL、ADL、RPL、MTL、ARL、LNL、PTL、WCL、NVL-S など）はすべて内蔵。

Mesa も同じ区別を `intel_device_info.c` の `has_local_mem` で持っている
（DG1、DG2、BMG が真）。PCI ID は sysfs の `vendor` / `device` から
root 無しで読めるので、この表を写せば Linux でも Windows でも、同じカードを
同じように分類できる。

罠が 2 つある。**同じ device ID が別の製品を指す**:

- A770 の 16 GB 版と 8 GB 版は、どちらも `0x56A0`。
- Arc 140T（Core Ultra 9 285H、Xe コア 8 基）と Arc 130T（225H、7 基）は、
  どちらも `0x7D51`。

だから device ID で決めてよいのはプラットフォームまでで、帯域や容量は決められない。

### 2. 単体 GPU のメモリ容量は sysfs に無い

xe の sysfs にあるのは `vram_d3cold_threshold` だけで、i915 には何も無い。
メモリ容量は、各ドライバのクエリ ioctl でしか読めない:

| ドライバ | ioctl（v7.0） | 構造体 |
|---|---|---|
| xe（BMG と、LNL 以降の内蔵） | `DRM_IOCTL_XE_DEVICE_QUERY` = `0xC0286440`、`DRM_XE_DEVICE_QUERY_MEM_REGIONS`（1） | `drm_xe_query_mem_regions`: u32 個数 + u32 pad + `drm_xe_mem_region`（88 バイト、`mem_class` の VRAM = 1、`total_size` は +8） |
| i915（DG2 の既定） | `DRM_IOCTL_I915_QUERY` = `0xC0106479`、`DRM_I915_QUERY_MEMORY_REGIONS`（4） | `drm_i915_query_memory_regions`: u32 個数 + u32×3 + `drm_i915_memory_region_info`（**88 バイト**、`I915_MEMORY_CLASS_DEVICE` = 1、`probed_size` は +8） |

どちらも 2 回呼ぶ方式で、1 回目は大きさ 0 で呼んで必要な長さを受け取る。
i915 の region info は 4 + 4 + 8 + 8 + 64 バイトの union で **88 バイト**
になる（調査の途中の見積もりで 80 としていたものは誤り。ヘッダで確認した）。

どちらも `DRM_RENDER_ALLOW` だが、render ノードは Debian 系で 0660
root:render なので、デーモンからは開けない。`sudo waired init` が 1 度読み、
`gpu-topology.json` に `vram_total_mb` として残す。AMD の FUSION の読みと
同じ経路である。読めていない単体カードは「使わない GPU」の側に理由つきで置く。
エンジンは使うかもしれないので、表示は「CPU で動く」とは言わず、
「メモリ容量が分からない」と言う。

### 3. Windows はレジストリで足りる

表示アダプタのレジストリ（`HardwareInformation.qwMemorySize`）に、
単体カードのメモリ容量がある。内蔵 GPU の小さな専用分は、
`carvedFromSystemRAM` の算術が RAM から切り出したものだと見抜ける。

### 4. エンジン側は、内蔵をすべて捨てる

ollama 0.34.2 には SYCL も oneAPI も無く、Intel の GPU の経路は Vulkan
だけ。Vulkan の内蔵 GPU は既定ですべて捨てられる。単体カードは Vulkan で
使われる。この区別が #1483 の方針をそのまま決める
（docs/decisions/20260921/1500-gpu-choice-follows-the-engine-default.md）。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1483
- https://github.com/torvalds/linux/blob/v7.0/include/drm/intel/pciids.h
- https://github.com/torvalds/linux/blob/v7.0/drivers/gpu/drm/xe/xe_pci.c
- https://github.com/torvalds/linux/blob/v7.0/include/uapi/drm/xe_drm.h
- https://github.com/torvalds/linux/blob/v7.0/include/uapi/drm/i915_drm.h
- internal/hardware/intel_pci_parts.go, internal/hardware/gpu_intel.go, internal/hardware/gpu_intel_linux.go
