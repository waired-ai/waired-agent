# iGPU は誰がどう判定しているか — 3 層が食い違う (20260805 16:10)

## Issue

waired-agent#496 の足切り設計中に出た問い: 現代の PC も業務用サーバも純粋な
CPU 単体構成は稀で、大小あれど iGPU を積んでいる。**CPU-only 判定において
iGPU はどう扱われているのか、dGPU として数えられるのか。**

参照ホスト（このリポジトリの 24 GB 機）自体が該当する: NVIDIA RTX PRO 4000
Blackwell に加えて AMD の iGPU を積んでいる。

```
$ lspci -nn | grep -iE "vga|display"
01:00.0 VGA compatible controller: NVIDIA GB203GL [RTX PRO 4000 Blackwell] [10de:2c34]
10:00.0 VGA compatible controller: AMD/ATI Granite Ridge [Radeon Graphics] [1002:13c0]

$ cat /sys/class/drm/card1/device/mem_info_vram_total
2147483648          # iGPU に 2 GB の carve-out
```

しかし `scripts/dev/hwprobe` の出力は `gpus` が NVIDIA 1 件、`rocm: false`。
**iGPU はプロファイラから完全に不可視。**

## Learnings

判定は 3 層に分かれていて、層ごとに答えが違う。

### 1. プロファイラ (`internal/hardware`) — OS によって見え方が違う

- **Linux / darwin**: AMD の検出は **rocm-smi のシェルアウトのみ**。
  `gpu_amd_unix.go` の `amdWindowsFallback` は `nil` を返す固定スタブで、
  sysfs (`/sys/class/drm`) を読む経路は存在しない。ROCm 未導入なら iGPU は
  不可視。
- **Windows**: `gpu_windows_adapters.go` の `windowsDisplayAdapters` が
  レジストリのディスプレイアダプタを列挙し `qwMemorySize` を読むので、
  **iGPU は検出される**。

→ **同じ物理マシンが OS によって別のクラスに落ちる。**

### 2. `hostfit.Host.Class()` — 「integrated」という概念が無い

```go
switch {
case h.UnifiedMemory: return ClassUnified
case h.GPUCount > 0:  return ClassDiscrete
default:              return ClassCPUOnly
}
```

iGPU が検出されて `UnifiedMemory` が false なら、**dGPU と完全に同じ扱い**に
なり、carve-out（この箱なら 2 GB）が VRAM 予算として使われる。

`UnifiedMemory` は Linux では CPU 型番のヒューリスティックで、
`amdMobileiGPURe` は `\b\d{3}m\b`（780M 等）を要求する。デスクトップの素の
"Radeon Graphics" はマッチしないので、**デスクトップ iGPU は ClassUnified
ではなく ClassDiscrete に落ちる**。

そして向きが悪い。`EstimateOllamaDecode` の ClassDiscrete-spilled アームは
`BandwidthSystemRAMGBs/(1-share)` を返すので **60 GB/s より速い**値になり、
さらに `UpperBound = true`（除外可能）になる。真の ClassCPUOnly はちょうど
60 で annotate 専用。**iGPU を検出した方が、しない場合より速いホストに
見える。** これは #287（`hardware.GPU` に `integrated` フィールドが無い）の
未解決部分。

### 3. エンジン (ollama 0.31.1) — 既定で iGPU を捨てる

ピン版バイナリを直接読んだ:

```
$ strings ollama | grep -E "integrated|IGPU"
github.com/ollama/ollama/discover.integratedGPUAllowedByDefault
github.com/ollama/ollama/discover.integratedGPUAdmission
dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1
Enable integrated GPUs   /   OLLAMA_IGPU_ENABLE
json:"integrated,omitempty"   json:"integrated_known"   gfx_target
```

#287 が読んだソースによれば `integratedGPUAllowedByDefault` は CUDA を常に
許可、ROCm は `defaultIntegratedROCmGFXTargets` にある GFXTarget のみ、
それ以外（**Vulkan 含む**）は破棄。

**バイナリ全体で gfx 文字列は `gfx1151` ただ 1 つ**で、`rocblas` / `kfd_gfx` /
`library` という ROCm 文字列領域の中にある:

```
$ strings ollama | grep -oE "gfx[0-9]{4}" | sort -u
gfx1151
```

`gfx1151` = **Strix Halo**。つまり Strix Halo が既定で許可される唯一の統合
GPU で、他の iGPU（この箱の Granite Ridge を含む）はすべて捨てられる。

### 4. 帰結

| ケース | `hostfit` | エンジンの実際 | 一致 |
|---|---|---|---|
| dGPU + iGPU・Linux（この箱） | ClassDiscrete（NVIDIA 由来、iGPU は不可視で無影響） | CUDA で NVIDIA | ✓ |
| iGPU のみ・Linux・ROCm 無し | ClassCPUOnly | CPU | ✓（偶然） |
| iGPU のみ・**Windows** | **ClassDiscrete**（2 GB 予算） | **CPU**（捨てる） | **✗** |
| Strix Halo | ClassUnified（最大 96 GB プール） | iGPU を使用（gfx1151） | ✓ |

### 5. Strix Halo は両側で拾われるが、根拠が違う

- waired: `IsStrixHaloAPU` が CPU 型番に "ryzen ai max" を含むかを見る
  （`profiler_linux.go:68,81` / `profiler_windows.go:132,142` が
  `UnifiedMemory = true` を立て、`strixHaloUMA` がプールを計算）。Linux では
  iGPU が rocm-smi 無しに見えないため、CPU 文字列が唯一の信頼できる信号
  （#290）。
- ollama: GFXTarget が `gfx1151` か。

同じ結論に達しているが**別の根拠**なので、食い違う場面がある:

1. "Ryzen AI Max" ブランドの後継機で GFXTarget が変わった場合 → waired は
   ClassUnified として大きなプールを約束し、ollama は iGPU を捨てて CPU で
   走る。**推奨した大型モデルが CPU に落ちる**、最悪の組み合わせ。
2. gfx1151 だが "Ryzen AI Max" と名乗らない SKU（組込み / OEM）→ ollama は
   使うが waired は低く見積もる。

#287 の「推測ではなく事実を集めよ」（rocm-smi のクエリに `GFXTarget` を
足す）が、まさにこの欠落を指している。

## なぜこれが #496 の設計を決めたか

上の食い違いはどれも「分類は正しいのに実行系が違う」型で、**ハードウェア
分類をいくら精密にしても消えない**。#496 の足切りをプローブの実測ターン時間
で行う設計は、そのホストが実際に構成されたまま測るので:

- Strix Halo で iGPU が効いていれば速い数字が出て通過
- ケース 1（分類は Unified だが実際は CPU）なら遅い数字が出て足切り

と、分類器の正しさに依存せず両方正しく落ちる。詳細は
`docs/knowledges/20260805/1513-probe-predicts-decode-rate.md`。

## Refs

waired-agent#287（`integrated` を検出事実にする）、#290（Strix Halo の
CPU 文字列信号）、#68（AMD モバイル APU を Vulkan で使う）、#496、#466
