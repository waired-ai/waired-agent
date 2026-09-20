# エンジン pin 移動の実測 — ollama 0.34.2 / uv 0.12.17、vLLM は 0.29.0 に据え置き (20260920 14:00)

## Issue

ollama `0.34.0` → `0.34.2`、uv `0.12.15` → `0.12.17` への pin 移動の実測記録 (waired-ai/waired-agent#1469)。vLLM は意図して `0.29.0` のまま動かしていない (§1)。

理由は前 2 回 (docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md、docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md) と同じで、作業の実体は測定である: この製品は upstream が約束していない挙動をエンジンから読み出しており、それが変わってもエラーにはならず、数値と判断が黙って偽になるだけだから。

ただし前回 (#1403、0.33.3 → 0.34.0) が静的確認に寄りかかれたのは、同梱の llama.cpp が b10760 のまま動かなかったからで、今回はそれが成り立たない。0.34.2 が同梱するのは **b10969** — 209 コミット、`src/` `common/` `tools/` の下で 177 ファイルが変わっている。だから静的確認 (§2) の上に 3 OS の読み戻し (§4)、パーサに実ログを通す確認 (§5)、同一機での b10760 対 b10969 の対照実験 (§6) を積んだ。

もう 1 つ、作業の途中で fleet にダウンロード禁止のオーナー指示 (2026-09-20) が入り、計測 1 件がいったん止まった。翌 2026-09-21 に解除され、最後の 1 件 — `ollama_version.go` が「次の bump で読み直せ」と書いていた QSA indexer — も片付いている (§7)。

計測環境 (すべて 2026-09-20)。ホスト名は書かない:

- 24 GB の CUDA dGPU の Linux 機。
- 128 GiB ユニファイドメモリの Vulkan iGPU の Windows 機 (Strix Halo)。
- 16 GiB の Metal の Mac。

## Learnings

### 1. vLLM を動かさない理由

upstream の vLLM で 0.29.0 より新しいタグは全部プレリリースである: v0.29.1rc0、v0.30.0rc1、v0.30.0rc2。PyPI の latest は今も 0.29.0。renovate.json はこの依存に `ignoreUnstable: true` を置いているので、これらに対する bump PR も上がらない。この事実は `vllm_pins.go` に日付付きで書いた — pin を読んで upstream に新しい rc を見つけた次の人が、見落としかどうかを再導出しなくて済むように。

uv は `VLLMPinSet` の要素ではない (タプルは `{vllm, transformers, python}`)。だから uv の pin だけを動かしても venv は 1 つも作り直されず、waired-agent#1431 はこの bump では発火しない。

### 2. 静的確認 — b10760 → b10969 で製品の経路は動いていない

upstream の v0.34.0 と v0.34.2、llama.cpp の b10760 と b10969 を diff して確認した。全部持ちこたえた:

- 4 つの (goos, goarch) 組のアセット名と sha256sum.txt は不変。sha256sum.txt の字面も同じ形のまま (空白 2 つ、`./` 前置)。
- `llm/llama_server.go` の runner argv のフラグ集合は**バイト一致** (抽出したフラグ集合を diff して差分ゼロ)。
- `api.ProcessModelResponse` は不変なので、`/api/ps` は同じ 8 キーを返し続ける。
- `server/sched.go` の 1 スロットで起動するモデルファミリの一覧は不変。行番号だけ動いた。
- `ParseLlamaPlacement` が依存する llama.cpp のログ行 3 つは b10969 に逐語で在る: `src/llama-model.cpp:1821` の「offloaded %d/%d layers to GPU」、`src/llama-kv-cache.cpp:301` の「size = ... K (%s) ... V (%s)」、`common/fit.cpp:321,350` の「projected to use」。
- `common/fit.cpp` の b10969 での変更は追加だけ — JSONL の「fit_memory_breakdown」ログが増えた。proto/hostfit の `OllamaFitTargetMB` が説明している余裕の論理は触られていない。
- 「ollama version is %s」は不変なので、internal/hardware の `ParseEngineVersion` は安全。
- waired-agent#1463 のメモリ OOM 分類器 (internal/runtime/load_memory.go) が依存する逐語のログ文字列 6 つは不変: 「warming up the model with an empty run」は両タグとも `common/common.cpp:1512`。「GGML_ASSERT(ctx->mem_buffer != NULL)」は `ggml/src/ggml.c:1643` から `:1644` へ動いたが式は同一。「ggml_vulkan: Memory allocation of size」は `ggml-vulkan.cpp:3647` から `:3803` へ動いたが文字列は同一。「load_tensors:」は両タグとも `src/llama-model.cpp` に在る。`std::bad_alloc` と `vk::OutOfDeviceMemoryError` は C++ 標準ライブラリと Vulkan-Hpp から来るので、版に依存しない。

### 3. 危なく見えて当たらなかった 0.34.1 / 0.34.2 の変更 3 つ

- **`typical_p` が HTTP 400 で拒否されるようになった** — リクエストでも `ollama create` でも (`server/routes.go:215`、`server/create.go:69`)。製品はこれを送らない: grep はゼロ件で、わざと載せたリクエストは 3 OS すべてで HTTP 400 を返した。後者があるので、これは「無い」という議論ではなく測定である。
- **0.34.2 は初回起動時のセットアップを足した**が、`runWelcome` (`cmd/welcome.go:20`) は stdin と stdout の**両方**が端末でない限り即座に nil を返し、配線されているのは `cmd/cmd.go:2399` の裸の `ollama` ルートコマンドだけ — serve、create、pull には無い。spawn されたエンジンは到達しない。
- **`/api/show` の形が変わった** (tensors は verbose のときだけ、型文字列が大文字化、verbose でないときに長い配列を平坦化しない)。この製品の Go は `/api/show` を呼ばない。唯一の呼び手は `capabilities` を読む dev スクリプトである。

### 4. 3 OS で採り直した読み戻し — 前回の macOS のカバレッジ欠落を埋めた

各 OS でエンジン起動 1 回、`qwen3.5:0.8b-q8_0`、`OLLAMA_CONTEXT_LENGTH=32768`。製品の agent は差し替えて**いない**: 0.34.2 のスクラッチコピーをポート 11435 で、専用のモデルストア付きで手動起動したので、fleet ホストの稼働中エンジンには触れていない。

| 確認項目 | 24 GB CUDA dGPU (Linux) | 128 GiB ユニファイド Vulkan iGPU (Windows) | 16 GiB Metal (macOS) |
|---|---|---|---|
| 先頭でない system ターン、`/v1/chat/completions` | 200 | 200 | 200 |
| 先頭でない system ターン、`/api/chat` | 200 | 200 | 200 |
| keep_alive=37m を `/v1` 経由 | 既定の約 5 分の失効のまま (12:44:43→12:49:43Z) | 同 (12:57:05→+5min) | 同 (12:52:59→+5min) |
| keep_alive=41m を `/api/chat` 経由 | `expires_at` が now+41m に動いた | 同 | 同 |
| `/api/ps` のキー数 | 8 | 8 | 8 |
| engine.log が logfmt で `msg="..."` を持つ | yes (86 行) | yes (85 行) | yes (65 行) |
| runner の argv | `-c 32768 -np 1 -b 512 -ub 512` | `-c 32768 -np 1 -b 1024 -ub 1024` | Vulkan と同じ |
| `typical_p` の負の対照 | 400 | 400 | 400 |
| vram-based default context | 23.5 GiB → 32768 | 102.2 GiB → 262144 | 11.8 GiB → 4096 |

先頭でない system ターンの 200 は #1035 が直ったままということ。keep_alive の非対称 (`/v1` は捨てる、`/api/chat` は効く) は ResidencyEffect (#908) が建っている土台そのもの。engine.log ではエンジン自身の行が今も `time=` 前置を持ち、`lastRunnerLine` はこれで runner の行と見分けている。runner の argv には今も `-np` が乗り、生成バッチはエンジンが自分でサイジングし続けている。

vram-based default context の 3 段を 1 回の作業で全部読んだのは今回が初めてで、いちばん下の段 (4096) を読んだのも初めてである。

`cached_tokens` は CUDA 機で再送を確認した: `/v1` の `prompt_tokens_details.cached_tokens` は 22 中 0 → 18、`/api/chat` の `prompt_eval_cached_count` は 18。両面が再利用を報告する。

**記録しておく罠**: Windows 機では `OLLAMA_IGPU_ENABLE=1` を設定するまでエンジンが iGPU をまるごと落とし、total_vram「0 B」・default_num_ctx 4096 を報告した — エンジンは「dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1」と記録する。製品はこのホストクラスで `OLLAMA_VULKAN=1` と `OLLAMA_IGPU_ENABLE=1` を一緒に設定しているので製品の経路は影響を受けないが、これを省いた手動起動のエンジンは、そう言わずに CPU だけの機械を測る。

### 5. `ParseLlamaPlacement` を実ログに通した

正規表現について推論する代わりに、本物の 0.34.2 の engine.log を持ち帰ってマージ済みの `ParseLlamaPlacement` に食わせた。全フィールドが埋まった:

```
ContextCells:32768 OffloadedLayers:14 TotalLayers:26 DeviceWeightsMiB:510.72 HostWeightsMiB:510.72 KVCacheType:f16 FitProjectedMiB:1239 FitFreeMiB:2125 FitShortMiB:355
ok=true  CPULayers=12
```

internal/runtime/llamacpp_placement.go の正規表現 8 つ全部が掛かる — ソースを読むだけの確認では未検証のまま残ったはずの 4 つ (`llamaLoaderRe`、`llamaModelBufRe`、`llamaNCtxRe`、`llamaFitReduceRe`) を含めて。

もう 1 つ記録しておく: fit の行は「cannot meet free memory target of 1241 MiB」で、1024 ではなかった。1241 は `OllamaFitTargetMB` (1024) に、projector を持つモデルに対して ollama の `llm/llama_server.go` が足す mmproj の分を加えた値である。llama.cpp の既定値が変わったのではない。

### 6. b10760 対 b10969 の対照実験 — この bump のいちばん重い結果

同じ機 (128 GiB ユニファイドメモリの Vulkan の Windows 機)、同じモデル (`qwen3.5:9b-q4_K_M`)、同じ構成、同じプロンプト (PRNG の seed も同じなのでトークン数も同一)、違うのはエンジンだけ。b10760 はそのホストのディスクに既に在った ollama 0.33.3 のバイナリ、b10969 は 0.34.2 のスクラッチコピー。両方の実行とも fit 時点で 99,386 MiB の空きデバイスメモリを読んでいたので、常駐プロセスの構成は比較可能だった。

runner の argv は両方とも `-c 262144 -np 1 --cache-type-k q8_0 --cache-type-v q8_0 --flash-attn on -b 2048 -ub 2048`。

prefill (深さごとに別のランダム語のプレフィックスを付けたので、行の間にプレフィックス再利用は無い):

| prompt_eval_count | b10760 tok/s | b10969 tok/s | 変化 |
|---|---|---|---|
| 6,202 | 471.7 | 588.6 | +24.8% |
| 12,340 | 506.4 | 621.0 | +22.6% |
| 24,640 | 488.1 | 562.2 | +15.2% |
| 46,139 | 439.4 | 541.5 | +23.2% |

デコード (120 トークン、短いプロンプト): 34.06 → 34.58 tok/s、つまり不動。

**メモリモデルは動いていない**。配置と fit の項は 2 つのエンジンの間で全部バイト一致:

- `common_params_fit_impl: projected to use 10527 MiB of device memory vs. 99386 MiB of free device memory` — 同一
- `load_tensors: offloaded 34/34 layers to GPU` — 同一
- `Vulkan0 model buffer size = 4717.38 MiB`、`Vulkan_Host model buffer size = 545.63 MiB` — 同一
- `llama_kv_cache: size = 4352.00 MiB (262144 cells, 8 layers, 1/1 seqs), K (q8_0): 2176.00 MiB, V (q8_0): 2176.00 MiB` — 同一
- `llama_context: flash_attn = enabled` — 両方

結論をそのまま書く: llama.cpp の 209 コミットは、このバックエンドで prefill のスループット 15〜25% を買い、hostfit が値付けするものは何一つ動かさなかった。この bump が見積もりもカタログの注記も proto の項も変えないのはこのためである。

### 7. QSA indexer — 前の bump が予約した宿題が片付いた

`ollama_version.go` は「AT THE NEXT BUMP」と明示した指示を持っていて、この bump がその bump だった: ggml-org/llama.cpp#28330 (コミット 311d4211b、b10889 で初出) は b10969 の祖先**である** — 実リポジトリに対する `git merge-base` で確認した。

実測 (2026-09-21、24 GB の CUDA dGPU の Linux 機、ollama 0.34.2、`num_ctx` 8192、KV は f16、この構成の 1 回目の起動、計測時の空きデバイスメモリ 23,666 MiB、load average 0.47、runner は `-c 8192 -np 1 -b 512 -ub 512 --flash-attn auto`)。重みは 49/49 層が GPU に載らず、CUDA0 に 20,408.09 MiB、`CPU_Mapped` に 47,373.43 + 27,465.95 MiB へ分かれた (fit は 47,900 MiB を見積もって 23,666 MiB の空きに対し 26,148 MiB 足りないと言う)。この項目は「どちらのキャッシュが確保されるか」を読むだけなので、こぼれた配置は結論に影響しない:

```
llama_kv_cache: size =  192.00 MiB ( 8192 cells,  12 layers,  1/1 seqs), K (f16):  96.00 MiB, V (f16):  96.00 MiB
llama_memory_recurrent: size =  112.57 MiB (    1 cells,  48 layers,  1 seqs  0 rs_seq), R (f32): 4.22 MiB, S (f32): 108.00 MiB, P (f32): 0.35 MiB
llama_kv_cache: size =   24.00 MiB ( 8192 cells,  12 layers,  1/1 seqs), K (f16):  24.00 MiB, V (f16):   0.00 MiB
```

**2 本目の V 半分は消えた** — `0.00 MiB`。同じブロックでエンジンは indexer のキャッシュについて `attn_rot_v = 0, n_embd_head_k_all = 0` とも書く (1 本目は `n_embd_head_k_all = 256`)。コンテキストウィンドウに依らない形に割り戻すと、attention が 24,576 B/token、indexer key が 3,072 B/token、合計 **27,648 B/token**。これは `proto/catalog/bundled/qwen3.8-flash-next.json` の `kv_bytes_per_token_fp16` そのもので、**注記は変えていない**。b10760 では indexer が K 3,072 に加えてモデルが射影を持たない V 6,144 を確保しており、合計は 33,792 だった。

ここで効いたのは、カタログが実測値ではなく**導出できる**数を刻んでいたことである (docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md §3)。エンジンが間違っていた側でも注記を変える必要が無く、エンジンが直った側でも変える必要が無い。同記録の §4 は役目を終えたので、この PR で済みにした。

なお、この計測はダウンロード禁止のオーナー指示 (2026-09-20) でいったん止まっていた。約 78 GB の重みは fleet のどのホストにも無く、pull は約 50 GB で中断した。中断した部分 blob は**再起動を越えて残らなかった** — ollama の `-partial` は全長を事前確保したスパースファイルなので `ls -la` は最初から 78 GB と表示するが、実割り当ては `du` でしか分からず、再起動後は 4.1 GB しか残っていなかった。残置を「使える資産」として数えるときは `ls` ではなく `du` の値を書くこと。

### 8. ついでに答えが出た / 出なかった相乗り

- **waired-ai/waired#805 (Strix Halo Windows/Vulkan の prefill が遅い)**: issue の仮説は「ollama が同梱する llama.cpp に Strix Halo 向けの Vulkan の高速経路が無く、より新しい同梱ビルドがてこになる」だった。§6 がその測定である: てこは効く — 測ったすべての深さで 15〜25%、まさにこのハードで、エンジンの bump だけで。もう 1 つ記録しておく: スループットは 6.2k から 46k トークンまで実質**平ら** (588→621→562→541 tok/s) で、減衰していない。issue 自身の 0.31.1 の表は減衰していた (4k 1282、8k 828、16k 668、30k 576) が、あれは別のエンジン**かつ**別のセッションの数字なので、荷重を持つ比較は同一機の b10760 対照であって、あの表ではない。
- **waired-ai/waired#664 (spill した CPU 計算が単スレッド、num_thread が無視される)**: 構造の半分しか採れなかった。0.34.2 で runner の argv に `--threads` は**無く**、リクエストに `options.num_thread=16` を置いても現れない — issue が 0.31.1 で報告したのと同じ所見。スループットとスレッド占有の数値は採った**が捨てた**: その実行は 32 スレッド機が load average 38.69 のときに走っており、実機計測に対するオーナーの前提「他に活発なプロセス無し」を満たさない。これは結果ではなく方法の記録として残す。
- **waired-ai/waired-agent#1266 / #1248 (Windows の ROCm 許可リスト)**: 0.34.2 で読み直して何も動いていない。overlay は今も `rocm_v7_1` で、その `rocblas/library/` はちょうど 9 つの `TensileLibrary_lazy_gfx*.dat` ターゲット — gfx906、gfx1030、gfx1100、gfx1101、gfx1102、gfx1150、gfx1151、gfx1200、gfx1201 — を持つ。0.34.2 の zip そのものから読んだ。これは `discover/amd.go` の `rocblasGFXTargets` が glob する集合で、つまり ROCm の可否を決める集合である。upstream の `docs/gpu.mdx` は v0.33.3 から v0.34.2 までバイト一致なので、#1248 の再確認の答えは「no」 (RX 9000 は upstream の Windows 列に移っていない) で、#1266 の食い違いも不変。

### 9. 記録の訂正 2 件

どちらも、この bump が読み直すことになっている stamp を読み直していて見つけた。

1. **darwin アーカイブのエントリ数**。`ollama_version.go` は darwin アーカイブが 59 エントリを列挙すると書いていた (前回の記録 docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md §1 も同じ)。実際は 57 で、0.33.3 でも 0.34.0 でも 57 だった — この 2 つのアーカイブのファイル一覧はバイト一致である。つまり 59 は数え間違いであって、何かが変わったのではない。0.34.0 → 0.34.2 の差は同梱の soname だけ: libggml-base と libggml が 0.22.0 → 0.24.0、libllama / libllama-common / libmtmd が 0.3.0 → 0.4.1。
2. **`ollama_backend.go` の保守 stamp**は、ggml-org/llama.cpp#27856 (HIP/gfx1151 での qwen4exp のデコード崩れ) が 0.33.3 時点で open で、Linux の ROCm の腕に当たると書いていた。修正は ggml-org/llama.cpp#27466 (長い行に対する radix TOP_K、コミット f8dbcd6、2026-08-31 merge) で、**b10720 で初めて出荷された** — つまり b10760 は既にそれを持っていて、0.33.3 は一度も曝されていなかった。古かったのは追跡であって、エンジンではない。issue 自体は 2026-09-07 に閉じた。別件として、ollama/ollama#17870 (Vulkan 側の対抗の重り) は 2026-09-07 に not_planned で閉じたので、あの回避策に upstream の修正は来ない。正しさのバグ 3 つ (#17895、#17847、#17498) は今も open なので、Strix Halo の Vulkan の腕はそのまま。

一般化すると: upstream の issue を名指しする stamp は**両方向**に古びる — 直っていないと書いたものが直っていることも、その逆もある。読み直すとは、段落の日付を付け直すことではなく、issue の状態を確かめることである。

### 10. 方法上の注意 — 踏んだ罠

- **製品 agent を差し替えず、スクラッチのエンジンを別ポート・別モデルストアで手動起動する形にした**ので、fleet ホストの稼働中エンジンに触れずに全部測れた。差し替えは他レーンのマージ済み変更も運ぶので、読み戻しだけが目的なら不要。
- **Windows では ssh のセッションが終わると `Start-Process` で起こしたエンジンが道連れで死ぬ。** 起動と計測は 1 回の ssh にまとめる必要がある。
- **Windows PowerShell 5.1 の `Set-Content -Encoding utf8` は BOM を付ける**ので、そのファイルを curl の `--data-binary` に渡すと JSON が壊れ、エンジンは黙って空の結果を返す。`[System.IO.File]::WriteAllText` に `UTF8Encoding($false)` を渡す。
- **スレッド占有をサンプリングするなら、生成がサンプリングの区間より長いことを先に確かめる。** 400 トークンの生成は 110 tok/s では 3.6 秒で終わり、6 秒の区間の大半がアイドルになって「どのスレッドも動いていない」と読める。
- **計測のたびに、その構成で何回目の起動か・計測時点の空きデバイスメモリ・runner の argv を併記する。** 今回の対照実験が意味を持つのは、両側とも fit が 99,386 MiB の空きを読んでいたからで、その数字が無ければ比較にならなかった。
- **load average を採ること。** 捨てた計測は load 38.69 で走っていた。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1469
- https://github.com/waired-ai/waired-agent/pull/1403
- https://github.com/waired-ai/waired-agent/pull/1148
- docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md
- docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md
- docs/decisions/20260829/1600-move-both-engine-pins.md
- docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md (§4 は未確認のまま生きている)
- https://github.com/waired-ai/waired/issues/805
- https://github.com/waired-ai/waired/issues/664
- https://github.com/waired-ai/waired-agent/issues/1266
- https://github.com/waired-ai/waired-agent/issues/1248
- https://github.com/waired-ai/waired-agent/issues/1463
- https://github.com/waired-ai/waired-agent/issues/1443
- https://github.com/waired-ai/waired-agent/issues/1431
- https://github.com/ggml-org/llama.cpp/pull/28330
- https://github.com/ggml-org/llama.cpp/pull/27466
- https://github.com/ggml-org/llama.cpp/issues/27856
- https://github.com/ollama/ollama/issues/17895
- https://github.com/ollama/ollama/issues/17847
- https://github.com/ollama/ollama/issues/17498
- https://github.com/ollama/ollama/issues/17870
- https://github.com/ollama/ollama/releases/tag/v0.34.2
- https://github.com/astral-sh/uv/releases/tag/0.12.17
