---
status: accepted
supersedes:
  - docs/decisions/20260820/0005-windows-apu-carve-out-is-not-additive.md
  - docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md
---

# Windows の Strix Halo の予算は、参照機で読み込めた最大の負荷で止める (20260920 18:00)

## Status

Accepted。waired-ai/waired-agent#1443 の計測（2026-09-20）に基づく。実装は同 issue の PR（`internal/hardware.windowsUMALoadableCapMB`）。

次の記録を**部分的に狭める（覆さない）**。記録の `## Status` に鏡の一文を置いた。

- `docs/decisions/20260820/0005-windows-apu-carve-out-is-not-additive.md` の決定 3（予算は 75 % ヒューリスティックではなく「RAM − OS 取り分」）はそのまま。予算はさらに、参照機で読み込めた最大の負荷でも止める（決定 1）。
- `docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md` の決定 3 が参照機のアクセラレータメモリの予算として挙げる `OllamaVRAMBudgetMB` の 98,304 MB は、81,920 MB になる。0340 の決定 3 は `docs/decisions/20260920/0300-catalog-admits-what-was-run-on-the-reference-host.md` が先に狭めており（参照機で実際に動かしたことを条件にする）、この記録はその予算の数値だけを改める。

## Context

### 参照機は、製品の予算の手前でモデルを読み込めなくなる

参照機（Strix Halo、128 GB のユニファイドメモリ、Windows、Vulkan）で、86 GB を超えるモデルが 1 回も読み込めなかった（waired-ai/waired-agent#1443。最初に見つけたのは waired-ai/waired#1427 の候補の計測）。製品が同じホストに与える予算は、0005 の決定 3 と 0340 の決定 3 のとおり 98,304 MB（96 GiB）で、測った上限より約 12 GB 上にあった。

2026-09-20 に境界を測り直した。計測の条件:

| 項目 | 値 |
|---|---|
| カーブアウト | 最小の 512 MB |
| エンジン | ollama 0.34.0（製品の pin と同じ版） |
| 製品のサービス | 停止。別ポート・別のモデルストアで動かした |
| 各回の前 | ホストが静かで、Windows の `Available` が約 119〜123 GB に戻るのを待った |
| モデル | unsloth の Qwen3.5-122B-A10B Q5_K_S（重み 86.39 GB）。出荷中の `qwen3.5-122b-a10b`（q4、81 GB、読める）と同じアーキテクチャで、重みだけ大きい |

KV キャッシュの型とコンテキストウィンドウを変えてデバイスメモリの総量を動かし、runner ログの `common_params_fit_impl: projected to use N MiB of device memory` の N で並べた。

| N (MiB) | 結果 |
|---:|---|
| 82,670 | 読めた |
| 83,389 | 読めた |
| 84,270 | 読めた |
| 85,070 | **落ちた** |
| 85,390 | **落ちた** |
| 87,173 | **落ちた**（DeepSeek-V4-Flash のビルド。waired-ai/waired#1427 の計測で同じ落ち方） |
| 88,270 | **落ちた** |

境界は N が 84,270 と 85,070 のあいだにある。KV キャッシュの型（f16、q8_0、q4_0 のどれも、境界の上で落ち、下で読めた）にも、コンテキストウィンドウにも、モデルのアーキテクチャにも、重みの大きさそのものにもよらない。

### 落ちるのは、メモリが実際に常駐しなければならない時点

- 落ち方はどれも同じ。llama-server が C++ 例外 `0xe06d7363` で終わり、runner ログの最後の行は `common_init_: warming up the model with an empty run`。ollama は HTTP 500 を返す。
- `GGML_VK_MEMORY_LOGGER=1` で回すと、**確保はすべて成功する**。デバイス側の合計は 85,074 MiB、ホスト側の合計は 914 MiB に達し、プロセスは最後の確保のあと、warm-up で死ぬ。確保が断られているのではなく、GPU が最初に走るときにメモリが常駐していなければならず、そこで落ちる。
- 配置は関係ない。`GGML_VK_PREFER_HOST_MEMORY=1` でも同じ時点で落ち、同じ日に環境変数を変えずに回した対照も落ちた。上限を上げるエンジンの環境変数は無い。
- エンジンは 2 つの Vulkan ヒープの予算の和（DEVICE_LOCAL 64.74 GiB + host-visible 32.37 GiB = 99,287 MiB free）に対して計画するので、エンジンの中に、実際の上限の手前で止まるものは無い。
- **和まで届かない理由は分かっていない。** 落ち方の再現と特徴付け（メモリが常駐しなければならない時点で落ちる）まではできたが、どのヒープのどの確保がそれを越えられないかは特定していない。上流の ggml-org/llama.cpp#16575 と PR #17110（Windows の iGPU で 64 GB を超えるヒープの報告の修正）は状況として近いが、この境界の原因と確かめたものではない。

### システムメモリは制約ではない

失敗した回を含めて、読み込み中の `Available` は 28.8 GB を下回らず、commit の最大は 105.6 GB（上限 138.4 GB = 物理 127 GB + ページファイル 8 GB）だった。Windows の System ログに Resource-Exhaustion（イベント 2004）は、#1443 の 2 回の固まりを含む 4 日間に 1 件も無い。ページファイルを増やすことは、この落ち方の対策にならない（オーナーからの問い）。

## Decision

1. **Windows の Strix Halo の予算は、参照機で読み込めた最大の負荷でも止める。** `internal/hardware.strixHaloUMA` が Windows で返す `UsableVRAMMB`（`hostfit.Host.OllamaVRAMBudgetMB` で読む）は、次の 3 つの最小値。
   - OS 可視 RAM − OS 取り分（0005 の決定 3。OS 取り分は `hostfit.Host.OSMemoryDeductionGB()`）
   - BIOS が割り当てられる上限 96 GiB（`strixHaloUMACapMB`）
   - 参照機で読み込めた最大の負荷 80 GiB = 81,920 MB（`windowsUMALoadableCapMB`）
2. **80 GiB にした理由。** 読めた最大の 84,270 MiB より約 3 GB 下で、デスクトップのセッションが持つ GPU メモリの分を空ける。計測は何も動いていないデスクトップで行った。
3. **定数であって、RAM の割合ではない。** 同じ Windows の分岐は逆の構成 — カーブアウトを大きく取り、OS に残る RAM が少ないホスト — にも使われる。waired-ai/waired-agent#863 は、その構成（OS 可視 31.65 GB）で 22.6 GB のモデルが動くことを測った（0005 の Context）。上限を RAM の割合で縮める規則は、この測って動いた構成を断る。定数の上限は、予算がここで測った値を超えるところでだけ効く。
4. **1 台の記録であって、プラットフォーム契約ではない。** RAM の少ない Windows の Strix Halo が何を読み込めるかは測っていない。コード・テストのコメントは、0005 の決定 5 と同じく「今日の挙動の記録」として書く。
5. **変えないもの。**
   - 容量ゲート（`hostfit.TotalMemoryMB`。参照機で約 128,000 MB）。
   - Linux の算術（0005 の決定 4）。
   - エンジンへの環境変数。上限を上げるものが無いので、渡さない。

## Consequences

- **参照機の予算は 98,304 MB から 81,920 MB に下がる。** 出荷中の ollama のビルドはすべて、入れる条件のガード（`internal/hardware/catalog_admission_test.go` の `TestBundledCatalog_EveryBuildFitsTheReferenceHost`）を通る。大きいものから:

  | ビルド | 判定に使う量 (MB) | 判定の節 |
  |---|---:|---|
  | `qwen3.5-122b-a10b` q4（manual_only） | 79,269 | 200,704 のコンテキストウィンドウ込みのデバイスメモリ |
  | `qwen3.8-flash-next` q2 | 77,109 | GGUF のレイアウトが無いビルドの、重みの常駐 |

- **実機の推奨と配信するコンテキストウィンドウも、同じ値で止まる。** `hostfit.OllamaRecommendModel` の判定と、ビルドをどのコンテキストウィンドウで配信するかは、BIOS の上限ではなく、読み込めた負荷の上限で決まる。
- **容量ゲートは変わらない。** 明示的に選んだモデルは、拒否ではなく警告して確認する経路のまま（`docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md` の決定 4「予測で除外しない」、waired#1067 の警告 + 明示確認）。
- **逆の構成のホストは変わらない。** カーブアウトが大きく OS 可視 RAM が少ない Windows のホストの予算は、これまでどおり「RAM − OS 取り分」で決まり、#863 の構成は測ったモデルを載せられる。
- **直っていないこと。** エンジンは今もヒープの予算の和に対して計画するので、カタログの外で選んだビルドは上限を越えて runner を落とし得る。同じ PR で、エンジンの起動が前のエンジンのプロセスツリーの終了を待つようにした。#1443 でホストが固まったのは、落ちた runner がメモリを握ったまま次のエンジンが読み込みを始めたためで、待ちはその経路を塞ぐ（`docs/knowledges/20260920/0600-windows-igpu-memory-ceiling-below-fit.md` §7）。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1443
- https://github.com/waired-ai/waired-agent/issues/863
- waired-ai/waired#1427
- docs/knowledges/20260920/0600-windows-igpu-memory-ceiling-below-fit.md
- docs/decisions/20260820/0005-windows-apu-carve-out-is-not-additive.md / docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md / docs/decisions/20260920/0300-catalog-admits-what-was-run-on-the-reference-host.md / docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md
- https://github.com/ggml-org/llama.cpp/issues/16575
- https://github.com/ggml-org/llama.cpp/pull/17110
- `internal/hardware/uma_common.go`（`windowsUMALoadableCapMB`、`strixHaloUMA`）、`internal/hardware/catalog_admission_test.go`
