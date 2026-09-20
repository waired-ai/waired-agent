# 参照機は llama.cpp の見積りよりずっと手前でデバイスメモリを使い切る (20260920 06:00)

## Issue

#1443: 参照機 (Strix Halo、統合メモリ 128 GB、Windows、Vulkan) で
86 GB を超えるモデルが 1 回も読み込めず、うち 2 回は OS ごと固まった。
見つけたのは waired-ai/waired#1427 の 1M 候補の計測で、最初の記録は
「81 GB までは読めて 86 GB は読めない」だった。

この記録は 2026-09-20 に参照機で境界を測り直した結果と、製品側で
変えたこと (PR、§7)、まだ分かっていないこと (§5) を残す。失敗する確保の
場所は特定していない。

計測条件: 製品のサービスは停止し、別ポート・別のモデルストアで
ollama 0.34.0 (製品の pin と同じ版) を動かした。flash attention は
on。各回の前に runner が 0 本で Windows の `Available` が約 118.9 GB に
戻るのを待った。

## Learnings

### 1. 境界は llama.cpp 自身の見積りで 84,270〜85,070 MiB

モデルは `unsloth/Qwen3.5-122B-A10B-GGUF` の Q5_K_S (重み 86.39 GB)。
出荷中の `qwen3.5-122b-a10b` (q4、81 GB、読める) と同じアーキテクチャ
で、重みだけ大きい。KV キャッシュの型とコンテキストウィンドウを変えて
デバイスメモリの総量を動かし、runner ログの
`common_params_fit_impl: projected to use N MiB of device memory` の N で
並べた。

| KV / コンテキストウィンドウ | N (MiB) | 結果 |
|---|---:|---|
| f16 / 32,768 | 82,670 | 読めた (54.5 秒) |
| q4_0 / 200,704 | 83,389 | 読めた (67.3 秒) |
| f16 / 98,304 | 84,270 | 読めた (56.8 秒) |
| f16 / 131,072 | 85,070 | **落ちた** (72.7 秒) |
| q8_0 / 262,144 | 85,390 | **落ちた** (69.7 秒) |
| DeepSeek-V4-Flash UD-IQ2_XXS、q4_0 / 200,704 (waired-ai/waired#1427 の計測) | 87,173 | **落ちた** |
| f16 / 262,144 | 88,270 | **落ちた** (74.0 秒) |

- 境界は N が 84,270 と 85,070 のあいだ。KV の型にも、モデルの
  アーキテクチャにも、重みの大きさそのものにもよらない。
- 7 回ともホストは固まらなかった (プロセスが落ちるだけ)。#1443 の
  2 回の固まりは別の状況で起きたもので、この計測では再現していない。
- 境界を測るときは、重みを変えるのではなく同じモデルで KV の型と
  コンテキストウィンドウを振るほうが速い。llama.cpp の見積りが 1 本の
  軸になり、ダウンロードが 1 回で済む。

### 2. 落ち方はどれも同じで、落ちるのは「確保した領域に触れる」段階

- llama-server が C++ 例外 `0xe06d7363` で終わる (WER には
  `KERNELBASE.dll` として記録される)。ollama は HTTP 500
  `llama-server process has terminated` を返す。
- runner ログの最後の行は
  `common_init_: warming up the model with an empty run`。
  waired-ai/waired#1427 の DeepSeek のときはテンソルを載せている途中
  だった。どちらも、メモリを要求した時点ではなく、実際に触れる時点。
- 上流には確保に失敗すると
  `ggml_vulkan: Memory allocation of size N failed.` を刷ってから投げ直す
  経路 (`ggml_vk_create_buffer_check`) があるが、この行は失敗したどの
  ログにも無い。つまり何も刷らずに投げる経路 (たとえば
  `ggml_vk_create_buffer` を直接呼ぶ `ggml_vk_host_malloc`) で死んで
  いる。どの経路かは §5。

### 3. ホストのメモリは尽きていない — ページファイルは答えではない

失敗した回を含めて、読み込み中のホスト側の数値:

| | 値 |
|---|---|
| `Available` の最小 | 28.8 GB |
| commit の最大 | 105.6 GB (上限 138.4 GB = 物理 127 GB + ページファイル 8 GB) |
| GPU の shared の最大 | 87.3 GB |
| System ログの 2004 (Resource-Exhaustion) | この 4 日間 (2 回の固まりを含む) に無し |

commit は上限に届かず、2004 も出ていないので、ページファイルを増やす
ことはこの落ち方の対策にならない (オーナーからの問い)。

### 4. 統合 GPU では llama.cpp が 2 つのヒープの予算を足して計画する

`vulkaninfo` で見たこのホストのヒープ:

| ヒープ | 大きさ | 予算 | 性質 |
|---|---:|---:|---|
| 0 | 34.07 GiB | 32.37 GiB | host-visible |
| 1 | 68.15 GiB | 64.74 GiB | DEVICE_LOCAL |

dxdiag は Shared Memory 104,159 MB、Dedicated 336 MB と報告する
(BIOS の carve-out は 512 MB、レジストリの `qwMemorySize` は
536870912)。

ggml-vulkan は統合 GPU では全ヒープの予算を足す。llama.cpp が
`99,287 MiB of free device memory` と報告してそれに対して計画するのは
このため。実際に使えたのは約 84 GiB まで。

上流の文脈: ggml-org/llama.cpp#16575 (「Vulkan llama.cpp > 64GB
Graphics card load bug」、同じ Ryzen AI Max + Radeon 8060S 級の Windows
機) は 2025-11-10 に閉じた。直前の PR #17110「vulkan: iGPU memory
reporting fix」(2025-11-09 merge) は、バッファを作るときに「ヒープが
割れているデバイス (BIOS が 64 GB を超える VRAM を割り当てた Windows
の iGPU)」を考慮するとしており、#17122 がその中のループの不具合を直して
いる。報告の 64 GB はこのホストの DEVICE_LOCAL の予算 64.74 GiB と
一致するので、今回の約 84 GiB は 2 つ目のヒープが使えるようになった
あとに残った上限に見える。

### 5. 失敗するのは確保ではなく、常駐させる段階

同じ日に、失敗する構成 (f16 / 131,072、見積り 85,070 MiB) を
`GGML_VK_MEMORY_LOGGER=1` で回した。

- **確保は 1 つも失敗していない。** 記録の最後まで確保は成功し、device
  側の合計は 85,074 MiB、host 側は 914 MiB に達する。そのあと、§2 の
  ウォームアップで落ちる。つまり「確保を断られる」問題ではなく、
  **確保した領域に実際に触れて常駐させる段階**の問題である。
- **置き場所を変えても動かない。** `GGML_VK_PREFER_HOST_MEMORY=1` を
  付けても同じ場所で落ちる (69.7 秒)。環境変数を付けない対照も同じ日に
  落ちている。**エンジンの環境変数で上限を上げる道は無い。**
- まだ分かっていないのは、**予算の合計 (99,287 MiB) まで届かない理由**
  である。§4 の読みは状況からの推定で、再現して確かめた原因ではない。
- 記録を採るときの注意: 確保の記録を出すと読み込みが 937 秒かかる
  (通常は 70 秒前後)。また、1 回ごとに別のプロセスで走らせ、前の回の
  プロセスを止めてから次を始めること。止めないと空きメモリが十数 GB
  戻らないまま次が始まり、条件がそろわない。

### 6. 訂正: この参照機は最初から mmap で読んでいない

#1443 の 2026-09-19 のコメントは「Windows では重みを mmap で読むので
二重に持つ」と推定した。実機のログで否定された (訂正は
waired-ai/waired#1427 側の計測から。同 issue のコメント 5742550698)。

- llama.cpp b10760 (`src/llama-model.cpp` の AUTO の分岐) は、デバイスが
  mmap 対応を報告しないと mmap を使わない。このホストの runner ログは
  毎回 `(load_mode = none)`。
- ollama v0.34.0 (`llm/llama_server.go` の `appendLoadModeArgs`) が
  `--load-mode` を渡すのは、Linux の統合 CUDA / ROCm (dio) か
  `use_mmap` が false のときだけ。
- したがって `use_mmap: false` も `LLAMA_ARG_LOAD_MODE=none` も、この
  ホストでは既定と同じ読み込みになる。waired-ai/waired#762 の修正の
  方向は、この carve-out の大きさでは作用する対象が無い。

### 7. 製品側で変えたこと (#1443 の PR) と、変えていないこと

固まりの再現は取れていないので、境界の診断とは独立に、固まりに至った
2 つの経路を塞いだ。

- **エンジンの起動は、前に退役したエンジンのプロセスツリーが消えるまで
  待つ。** 参照機では、kill された llama-server の runner が GPU 側の
  確保の途中でサーバより 8〜9 分長く生き、モデルのメモリを握ったままに
  なっていた。`Stop` と `killAndReap` が待つのはエンジン自身の子だけで
  `StopTimeout` までなので、どの再起動経路もその直後に次のエンジンを
  起こし、それがすぐモデルを読み込んでいた。今は 1 ホストに 1 つの
  記録 (`PendingExits`、ollama と vLLM の adapter で共有) を起動側が
  待つ。上限 `DefaultTreeExitTimeout` (15 分) は退役の時刻から数える。
  待ちは起動側の context だけに掛かるので、tray・管理 API・shutdown の
  `Stop` は #316 のまま `StopTimeout` で返る。
- **residency の warm-up は、同じ読み込みが続けて失敗するたびに間隔を
  倍にする** (1 分 → 最大 30 分、`residencyWarmRetryAfter`)。読み込めない
  大きさのモデルが 1 分ごとに読み直され、毎回ホストを同じメモリ圧の下に
  戻していた。1 回目の再試行は 1 分のままで、boot・reconcile・operator の
  start は回数によらず即座に温める。
- **予算を測った上限に合わせた。** この host class の予算は
  `min(OS 可視 RAM − OS 取り分, 96 GiB, 80 GiB)` になり、参照機では
  98,304 MB から 81,920 MB に下がる (`internal/hardware/uma_common.go` の
  `windowsUMALoadableCapMB`)。決定記録は
  `docs/decisions/20260920/1800-windows-budget-stops-at-a-measured-load.md`。
  出荷中のビルドはこの予算でも全部通る (`catalog_admission_test.go`)。
- **変えていない**: 容量のゲート (`hostfit.TotalMemoryMB`、参照機では約
  128,000 MB)。明示的に選んだものは、今までどおり拒否ではなく警告で扱う。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1443
- waired-ai/waired#1427 (1M 候補の計測。境界を最初に見つけた側、§6 の
  訂正の出所) / waired-ai/waired#762
- #316 (Stop の予算) / #837 / #863
- https://github.com/ggml-org/llama.cpp/issues/16575
- https://github.com/ggml-org/llama.cpp/pull/17110
- https://github.com/ggml-org/llama.cpp/pull/17122
- `internal/runtime/process_tree.go` (`PendingExits`、`treeAliveFrom`)、
  `internal/runtime/spawner_windows.go` / `spawner_unix.go` (`TreeAlive`)、
  `cmd/waired-agent/inference_warm.go` (`residencyWarmRetryAfter`)、
  `internal/hardware/uma_common.go` (`strixHaloUMACapMB`)
