# `ollama ps` は CPU_Mapped の重みを数えない (20260912 21:30)

## Issue

waired-agent#1307 の追試中に、`ollama ps` が 73.44 GiB のモデルを
`size=54 GB / 100% GPU` と報告しているのを見つけた。差の 19 GiB 分は
どこにも出ていない。

ホストは Ryzen AI Max（Radeon 8060S、128 GB unified）/ Windows、
ollama 0.33.3 Vulkan、`qwen3.8-flash-next` の UD-Q2_K_XL。

## Learnings

### 1. `offloaded N/N layers to GPU` は「全部 device に載った」ではない

同じロードのエンジンログ:

```
print_info: file size = 73.44 GiB (3.57 BPW)   model params = 176.94 B
load_tensors: offloading output layer to GPU
load_tensors: offloading 47 repeating layers to GPU
load_tensors: offloaded 49/49 layers to GPU
load_tensors:      Vulkan0 model buffer size = 47322.20 MiB   (46.2 GiB)
load_tensors:  Vulkan_Host model buffer size =   416.80 MiB
load_tensors:   CPU_Mapped model buffer size = 27465.95 MiB   (26.8 GiB)
```

**49/49 と言いながら 36.4% が `CPU_Mapped`**、つまり GGUF の mmap のまま
CPU 側に残る。`ollama ps` の `size` / `size_vram` はこのうち device
buffer だけを数えるので、54 GB（= 46.2 GiB + KV ほか）としか言わない。

`docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md` §2 が
「`/api/ps` の `size` / `size_vram` は重みだけで、KV と生成 compute
buffer は出てこない」を記録している。本件はその逆側で、**重みのうち
device に載らない分も出てこない**。

### 2. 予算の問題ではない

同じロードで llama.cpp 自身がこう言っている:

```
common_params_fit_impl: projected to use 56538 MiB of device memory vs 99210 MiB of free device memory
common_params_fit_impl: will leave 42671 >= 1914 MiB of free device memory, no changes needed
```

**device の空きを 42.6 GiB 残したまま「変更不要」**。このアーキテクチャ
（`general.architecture = qwen4exp`、`ple.head_vocab_sizes` が 16 × 約
2,000 万語の per-layer embedding 表）では、その表は予算がいくらあっても
CPU 側に置かれる。`estimated_weight_gb` のような 1 つの数では、
「device に載る分」と「載らない分」を区別できない。

### 3. 影響範囲

`cmd/waired-agent/inference_ollama_verify.go` の residency / verify /
capacity はこの `size` を読む。実サイズで fit を直しても、その場で
「載った」を確かめる witness は device buffer 分しか見えない。

同じロードについてエージェントのログは
`model decision: … serves the ~200k coding window fully GPU-resident`
と書く。このホストでは誤りである。

### 4. 「常駐後もページが落ちる」は過渡だった

同じ issue が「2 回目の推論でも 80 MB/s の読みが続く」を記録している
が、モデルが 17 時間常駐したあとに同じ深さ（28k トークン）で測り直すと
ディスク読みは 12 MB / 11 MB、prefill は 281 tok/s で安定した。
`llama-server` の working set は 64.7 → 66.6 GiB まで上がり、そこで
止まる。起票時の 45.6 GB / 9.0 GB は、まだ触られていない `CPU_Mapped`
ページを触っていただけで、メモリ圧ではない。

### 5. 測るときの 2 つの罠

- **`keep_alive` の綴り**。`/api/generate` の body に `"-1"` を入れると
  `time: missing unit in duration "-1"` で 400 になる。`-1` は
  `OLLAMA_KEEP_ALIVE` の文法であってリクエストの文法ではない
  （waired-agent#927 が製品側で踏んだのと同じ穴）。`"-1s"` は通る。
- **ディスク読みはレートでなく累積で取る**。
  `\PhysicalDisk(_Total)\Disk Read Bytes/sec` はレートなので、1 秒でも
  取りこぼすとその分が消える。
  `Win32_PerfRawData_PerfDisk_PhysicalDisk` の `DiskReadBytesPerSec` は
  RAW では累積値なので、前後で引けばよい。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1307
- https://github.com/waired-ai/waired-agent/issues/1305
- docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md
- docs/knowledges/20260906/0430-rocm-runs-on-strix-halo-windows.md
