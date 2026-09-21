---
status: accepted
---

# 使う GPU は推論エンジンの既定に任せる (20260921 15:00)

## Status

Accepted。waired-agent#1484。オーナー裁定（2026-09-21）。

## Context

waired は ollama より積極的に内蔵 GPU（integrated GPU）を使っていた。
`ResolveOllamaBackend` が Radeon 780M / 880M / 890M などに
`OLLAMA_IGPU_ENABLE=1` を付けて Vulkan に載せ、Linux では GPU が見えないときに
CPU 名（`IsAMDMobileAPU`）から同じ判断をしていた（#40、#68）。Windows の
デスクトップ Ryzen の 2 CU の iGPU も、名前の正規表現をすり抜けて同じ扱いだった。

一方 ollama 0.34.2 自身は、内蔵 GPU を既定では**CUDA のものと ROCm の gfx1151
（Strix Halo）しか使わない**（`discover/runner.go` の
`integratedGPUAllowedByDefault`、`defaultIntegratedROCmGFXTargets`）。Vulkan で
見つかった内蔵 GPU は一律に捨てる。880M / 890M を許可リストに足す提案は、
試験機 2 台がどちらも動かず見送られている（ollama/ollama#16701）。
Intel の新しい iGPU には誤った出力を返す不具合が open のまま残っている
（ollama/ollama#13964、ggml-org/llama.cpp#28648）。

上流の実測（同じ機械での CPU 対 iGPU）は 3 つに割れる。大きい iGPU（8060S、
Arc B390 / 140T）は prefill も decode も速い。中くらい（780M、Vega）は
prefill だけ速い。小さい iGPU（24〜32 EU の UHD）は decode で CPU より遅い。
「iGPU があれば常に使う」は一般則としては成り立たない。

hostfit の側では、検出した iGPU が `Profile.GPUs` に居るとホストは
`ClassDiscrete` になり、カーブアウト（例: 512 MB）が VRAM として扱われる。
これは**モデルを除外できる唯一のクラス**で、エンジンが CPU で動いている機械に
それを当てていた（docs/knowledges/20260805/1610-igpu-classification-three-layers.md §4）。

## Decision

1. **GPU の選択は推論エンジンの既定に任せる。** waired が独自に上書きするのは、
   同じ機械で測って既定より良いと示せた場合だけ。今日それに当たるのは
   Windows の Strix Halo で Vulkan を指定している 1 か所（#1233: ROCm が黙って
   誤答する不具合 ollama/ollama#17895 / #17847、prefill +37.8 %・decode +12.3 %、
   エンジンが見せるプールが 99,437 MiB 対 78,197 MiB）。
2. **エンジンが既定で使わない内蔵 GPU は使わない。** 既に iGPU で動いていた
   780M などのホストも含む。どの内蔵 GPU を使うかは、ollama の許可規則の写し
   （`internal/hardware/engine_gpus.go`）で決める。使いたい運用者は
   `OLLAMA_IGPU_ENABLE=1` を agent の環境に書けばよい。waired のプランがこの
   キーを設定しないので、そのままエンジンに届く。
3. **ホストはエンジンが使う GPU だけで記述する。** 使わない GPU は
   `Profile.UnusedGPUs` に理由つきで移し、`Profile.GPUs` を読むもの
   （クラス、予算、ホストキー、バックエンドの選択、コントロールプレーンへ送る
   要約）は、はじめから使う GPU だけを見る。
4. Linux の Strix Halo にも waired 独自の規則は足さない。エンジンの既定どおり
   ROCm が優先されるなら、それでよい。

## Consequences

- Windows の 780M などのホストは Vulkan から CPU へ移る。クラスは
  `ClassDiscrete`（カーブアウトを VRAM とする）から CPU のみへ、ホストキーは
  `unified-amd-…` から `cpu-none` へ変わる。
- コントロールプレーンへ送る `signer.HardwareSummary.GPUs` からも外れるので、
  コンソールや NAVI の GPU 表示から消える。コントロールプレーンは同じ
  `proto/hostfit` でクラスを出すため、これは両側のクラスを揃えるのに必要。
- ベンチのキャッシュは GPU の型番を鍵に含むので、該当ホストでは 1 回
  測り直しが走る。
- iGPU を検出しても、それが使われない限り記述が悪くならない。Intel の検出器
  （#1483）と AMD の sysfs 読み（#1485）は、この上に安全に足せる。
- 許可規則の写しは、エンジンの版を上げるたびに読み直す
  （`internal/runtime/ollama_version.go` に記載）。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1484
- https://github.com/ollama/ollama/blob/v0.34.2/discover/runner.go
- https://github.com/ollama/ollama/pull/16701
- docs/knowledges/20260805/1610-igpu-classification-three-layers.md
- docs/knowledges/20260906/0430-rocm-runs-on-strix-halo-windows.md
