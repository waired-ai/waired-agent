# ollama pin 移動の実測 — 0.33.3 (20260906 02:30)

## Issue

waired-agent#1193 (ollama pin 0.33.2 → 0.33.3 の検証と移動、
waired-ai/waired#1312 の L100) の実測記録。決定そのものは
`docs/decisions/20260906/0230-move-the-ollama-pin-to-0333.md`。

前回 (`docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md`) と
同じ理由で、作業の実体は測定である: この製品は upstream が約束して
いない挙動をエンジンから読み出しており、upstream がその 1 つを変えても
何もエラーにならず、製品の数値と判断が黙って偽になるだけ。

計測環境 (すべて 2026-09-06):

- pc-mbp14-m5 — macOS 26.6.2 / Apple M5 Pro / RAM 48 GB / arm64。
  ollama 0.33.3 をポート 11435、別 state dir で走らせた。この機体自身の
  waired agent (rc5) は止めずにそのまま動かしてある。
- Linux 脚 — 開発機 (WSL2、NVIDIA の dGPU 2 枚: RTX 5080 15.9 GiB +
  RTX 5070 Laptop 7.9 GiB、total_vram 23.8 GiB)。ollama 0.33.3 をポート
  11436、別 state dir で。**前回 Linux 脚に使った sv-mag ではない** —
  作業時間中ずっと別セッションが確保していた。
- sv-evox2 — Windows 11 build 26200 / Ryzen AI Max+ 395 (Strix Halo) /
  unified memory 127.15 GB。ollama 0.33.3 を `C:\l100` に手で展開。

モデルは特記なければ `qwen3.5:0.8b-q8_0`。

## Learnings

### 1. 0.33.3 のパッケージング — 3 OS とも変わっていない

- `go test -tags integration -run TestPinnedReleasePublishesEveryAssetChecksum ./internal/runtime/`
  が実物の v0.33.3 release に対して通る: `ollamaReleaseFor`
  (internal/runtime/ollama_release.go) の表の 6 アセットすべてが、
  release 自身の sha256sum.txt に載っている。
- アーカイブの**中身の配置**も `ExtractSub` の前提のまま。3 つとも展開して
  確認した: darwin `ollama-darwin.tgz` はフラットで `ollama` /
  `llama-server` / `llama-quantize` / `*.dylib` 一式に `mlx_metal_v3` /
  `mlx_metal_v4`。windows `ollama-windows-amd64.zip` は `ollama.exe` が
  アーカイブ**ルート**に `lib/` と並び、`lib/ollama/` には `cuda_v12` /
  `cuda_v13` / `vulkan` があって **rocm は無い** (`amdROCmSupported` が
  依って立つ前提)。linux `ollama-linux-amd64.tar.zst` は `bin/` + `lib/`。
  3 つの checksum は sha256sum.txt と手でも突き合わせた。
- **罠**: sha256sum.txt の行はアセットを `./<name>` と書いていて、
  `<name>` ではない。手書きの `grep ' <name>$'` は黙って 0 件になる。
  製品自身のパーサはこの形を扱える。

### 2. 製品が依存する挙動は macOS と Linux で完全に一致して持ちこたえた

- **先頭でない system ターンは受理される**: `/v1/chat/completions` と
  `/api/chat` の両方で HTTP 200、本文あり。waired-agent#1035 は直ったまま。
- **keep_alive の非対称はそのまま**。`/api/ps` で確認: `/v1/chat/completions`
  経由の keep_alive=37m は既定のまま (`expires_at` が now+5min)、
  `/api/chat` 経由の keep_alive=41m は `expires_at` を now+41min へ動かした。
  ResidencyEffect (waired-agent#908) はまさにこの非対称の上に建っている。
- **`/api/ps` は verify パスが読むキーを今も全部返す**: `context_length`
  / `details` / `digest` / `expires_at` / `model` / `name` / `size` /
  `size_vram`。
- **runner の argv には今も `-np` が乗る**ので `ObservedNumParallel` の
  読み戻し (#763) は成立する: 0.8b は `-c 32768 -np 1 ... -b 1024 -ub 1024`、
  `qwen3.8:27b-mtp-q4_K_M` は `-b 512 -ub 512`。生成バッチはエンジンが
  自分でサイジングし続けている (#1079 の前提)。

### 3. instruction ターンの畳みの一致 — 前回の結論を再確認

生の形 (エンジンが畳む) と畳み済みの形 (gateway が畳む) の
`prompt_eval_count`、`/api/chat`:

| 形 | 生 vs 畳み済み | 判定 |
|---|---|---|
| 先頭でない system ターン 1 つ | 19 vs 19 | 一致 |
| instruction ターン 2 つ | **28 vs 24** | **差あり** |

macOS と Linux で同一。0.33.2 の記録と同じ「一致 / 差あり」のパターンで、
一致するかどうかはエンジン版でも OS でもなくモデルのチャットテンプレート
の性質だという前回の結論を裏付ける。絶対値が前回と違うのはフィクスチャの
文面が違うため。

### 4. 新挙動「Report cached prompt tokens」— 製品に届く

0.33.3 はフラグ無しで、両面にプレフィックスキャッシュの内訳を返す:
OpenAI 互換の usage に `prompt_tokens_details.cached_tokens`、`/api/chat` に
`prompt_eval_cached_count`。定数ではなく実値:

- 610 トークンのプロンプトを 2 回送ると、`cached_tokens` は 1 回目 0、
  同一の 2 回目 606。`/api/chat` は `prompt_eval_count` 610 で
  `prompt_eval_cached_count` が 0 → 606。
- **3 回目に同じプロンプトへ文を追記して送ると、短いプレフィックスでは
  なく `cached_tokens` 0 が返った。** つまりこの field は再利用の**深さ**
  の尺度にはまだなっていない。#1125 / #1127 のようにここから推論する
  人には重要。

ツリーへの影響: internal/gateway/convert.go は #885 で vLLM 向けにこの
field を既にパースしていて、その doc は「ollama has no equivalent on any
surface: its OpenAI Usage carries prompt and completion counts only, and it
folds llama-server's cache_n into the prompt total before anyone sees it」
と書いていた。これが偽になったので、いつから偽かを書く形に狭めた。
コードは変えず、`OpenAIUsage.CachedPromptTokens` が ollama 経路でも実際の
再利用量を返し始める (internal/gateway/anthropic.go の `rr.setCachedInput`
へ流れる)。

### 5. 新挙動「Honor GGUF model defined default parameters」— 窓は動かない、既定がホスト依存になった

この製品が名乗る窓は 1 つも動かない。agent は常に `OLLAMA_CONTEXT_LENGTH`
を export するので、エンジン自身の既定が waired の窓を決めることは無い。

変わったのは既定の決まり方。エンジンは起動時に `vram-based default context`
とログし、見つけた VRAM から導く: default_num_ctx は 37.4 GiB の Mac で
32768、Linux 機 (total_vram 23.8 GiB) でも 32768、102.2 GiB の Strix Halo
で 262144。cmd/waired-agent/inference_ollama_tuning.go の
`ollamaContextFloor` は無関係 (agent 自身の要求を床で押さえる定数) だが、
その doc の「pin 版エンジン自身の既定」という文は範囲の一端を指す文に
なっていたので、こちらも狭めた。

### 6. Windows の ROCm overlay — allowlist・upstream docs・成果物の 3 者が食い違う

`ollama-windows-amd64-rocm.zip` は `lib/ollama/rocm_v7_1` に展開される —
ROCm **7.1**。`amdROCmSupportedRes` (internal/runtime/ollama_backend.go)
のコメントは「ROCm v6.1 overlay」と刻んでいた。zip の central directory
から読んだ rocBLAS カーネル (`Kernels.so-000-<target>.hsaco`) の対象は
gfx906 / gfx1030 / gfx1100 / gfx1101 / gfx1102 / gfx1150 / gfx1151 /
gfx1200 / gfx1201 — つまり Strix Halo (gfx1150/1151) と RDNA4
(gfx1200/1201) が Windows overlay に入っている。

一方 upstream 自身の Windows 対応表は成果物より**狭い** (RX 7900 / 7800 /
7700 / 7600 と PRO W7900…W7500 だけ)。したがって 3 つの集合が食い違う:
こちらの allowlist、upstream の docs、overlay の中身。**何も変えていない**
— 測定は waired-agent#1233 が持つ (理由は決定記録の 3)。

別件で、同じコメントの「!!! MAINTENANCE: keep in sync with
Test-AMDRocmSupported in scripts/install/ollama-windows.ps1」は #493 が
削除したファイルを指していた。指し先の「bump ごとの点検手順」は存在して
いなかったことになる。点検手順はリストの隣に置き直し、release notes では
なく overlay を読むよう書いた。

### 7. 踏んだ罠

- **Windows: ollama はモデル blob を sparse file で事前確保する。**
  ダウンロード途中の blob に `Measure-Object Length` を当てると、
  実際にはまだ落ちていないのに 78.87 GB のフルサイズを報告する。
  実際に格納されたバイト数は `compact /q` で見える。正直な数値は pull
  ログ自身のパーセンテージ。
- **メモリ不足のエンジンも HTTP 200 を返す。** Mac で
  `qwen3.8:27b-mtp-q4_K_M` を、機体自身の waired エンジン (37.4 GiB の
  Metal 予算のうち 13.5 GB を保持) の隣にロードすると、engine log には
  `kIOGPUCommandBufferCallbackErrorOutOfMemory` / `decode: Compute error`
  が出た — が HTTP 層は 200 でゼロ値の本文を返した:
  `{"model":"","created_at":"0001-01-01T00:00:00Z","message":{"role":"","content":""},"done":false}`。
  `/v1` 面は全カウント 0 の usage を返した。ステータスコードだけを見る
  チェックはここを黙って通過する。§3 の 27B 脚をこのホストで報告せず
  別ホストへ移したのはこのため。
- **ベンチキャッシュは EngineVersion でキーされる (#1131)** ので、この
  bump の後は全ホストが一度ミスして測り直す。旧エンジンの値なので、
  それが正しい結果であってコストではない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1193
- https://github.com/waired-ai/waired-agent/issues/1192
- https://github.com/waired-ai/waired-agent/issues/1233
- https://github.com/waired-ai/waired-agent/issues/1035
- https://github.com/waired-ai/waired-agent/issues/908
- https://github.com/waired-ai/waired-agent/issues/763
- https://github.com/waired-ai/waired-agent/issues/1079
- https://github.com/waired-ai/waired-agent/issues/885
- https://github.com/waired-ai/waired-agent/issues/1125
- https://github.com/waired-ai/waired-agent/issues/1127
- https://github.com/waired-ai/waired-agent/issues/1131
- https://github.com/waired-ai/waired-agent/issues/493
- https://github.com/waired-ai/waired/issues/1312
- https://github.com/ggml-org/llama.cpp/pull/27742
- docs/decisions/20260906/0230-move-the-ollama-pin-to-0333.md
- docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md
- docs/decisions/20260829/1600-move-both-engine-pins.md
