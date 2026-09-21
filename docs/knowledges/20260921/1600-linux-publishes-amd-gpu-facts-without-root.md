# Linux は AMD GPU の事実を root 無しで公開している (20260921 16:00)

## Issue

waired-agent#1485 と #1484 の作業中に、AMD GPU について知りたい 4 つの事実
（列挙・内蔵かどうか・アーキテクチャ・GPU が使えるメモリ）を、rocm-smi も
root も無しに読めるかを調べた。プロファイラはそれまで Linux の AMD を
rocm-smi でしか見ておらず、ROCm SDK の無い大半のホストでは GPU が見えて
いなかった（docs/knowledges/20260805/1610-igpu-classification-three-layers.md §1）。

## Learnings

### 1. 4 つとも 0444 のファイルにある

Linux fleet の検証機（Ryzen 9 9950X の内蔵 GPU Granite Ridge 1002:13c0、
Linux 7.0）で 2026-09-21 に読んだ値:

| 事実 | ファイル | 実値 | モード（カーネルの宣言） |
|---|---|---|---|
| PCI の組 | `/sys/class/drm/card1/device/{vendor,device,revision}` | `0x1002` `0x13c0` `0xc1` | 0444 |
| カーブアウト | `.../device/mem_info_vram_total` | 2147483648 | `S_IRUGO`（amdgpu_vram_mgr.c） |
| GTT | `.../device/mem_info_gtt_total` | 64933629952（RAM の 50 %） | `S_IRUGO`（amdgpu_gtt_mgr.c） |
| GC の IP 版 | `.../device/ip_discovery/die/0/GC/0/{major,minor,revision}` | 10.3.6 | `__ATTR_RO`（amdgpu_discovery.c、5.18 以降） |
| ISA ターゲット | `/sys/class/kfd/kfd/topology/nodes/1/properties` の `gfx_target_version` | 100306（= gfx1036） | `KFD_SYSFS_FILE_MODE` = 0444（kfd_priv.h） |
| KFD のプール | `.../nodes/1/mem_banks/0/properties` の `size_in_bytes` | 64933629952 | 同上 |
| PCI アドレスとの対応 | 同じ `properties` の `location_id` / `domain` | 4096 / 0（= 0000:10:00.0） | 同上 |

`gfx_target_version` は major×10000 + minor×100 + stepping で、stepping は
16 進 1 桁で綴る（90012 は gfx90c、110501 は gfx1151）。

### 2. 「内蔵か」はカーネル自身が GC の版で決めている

amdgpu は IP discovery を持つ部品について、`AMD_IS_APU` を GC の版の列で立てる
（amdgpu_discovery.c、Linux 7.0 の `switch (amdgpu_ip_version(adev, GC_HWIP, 0))`）:
9.1.0 / 9.2.2 / 9.3.0 / 10.1.3 / 10.1.4 / 10.3.1 / 10.3.3 / 10.3.6 / 10.3.7 /
11.0.1 / 11.0.4 / 11.5.0〜11.5.4。v6.17 では 11.5.4 が無い。
`AMDGPU_IDS_FLAGS_FUSION`（`sudo waired init` が render ノードの ioctl で
読んでいたビット）は、この `AMD_IS_APU` をそのまま返している。
だから sysfs の GC の版をこの列に当てれば、同じ答えが root 無しで出る。

ただし答えられるのは「内蔵である」の側だけ。列に無い版は「このコピーが
古いだけかもしれない」ので、何も言わない。GC 9.4.3 はデータセンタの APU と
ディスクリート部品が共有しており、カーネルはパッケージ種別（SMUIO 13.0.3 /
13.0.11）で分けている。これは sysfs からは読めない。

### 3. GPU が使えるメモリは KFD が答える

KFD のメモリバンクは、ROCm スタックが実際に確保する大きさを示す。
**カーネル 6.15 から**、APU で GTT がカーブアウトより大きいときは計算用の
確保を GTT に置く（amdgpu_ttm.c の `apu_prefer_gtt`。v6.14 には無く v6.15 に
在るのを確認）。このとき KFD は GTT を報告する。検証機では
カーブアウト 2 GiB に対し KFD は 60.5 GiB（= GTT）だった。

AMD の Linux 向けの推奨構成は「BIOS のカーブアウトを小さく、GTT を大きく」で、
この構成ではカーブアウトは約 512 MB になり、容量についてほとんど何も言わない。
予算を KFD の値で取れば、カーネルのこの判断をそのまま使える。

### 4. 名前は libdrm の amdgpu.ids、ただし表示用だけ

`/usr/share/libdrm/amdgpu.ids` は「device, revision, 製品名」の表で、
Mesa も同じ表から名前を引く。ただし項目は後から直される（`150E,C4` は
890M から 880M に変わった）。デスクトップの Raphael / Granite Ridge の
revision は載っていない（検証機の 13C0 も無い）。だから名前の表示にだけ使い、
判断には使わない。

### 5. root が要るのは SMBIOS だけ

`/sys/firmware/dmi/tables/DMI` は 0400（`BIN_ATTR_SIMPLE_ADMIN_RO`）。
システム RAM の速度とバス幅を知りたいときはここを読むことになるが、
これだけは root が要る。render ノードの ioctl は Debian 系で 0660
root:render なので、デーモン（`User=waired`、補助グループ無し）からは開けない。

### 6. Windows には同じものが無い

HIP ランタイム無しに gfx ターゲットを返す API は無い。ただし PCI の組は
`MatchingDeviceId` で読める。device ID は 1 つのダイを指すので、
`pci.ids` のダイ名（744c は "Navi 31"）と LLVM の AMDGPU processor 表
（Navi 31 は gfx1100）を突き合わせた表で代用できる
（`internal/hardware/amd_pci_gfx.go`）。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1485
- https://github.com/waired-ai/waired-agent/issues/1484
- https://github.com/torvalds/linux/blob/v7.0/drivers/gpu/drm/amd/amdgpu/amdgpu_discovery.c
- https://github.com/torvalds/linux/blob/v7.0/drivers/gpu/drm/amd/amdkfd/kfd_device.c
- https://github.com/torvalds/linux/blob/v6.15/drivers/gpu/drm/amd/amdgpu/amdgpu_ttm.c
- internal/hardware/amd_sysfs.go, internal/hardware/amd_pci_gfx.go
