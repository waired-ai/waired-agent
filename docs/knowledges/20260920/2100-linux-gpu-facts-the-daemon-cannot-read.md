# Linux で「統合 GPU か」を言う事実と、製品が読めない理由 (20260920 21:00)

## Issue

waired-agent#459 / #1455 の実装中に、**world-readable な事実だけでは
Linux の AMD が統合かどうか判定できない**ことを実機で確かめた。上流の
実装（ollama / ROCr）が使っている根拠も、この repo の実機では偽になる。

## Learnings

### 1. 一級の事実は ioctl の向こうにあり、製品は開けない

`AMDGPU_IDS_FLAGS_FUSION`（`DRM_IOCTL_AMDGPU_INFO` の `ids_flags` ビット 0）が
答える。Mesa が `has_dedicated_vram = !(ids_flags & FUSION)` として読むのと
同じビット。

**ところが `/dev/dri/renderD*` は `0660 root:render` で、この repo の Linux 機では
`render` グループが空、ACL は `gdm-greeter` にしか権限が無い。** systemd の unit は
`User=waired` / `Group=waired` / `SupplementaryGroups=` 空なので、
**デーモンは開けない。**

```
一般ユーザ (euid=1000): open /dev/dri/renderD129: permission denied
root      (euid=0):    device_id=0x13c0 ids_flags=0x11  -> FUSION = true
```

**「カーネルが何を返すか」と「製品が何を読めるか」は別の主張**なので、root で
確かめた結果を「実機で検証済み」と書かない。

### 2. KFD の sysfs は答えない（3 つとも実測）

- **`cpu_cores_count` はこの実機の APU で 0**。ROCr が
  `HSA_AMD_MEMORY_PROPERTY_AGENT_IS_APU` を `NumCPUCores > 0` から立てている
  ので、**HSA 経由なら「APU ではない」と誤答する**。AMD 自身も
  `ROCm/rocm-systems#8190`（マージ済み）で gfx1151 の SPX モードについて同じ
  不安定さを認めている。
- **`local_mem_size` は全デバイスで 0 にハードコード**（`kfd_topology.c`、
  v5.10〜master で同一）。
- **`HSA_CAP_FLAGS_COHERENTHOSTACCESS` は MI300A と XGMI 機だけ**で、
  Strix Halo では 0。

AMD の OPEN issue `ROCm/rocm-systems#8476`（"APUs are not identifiable through
amdsmi"）が、この穴をそのまま認めている。

### 3. iGPU はディスクリートと同じ PCIe リンクを名乗る

```
/sys/class/drm/renderD129/device/max_link_width  = 16
/sys/class/drm/renderD129/device/max_link_speed  = 16.0 GT/s PCIe
```

リンクの有無・幅・速度は判定に使えない。

### 4. カーネルの PCI ID 表は、現代の部品を含まない

`amdgpu_drv.c` は `{ PCI_DEVICE(0x1002, 0x1305), .driver_data = CHIP_KAVERI|AMD_IS_APU }`
の形で **76 件**の表を持つ（ファイルは GPL ではなく **MIT**）。しかし
**`0x13c0` も `0x1586` もその表に無く**、末尾の
`{ PCI_DEVICE(0x1002, PCI_ANY_ID), .driver_data = CHIP_IP_DISCOVERY }` に落ちる。

現代の部品では `amdgpu_discovery.c` が実行時に立てる — GC の IP バージョン
（`IP_VERSION(11, 5, 1)` 等）か、`smuio.funcs->get_pkg_type(adev) == AMDGPU_PKG_TYPE_APU`
で**チップにパッケージ種別を訊く**か。つまりカーネル自身が「表を引く」から
「ハードウェアに訊く」へ移行している。**表を写しても答えは得られない。**

### 5. ollama の sysfs ヒューリスティックは参照機で壊れる

`discover/amd.go` の `vram <= 4 GiB && gtt >= 8 GiB && gtt >= 4*vram`。
この repo の iGPU（VRAM 2 GiB / GTT 64.9 GB）は正しく通るが、
**カーブアウト 96 GB に設定した Strix Halo は第 1 条件で落ちる**。
LM Studio が同じバグを抱えている（`lmstudio-ai/lms#589`、OPEN）。

### 6. aarch64 の Linux には `/proc/cpuinfo` の `model name` が無い

ARM には x86 の CPUID に当たる仕組みが無く、あるのは `CPU implementer` /
`CPU part` の生値だけ。上流の ARM64 メンテナは追加を繰り返し拒否している。
**`Profile.CPU.Model` は空になる** — Grace / GB10 / Jetson が該当する。
機械の素性が要るなら `/sys/class/dmi/id/product_name`（DGX Spark は
`"DGX Spark"`）が root 不要で読める唯一の口。**`/proc/device-tree/model` は
GB10 と Grace には存在しない**（ACPI/UEFI の SBSA で起動するため。DT が
あるのは Jetson だけ）。

## 結論

**Linux/AMD は ioctl しかない。** そして ioctl は特権が要る。だから
`sudo waired init` が 1 回読んで `runtime/gpu-topology.json` に永続化し、
デーモンは PCI ペアをキーに読み戻す。**`render` グループを常時与える案は
採らない** — 変わらない事実のために常時の特権を増やさないため。

## Refs

- https://github.com/waired-ai/waired-agent/issues/459
- https://github.com/waired-ai/waired-agent/issues/1455
- https://github.com/ROCm/rocm-systems/issues/8476
- https://github.com/ROCm/rocm-systems/pull/8190
- https://github.com/lmstudio-ai/lms/issues/589
- docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md
- internal/hardware/integrated_linux.go, internal/runtime/state/gpu_topology.go
