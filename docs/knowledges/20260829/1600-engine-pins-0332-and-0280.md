# エンジン pin 移動の実測 — ollama 0.33.2 / vLLM 0.28.0 (20260829 16:00)

## Issue

waired-agent#1132 (ollama pin の検証と移動) と waired-agent#1133 (vLLM pin
セットの検証と移動) の実測記録。waired-agent#1131 (ベンチキャッシュを
エンジン版でキーする) と同一 PR で着地する (両 pin issue の指示)。

pin 移動が 1 行の変更で済まない理由: この製品は、upstream が約束していない
挙動をエンジンから読み出している。upstream がその 1 つを変えても何も
エラーにならず、製品の数値と判断が黙って偽になるだけ。だから作業の実体は
測定であり、この記録は「何を測ったか」の記録である。

計測環境 (すべて 2026-08-29):

- sv-mag — Linux (Ubuntu) / NVIDIA RTX PRO 4000 Blackwell (VRAM 24467 MiB) /
  driver 610.43.02 / compute capability 12.0 / RAM 120 GB / 32 cores
- sv-evox2 — Windows
- sv-macmini — macOS 26.5.1 / Apple M4 / RAM 16 GB

vLLM は、製品の daemon が sv-mag で実際に走らせていた argv を**逐語で再生**
して検証した:

```
--host 127.0.0.1 --port <p> --model <local gpt-oss-20b path>
--gpu-memory-utilization 0.85 --no-enable-log-requests
--enable-prefix-caching --served-model-name openai/gpt-oss-20b
--max-model-len 124928 --kv-cache-dtype fp8
--enable-prompt-tokens-details --max-num-batched-tokens 4096
--enable-auto-tool-choice --tool-call-parser openai
```

ollama は `qwen3.8:27b-mtp-q4_K_M` (num_ctx 200704)、対照に
`qwen3.5:0.8b`。

## Learnings

### 1. ollama 0.33.2 のパッケージング — 3 OS とも変わっていない

- リリース自身の sha256sum.txt が `ollamaReleaseFor`
  (internal/runtime/ollama_release.go) の表の全アセットを覆っており、
  アセット**名**は不変。OS ごとにダウンロードして checksum を突き合わせた:
  linux `ollama-linux-amd64.tar.zst`、darwin `ollama-darwin.tgz`、
  windows `ollama-windows-amd64.zip` (1,460,134,793 bytes、sha256
  `2439cbea65310b1aadf7d8fc41d7faf5d033f920d42e00a476c58bf9bff6950e`)。
- アーカイブの**中身の配置**も `ExtractSub` の前提のまま。linux は
  `bin/` + `lib/`。darwin はフラット (`ollama`、`llama-server`、
  `*.dylib`) に `mlx_metal_v3/` と `mlx_metal_v4/` が付く形で、Mach-O は
  universal binary (x86_64 + arm64)。windows は `ollama.exe` がアーカイブ
  **ルート**に `lib/` と並ぶので、`BaseDir\bin` へ展開すると
  `BundledOllamaBinaryPath` の言う位置に exe が来る (展開して
  `ollama.exe --version` が 0.33.2 を返し、`lib\ollama` が隣に落ちる
  ところまで実行して確認)。
- 3 OS すべてで、展開した 0.33.2 のバイナリを実際に走らせた。

### 2. 製品が依存する 0.33.2 の挙動は全部持ちこたえた

- **先頭でない system ターンは受理される**: `/v1/chat/completions` と
  `/api/chat` の両方で HTTP 200。0.32.12/0.32.13 が持っていて
  0.32.14/0.32.15 が直した waired-agent#1035 の退行は、直ったまま。
- **keep_alive は OpenAI 互換面では捨てられ、native 面では効く**のも
  そのまま。`/api/ps` で確認: `/v1` 経由の keep_alive=37m は既定の失効
  (約 5 分後) のまま動かず、`/api/chat` 経由の keep_alive=41m は
  `expires_at` を約 36 分後へ動かした。ResidencyEffect (waired-agent#908)
  はまさにこの非対称の上に建っている。
- **runner の argv には今も `-np` が乗る**: llama-server の子は
  `-c 200704 -np 1 ... -b 512 -ub 512` で spawn された。つまり proclist の
  `ObservedNumParallel` 読み戻し (#763, #846) は成立し続け、生成バッチも
  エンジンが自分でサイジングし続けている — #1079 が強制バッチを廃止した
  前提のまま。
- **エンジン自身の既定コンテキスト窓は今も 32768**: num_ctx 無しでロード
  すると runner は `-c 32768` で spawn され、`/api/ps` は
  `context_length` 32768 を報告した。`ollamaContextFloor` が主張している
  のはこの値。
- **`/api/ps` は verify パスが読むキーを今も全部返す**: `context_length`
  / `details` / `digest` / `expires_at` / `model` / `name` / `size` /
  `size_vram`。engine.log も logfmt のままで `msg="..."` フィールドを持つ。

### 3. instruction ターンの畳みの一致は、エンジンではなくモデルのテンプレートの性質

gateway (internal/gateway/convert.go の `normalizeInstructionTurns`) は
すべての instruction ターンを `"\n\n"` で連結した先頭 1 system ターンに
畳む。convert.go のコメントは「これは修正済みエンジンが render するのと
同じプロンプトになる」と書いていた。生の形 (エンジンが畳む) と畳み済みの
形 (こちらが畳む) の `prompt_eval_count` を比べた:

| モデル | 形 | 生 vs 畳み済み | 判定 |
|---|---|---|---|
| qwen3.8-27b | 先頭でない system ターン 1 つ | 44 vs 44 | 一致 |
| qwen3.8-27b | instruction ターン 2 つ | 55 vs 55 | 一致 |
| qwen3.5:0.8b | 先頭でない system ターン 1 つ | 44 vs 44 | 一致 |
| qwen3.5:0.8b | instruction ターン 2 つ | **59 vs 55** | **4 トークン差** |

qwen3.5:0.8b の乖離は **Linux でも macOS でも同一**に出た。つまり OS でも
エンジンのビルドでもなく、**モデルのチャットテンプレート**の性質である。
これで壊れるものは無い — gateway はエンジンが生の形を見る前に正規化する
ので、メッシュの要求側と serving 側は同じ畳み済みの形を数える (#436)。
convert.go のコメントは、測定が支持する範囲まで狭めた。

**方法上の注意**: 最初の読みは macOS 上の qwen3.5:0.8b だけから来ていて、
エンジン版の退行に見えた。同じスクリプトを Linux のカタログモデルに、
次に Linux の小モデルに当てたことで、テンプレート単位の事実に変わった。
1 モデル × 1 OS の乖離を見たら、モデル軸と OS 軸を別々に振ってから
名前を付けること。

### 4. プレフィックス再利用の再実測 — 0.32.13 の 2 記録は 0.33.2 でも成立

再測定した理由: `docs/knowledges/20260819/2130-local-engine-caches-the-prefix.md`
と `docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md`
は 0.32.13 での実測で、0.33.0 は prefill の restore point を明示的に
作り替えている。waired-agent#1125 と #1127 はこの 2 記録の上で設計を
進めている。

sv-mag、`qwen3.8:27b-mtp-q4_K_M`、ctx 200704、約 70k トークンの
プロンプト。再利用は `prompt_eval_duration` から読む — ollama の
`prompt_eval_count` はヒットしてもしなくても全プロンプト分を報告するため:

| 手順 | prefill | wall |
|---|---|---|
| 1. A 冷 (70,019 トークン) | 153.71 s | 163.88 s |
| 2. A 再送 | 0.68 s | 1.03 s |
| 3. B 冷 (別会話) | 155.21 s | 156.72 s |
| 4. B の後に A | 155.12 s | 156.82 s |
| 5. 直後にもう一度 A | 0.67 s | 1.03 s |
| 6. A の**末尾**を編集 | 2.79 s | 3.15 s |
| 7. A の**先頭**を編集 | 154.95 s | 156.71 s |

結論はすべて 0.32.13 の記録から**不変**:

- 再送は完全ヒット (226 倍)。
- 温かいまま残る大きなプレフィックスは 1 本だけ — 2 本目の会話が
  1 本目を追い出す (手順 4)。
- 再利用を決めるのは分岐距離 — 末尾近くの編集は冷 153 s に対し 2.79 s、
  先頭の編集はフル再 prefill。

したがって #1125 / #1127 は上の 2 記録の上で推論を続けてよい。両記録は
**確認された**のであって置き換えられたのではない。日付入りの記録は凍結
するので、両ファイルは編集せず、0.33.2 での読みをこの記録として追加する。

### 5. vLLM 0.28.0 — フラグと tool parser

- `VLLMAdapter.commandArgs` が出し得る全フラグは今も受理される。エンジン
  自身の "non-default args" 行が全部を echo し、argparse は 1 つも拒否
  しなかった (未知フラグは argparse の exit 2 でエンジンごと失うので、
  これを最初に確認する)。なお 0.28.0 の `vllm serve --help` はグループの
  索引しか刷らない — フラグ全列挙は `--help=all`。
- このビルドが渡し得る 5 つの `--tool-call-parser` 名
  (`hermes` / `qwen3_xml` / `openai` / `glm45` / `deepseek_v4`) は今も
  `_TOOL_PARSERS_TO_REGISTER` (計 48 名) に登録されており、実 tool call
  は `finish_reason=tool_calls` の構造化 `tool_calls` 配列で返った。
  2 つのクラスは**名前を変えずに** upstream 内で移動している —
  `deepseek_v4` の解決先は `DeepSeekV4ToolParser` から
  `DeepSeekV4EngineToolParser` になり、`glm47` が `glm45` と同じクラスを
  指す形で加わった。cmd/waired-agent/inference_vllm_toolparser.go の表が
  Python クラス名でなく **CLI 名**を記録しているのはこのため。

### 6. KV 容量の行は正規表現に掛かり続けるが、数値そのものが動いた

`vllmKVCapacityRe` (cmd/waired-agent/inference_vllm_tuning.go) は今も
マッチする:

```
GPU KV cache size: 339,160 tokens, Maximum concurrency for 124,928 tokens per request: 2.71x
```

行の出所は kv_cache_utils.py:2146 から :1869 へ動いたが、正規表現は位置を
見ていない。動いたのは**数値**のほう: 同じ argv・同じカードで 0.24.0 は
393,709 トークン、0.28.0 は 339,160 トークン — **14% 小さい**。
waired-agent#1126 は vLLM 側の設計をこの行の上に建てるので、この数値は
エンジン版に依存する値として扱う必要がある。

### 7. スケジューラ既定・fp8・speculative・pin タプル

- 70 GiB 未満のカードに対するエンジン既定は不変:
  `max_num_batched_tokens` 2048 / `max_num_seqs` 256。**インストール済みの
  arg_utils.py から読んだ値**で、散文からではない — 後発の upstream V1
  ドキュメントは 1024 と主張しており、この製品が serve する全カードで
  誤っている。upstream には**第 3 段**が生えた: >= 160 GiB (B200/B300 級)
  は既定 16384 batched / 1024 seqs で、`vllmMaxNumBatchedTokens` が
  大型 GPU に選ぶ 8192 より上。カタログのカードはまだそこに無いので、
  8192 を渡して chunk が下がるのは製品が serve していないハードだけ。
- `kv_offloading_backend` の既定は今も native で、lmcache ではない。
- `--kv-cache-dtype fp8` は compute capability 12.0 で受理され、metrics
  エンドポイントは `cache_dtype="fp8"` を報告した。
- `--speculative-config` の ngram JSON は受理され、エンジンは
  `SpeculativeConfig(method='ngram', ...)` を報告した。副作用 2 つ:
  "Async scheduling not supported with ngram-based speculative decoding
  and will be disabled" と警告し、KV プールが 339,160 → 234,334 トークン
  (concurrency 1.88x) に落ちる。
- `VLLMVerifyImports` は新 venv で通る: compute capability (12, 0)、
  vllm 0.28.0、huggingface_hub の console script あり。
- pin タプルの解決結果: vllm 0.28.0 / torch 2.13.0+cu130 /
  transformers 5.16.1 (つまり `<6.0` の cap は上端で生きていて、床に
  座っているのではない) / python 3.12.13 / hf_transfer 0.1.9 /
  huggingface_hub 1.29.0 / ninja 1.13.0 / torchvision 0.28.0 /
  torchaudio 2.11.0 / triton 3.7.1。0.28.0 の requires_python は
  `<3.15,>=3.10` で、`VLLMPythonVersion` の 3.12 は中に居る。
- 0.28.0 は出力から `reasoning_content` を**削除した** (upstream 自身が
  breaking client change と呼ぶ)。応答メッセージは `reasoning` を持ち、
  `reasoning_content` キーは一切無い。ここでコストがゼロなのは、
  convert.go の `reasoningText()` が既に `reasoning` を優先して
  `reasoning_content` へフォールバックしていたからに過ぎない。DeepSeek と
  一部の llama.cpp ビルドは旧キーのままなので、struct には両方残る。

### 8. コードを変えた発見 — 0.28.0 は PATH に nvcc が無いホストで起動しない

vLLM 0.24.0 は `flashinfer-python==0.6.12` と `flashinfer-cubin==0.6.12`
の**両方**を宣言していた。0.28.0 は `flashinfer-python==0.6.16.post3`
だけを宣言する。vLLM の `has_flashinfer()` (vllm/utils/flashinfer.py) が
true を返すのは、flashinfer が import できて、**かつ** (flashinfer_cubin
が入っている **or** `shutil.which("nvcc")` が非 nil) のとき。つまり cubin
依存を落としたことで、**PATH 上の nvcc が荷重を持つようになった**。

sv-mag では nvcc は /usr/local/cuda/bin/nvcc に在るが、ユーザーの PATH
にも root の PATH にも入っていない — Ubuntu の既定がそうである — ので、
エンジンは `_initialize_kv_caches` の途中で死んだ:

```
RuntimeError: FlashInfer backend is not available. Please install the package to enable FlashInfer kernels
```

子プロセスの PATH に /usr/local/cuda/bin を足すと、同一 argv が起動して
ready まで到達した。cubin を pin し直す道は**無い**: PyPI の
flashinfer-cubin の最新は 0.6.13 で、0.28.0 の要求は flashinfer-python
0.6.16.post3 — 組になるビルドが存在しない。よって修正は製品側:
`VLLMAdapter.processEnv` (internal/runtime/vllm.go) は、`ninja` のために
既に前置していた venv の bin に加えて、ホストの CUDA bin ディレクトリ
(`detectHostToolchain` から取る — `$CUDA_HOME` / `$CUDA_PATH` → PATH →
/usr/local/cuda の順で、flashinfer 自身の探索順を意図的に写している) を
前置する。これが無いと、CUDA は入っているが PATH に無い全ホストが次の
update でローカル推論を失う。

はっきり書いておく: これは #1133 の冒頭が CLI フラグについて警告している
のと同じ失敗クラス — 縮退せずに死ぬエンジン — に、フラグではなく
**依存関係**経由で到達したもの。

### 9. KV オフロードは保持を買う — 小さくない (#1133 の追加測定 1)

sv-mag、gpt-oss-20b。再利用は `usage.prompt_tokens_details.cached_tokens`
から読む。

- **オフロード無し**: 約 120k トークンの会話 3 本を 339,160 トークンの
  プールに流すと、あふれた後の LRU 会話は再送で完全に冷えて返る
  (120,067 中 64 トークンのみ cached、29.80 s)。
- **`--kv-offloading-size 8 --kv-offloading-backend native`** (プールは
  285,883 トークンに減る): 同じあふれ方でも、先行 2 会話は再送で**両方
  とも完全に温かい** — 120,066 中 120,064 cached で 0.18 s、120,064 中
  120,063 cached で 0.14 s。
- 対価: **数値としては確定できていない**。下の追記を読むこと。

「ホスト RAM を使ってプレフィックスを温存する」ことの**正の実測**は
このリポジトリで初。唯一の先行測定 (waired-agent#866 / PR#883、ollama 側)
は null result だった。**保持そのものの結論はプールの絶対値に依存しない**
— 対照は「あふれた後の再送が冷えるか温かいか」であって、プールが何トークン
だったかではない。

## 追記 (20260829 17:00) — KV プールの数値は起動時の状況でも動く

マージ後の実機確認 (#1148 を入れた sv-mag) で、**同じ argv・同じカード・
同じエンジン版なのにプールの値が食い違った**。切り分けた結果:

| 条件 | GPU KV cache size |
|---|---|
| 0.24.0 / 本番 argv / GPU クリーン | 393,709 |
| 0.28.0 / 本番 argv / GPU クリーン | **339,160** |
| 0.28.0 / 旧エンジンの VRAM 解放と重なった起動 | 285,883 |

339,160 は**隔離した scratch 実行と製品デーモンの両方で独立に出た**ので、
版差 **-14%** (393,709 → 339,160) は clean 同士の比較として確か。

**訂正が要るのは上の §オフロードの「対価 -15.7%」のほう。** そこで引いた
285,883 は、この表の「重なった起動」の値と *Maximum concurrency の表示まで
含めて完全に一致する*。別々の原因が同じ値を出すとは考えにくく、オフロード
の代償として測ったつもりの差分が、実際には起動時の残留 VRAM を測っていた
可能性を否定できない。**あの -15.7% を代償の数値として使わないこと。**

読み取れる一般則のほうが有用: **`GPU KV cache size` は (カード, argv,
エンジン版) の関数ではなく、プロファイル時点の空き VRAM の関数**である。
vLLM は重みをロードした後に残りを測ってプールを決めるので、直前のプロセス
が解放しきっていなければ小さく出る。waired-agent#1126 がこの 1 行を読む
なら、**起動ごとに測り直される値**として扱う必要がある — 前の起動の値を
そのまま持ち越してはいけない。#1131 がベンチキャッシュにエンジン版を
足したのと同じ理由が、ここでは版よりさらに細かい粒度で効く。

### 10. ブロック単位の LRU は #1126 の想定どおりに振る舞う (#1133 の追加測定 2)

小さめのあふれ (会話 3 本で計約 360k トークン vs プール 339,160、超過
約 21k) では、LRU の会話だけが追い出され、直近の会話は丸ごと生き残った:
D の再送は 100% cached で 0.09 s、E の再送は完全に冷えて 29.80 s。

**方法上の注意**: 最初の実験は「両方の会話が失われた」ように見え、LRU
ではなくフルフラッシュに読めた。そうではなかった。追い出された会話を
再投入すること自体が admission であり、それが次の eviction を強制する
ので、観測列は最初から LRU と整合していた。2 つの読みを分けたのは、
超過を小さくした対照実験である。実験の形そのものへの注意として残す。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1132
- https://github.com/waired-ai/waired-agent/issues/1133
- https://github.com/waired-ai/waired-agent/issues/1131
- https://github.com/waired-ai/waired-agent/issues/1035
- https://github.com/waired-ai/waired-agent/issues/908
- https://github.com/waired-ai/waired-agent/issues/1079
- https://github.com/waired-ai/waired-agent/issues/436
- https://github.com/waired-ai/waired-agent/issues/1125
- https://github.com/waired-ai/waired-agent/issues/1126
- https://github.com/waired-ai/waired-agent/issues/1127
- https://github.com/waired-ai/waired-agent/issues/866
- https://github.com/waired-ai/waired-agent/pull/883
- https://github.com/waired-ai/waired-agent/issues/843
- docs/knowledges/20260819/2130-local-engine-caches-the-prefix.md
- docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md
- docs/decisions/20260829/1600-move-both-engine-pins.md
