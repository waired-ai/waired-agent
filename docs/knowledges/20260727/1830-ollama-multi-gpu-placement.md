# ollama のマルチ GPU 配置ルール (20260727 18:30)

## Issue

`proto/hostfit` は「ollama は GPU をまたいで分散しない」という前提で
書かれており、`Host.VRAM0MB`（= `GPUs[0].VRAMTotalMB`）だけを予算にして
いた。2 枚挿しのホストが 1 枚分として判定される（#264）。

前提が正しいかどうかは upstream の実装を読むしかない。ollama の公開
ドキュメントは「必要なら複数 GPU に分割する」としか書いておらず、
「どの条件で」「どのデバイス同士が」プールされるかは書かれていない。
ここに書くのはピン留めしているバージョン（`OllamaPinnedVersion =
"0.31.1"`）のソースを読んだ結果。**エンジンを上げたら読み直すこと。**

## Learnings

### 配置の決定は `server/sched.go: selectLlamaServerPlacement`

1. `len(gpus) <= 1 || opts.NumGPU == 0` なら何もしない。
2. `groups := ml.ByLibrary(gpus)` — **バックエンドライブラリ名**
   (`"CUDA"` / `"ROCm"` / `"Metal"` / `"Vulkan"` / `"CPU"`) で厳密に
   グループ分けする。CUDA デバイスと ROCm デバイスが同じグループに
   入ることはない。→ **合算は最低でもベンダ単位でなければならない。**
3. `OLLAMA_SCHED_SPREAD` が未設定（既定）なら、まず
   `bestSingleGPUFit`: `predictedVRAM <= available*80/100` を満たす
   デバイスが 1 枚でもあれば、そのデバイス 1 枚に載せる。
4. 1 枚に載らないときだけ `bestGPUGroupByAvailableMemory` が
   グループごと選ぶ。`betterPlacementGroup` は
   「discrete を含むグループ」を優先し、次に合計空きメモリで比較する。
   選ばれたグループについて `availableMemoryForLoad` が
   `gpu.FreeMemory` を**合算**する。

つまり「1 枚に載るなら 1 枚」は*配置の好み*であって*容量の上限*では
ない。容量の上限はグループの合計。したがって fit 判定が見るべき予算は
合計側。

### グループ内の同質性は要求されない

`ml.ByLibrary` はライブラリ名だけで分ける。4090 と 3060 が両方 CUDA
なら同じグループに入り、そのままプールされる。

vLLM のテンソル並列（`router.VLLMVRAMBudgetMB` / #678）が
「同一モデル・同一 VRAM」を要求するのとは別物なので、そちらのルールを
そのまま持ってきてはいけない。vLLM は各テンソルを分割するので形が
揃っている必要があるが、ollama はレイヤ単位で分けるだけ。

### 統合 GPU (iGPU) の扱い — 既定で落ちるが CUDA は例外

`discover/runner.go: filterIntegratedGPUs` が
`DeviceInfo.Integrated` なデバイスを落とす:

```
dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1
```

ただし `integratedGPUAllowedByDefault` が先に効く:

| Library | 既定 |
| --- | --- |
| `"CUDA"` | **常に許可** |
| `"ROCm"` | `defaultIntegratedROCmGFXTargets` にある GFX ターゲットのみ |
| その他 (Vulkan など) | 落とす |

`OLLAMA_IGPU_ENABLE` は両方向に明示上書きする（`envconfig`）。

→ **NVIDIA に限れば integrated / discrete の区別は不要**。どちらでも
エンジンは同じように扱う。#264 の合算を NVIDIA 限定にしたのはこれが
理由で、`hardware.GPU` に `Integrated` を足さずに正しくいられる範囲を
選んだということ。AMD へ広げるときはこのフラグが検出された事実として
必要になる（モデル名からの推測ではなく）。

### 参考: 関係する環境変数

いずれもこのリポジトリでは設定していない（設定しているのは
`OLLAMA_IGPU_ENABLE` のみ、AMD iGPU / Intel のバックエンド計画で）。

- `OLLAMA_SCHED_SPREAD` — 常に全 GPU に分散する
- `OLLAMA_GPU_OVERHEAD` — GPU ごとに VRAM を予約（バイト）
- `CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES` / `ROCR_VISIBLE_DEVICES`
  / `GGML_VK_VISIBLE_DEVICES` / `GPU_DEVICE_ORDINAL`

### 残る不一致（承知のうえ）

- スケジューラが見るのは **free** メモリ、こちらが合算するのは
  **total**。1 枚のときからある差だが、枚数分だけ拡大する（#69）。
  しかも `bestGPUGroupByAvailableMemory` には `bestSingleGPUFit` の
  ような 80% の割引がない。
- ベンダ → ライブラリの対応は近似。ROCm が使えない AMD dGPU は
  upstream では別グループに落ちるが、こちらは同じベンダとして見る。
  NVIDIA 限定にしている間は問題にならない。

## Refs

- https://github.com/ollama/ollama/blob/v0.31.1/server/sched.go
- https://github.com/ollama/ollama/blob/v0.31.1/discover/runner.go
- https://github.com/ollama/ollama/blob/v0.31.1/ml/device.go
- https://github.com/waired-ai/waired-agent/issues/264
- `internal/runtime/ollama_version.go` — `OllamaPinnedVersion`
