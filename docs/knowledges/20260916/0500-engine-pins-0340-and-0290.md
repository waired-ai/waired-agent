# エンジン pin 移動の実測 — ollama 0.34.0 / vLLM 0.29.0 (20260916 05:00)

## Issue

ollama `0.33.3` → `0.34.0`、vLLM `0.28.0` → `0.29.0`、uv `0.11.26` →
`0.12.15` への pin 移動の実測記録。前回の移動
(docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md、PR #1148)
と同じ理由で、作業の実体は測定である: この製品は upstream が約束して
いない挙動をエンジンから読み出しており、それが変わってもエラーには
ならず、数値と判断が黙って偽になるだけだから。

今回、pin の行以外にコードを変えた発見が 3 つある (§4〜§6)。うち 1 つ
(uv の managed Python) は waired-ai/waired#588 の python3-dev の半分に
あたる。同 issue の libcuda.so の半分はこの PR では扱っていない。

計測環境 (すべて 2026-09-16):

- RTX PRO 4000 Blackwell (24 GB) の Linux 機 — vLLM の主な計測。
- WSL2 の開発機 (RTX 5080 16 GB + RTX 5070 Laptop 8 GB) — ollama の
  Linux 計測 (RTX 5070 Laptop)、WSL2 固有の 2 件、ollama の MTP 実験。

vLLM は製品の adapter が出す argv の形で回した: gpu-memory-utilization
0.85、KV キャッシュ fp8、max-num-batched-tokens 4096、max-num-seqs 16、
プレフィックスキャッシュ有効。モデルは gpt-oss-20b (max-model-len
124928、tool parser `openai`) と Qwen3.5-4B bf16 (65536、`qwen3_xml`)。
対照の 0.28.0 は同じインストーラで同じ形に組んだ venv。エンジンは
常に 1 つだけ動かした。デコード速度はストリーミングで 400 トークン
生成を 3 回行った中央値。「novel」は新しいコードを書かせる、「edit」は
プロンプトに入っているコードを書き直させるプロンプト。

## Learnings

### 1. ollama 0.34.0 — 製品が読んでいる経路に変更が無い (静的確認)

- 0.33.3 と 0.34.0 の間にリリースは無い。同梱の llama.cpp は
  **b10760 のまま**なので、hostfit が 0.33.3 で測った llama.cpp 由来の
  項はそのまま当てはまる。
- ggml-org/llama.cpp#28330 (QSA indexer の V キャッシュ、2026-09-10 に
  merge) が最初に入るのは b10889 で、0.34.0 には入っていない。
  `ollama_version.go` の「次の bump で読み直す」段落はまだ生きている。
- リリースの追加は OpenAI 互換面の機能 (response compaction
  `/v1/responses/compact`、client tool search、Codex 用の proxy route)。
  `server/sched.go` はログ行のデータ競合修正だけ。`llm/`、バッチの
  サイジング、`/api/ps`、keep_alive、既定のコンテキストウィンドウ、
  renderer に変更は無い。
- `TestPinnedReleasePublishesEveryAssetChecksum` と
  `TestOllamaInstaller_RealArchive` は 0.34.0 に対して通る。darwin と
  windows のアーカイブは 0.33.3 と同じ 59 / 87 エントリを列挙し、linux
  は今も `bin/` + `lib/` に展開される。3 アーカイブの sha256 は
  sha256sum.txt と一致した。

### 2. ollama 0.34.0 — Linux で再測定した挙動は全部持ちこたえた

RTX 5070 Laptop、`qwen3.5:0.8b-q8_0`:

- 先頭でない system ターンは `/v1/chat/completions` と `/api/chat` の
  両方で HTTP 200。
- keep_alive の非対称はそのまま: `/v1` 経由の 37m は既定の約 5 分の
  失効のまま、`/api/chat` 経由の 41m は 41 分に動いた。
- `/api/ps` のキーは不変 (`context_length` / `details` / `digest` /
  `expires_at` / `model` / `name` / `size` / `size_vram`)。
- `cached_tokens` は両面で報告される (再送で 918)。
- engine.log は logfmt のままで `msg="..."` を持つ。
- runner の argv は今も `-c 4096 -np 1 ... --cache-type-k q4_0
  --cache-type-v q4_0 --flash-attn on -b 512 -ub 512 --context-shift
  --keep 4`。7.9 GiB のカードに対する VRAM 段の既定コンテキストは 4096。
- `ParseLlamaPlacement` (#1384 のパーサ) は 0.34.0 のロードログを同じ
  フィールドに読む。
- カタログが刻む renderer `qwen3.5` と `qwen3.8` は今も登録されている:
  刻んだタグは先頭でない system ターンに 200 を返し、未登録の renderer
  名は 500 を返す。

Windows でも同じ確認を走らせた (Strix Halo の Windows 機、Radeon 8060S を
Vulkan で、製品と同じ `OLLAMA_VULKAN=1`、同じ `qwen3.5:0.8b-q8_0`)。先頭で
ない system ターンは両面で 200、keep_alive は `/v1` の 37m で 5 分・
`/api/chat` の 41m で 41 分、`/api/ps` のキーは上と同じ 8 つ、
`cached_tokens` は 918。VRAM 段の既定コンテキストは 102.2 GiB に対して
262144 で、runner は `-c 262144 -np 1` で起動し、`load_tensors: offloaded`
/ `common_params_fit_impl: projected to use` / `llama_kv_cache: size` の行も
同じ形で出た。macOS はこの PR ではアーカイブの配置だけを確認し、実機では
走らせていない。

### 3. vLLM 0.29.0 — pin タプルと受理されたフラグ

- 解決結果: torch 2.13.0+cu130 / transformers 5.17.0 / huggingface_hub
  1.31.0 / flashinfer-python 0.6.18 / triton 3.7.1 / python 3.12.14
  (uv managed)。**flashinfer-cubin は今も宣言されていない**ので、
  0.28.0 で入れた nvcc を PATH に前置する修正は引き続き荷重を持つ。
  対照の 0.28.0 は flashinfer-python 0.6.16.post3。
- transformers の床が `>=5.10.4` に上がった。`TransformersConstraint` は
  `transformers>=5.10.4,<6.0`。`VLLMPythonVersion` は 3.12 のまま
  (requires_python は `<3.15,>=3.10` で不変)。
- adapter が出し得る全フラグが受理された (`--kv-offloading-size 4
  --kv-offloading-backend native`、`--speculative-config` の ngram JSON
  を含む)。どの実行にも "unrecognized arguments" は出ていない。
- `GPU KV cache size:` 行の形式は不変で、`vllmKVCapacityRe` に掛かる。
- スケジューラの段は不変: 70 GiB 未満 2048 / 256、70 GiB 以上 (A100
  以外) 8192 / 1024、160 GiB 以上 16384 / 1024。
- tool parser の登録表は `hy_v4` が増えただけで、製品が使う 5 名
  (`hermes` / `qwen3_xml` / `openai` / `glm45` / `deepseek_v4`) は全部
  登録されている。
- 応答は `reasoning` を持ち `reasoning_content` は無い (0.28.0 と同じ)。

### 4. コードを変えた発見 1 — `api_server` モジュールが非推奨になった

0.29.0 は `python -m vllm.entrypoints.openai.api_server` を非推奨にし
(vllm-project/vllm#52131)、`vllm serve` の下では `--model` オプションも
非推奨で、どちらも警告を刷る。adapter の起動を
`python -m vllm.entrypoints.cli.main serve <model>` (モデルは位置引数)
に変えた。

- 0.28.0 の venv も同じモジュールから `vllm serve [model_tag]` に答え、
  gpt-oss-20b は 0.28.0 でも serve の形で起動した。converge 前の venv
  も同じ argv で起動できる。
- serve 経由の未知フラグは `main.py: error: unrecognized arguments:
  --nope` で exit 2。起動失敗の診断は既に "unrecognized arguments" を
  キーにしているので、serve の形の逐語フィクスチャを足すだけで済んだ。

### 5. コードを変えた発見 2 — WSL2 では pinned host memory を明示する

vLLM は WSL2 を検出すると pinned host memory を既定で切る。0.29.0 の
既定である Model Runner V2 はこれを必要とし、V1 にフォールバックせずに
`RuntimeError: UVA is not available` で死ぬ。`VLLMAdapter.processEnv`
は、operator が既に export していない限り `VLLM_WSL2_ENABLE_PIN_MEMORY=1`
を足す (`ExtraEnv` はその後に並ぶので勝つ)。vLLM がこの変数を読むのは
WSL を検出したときだけで、ネイティブ Linux には影響しない。これで
0.29.0 は WSL2 で起動した。

WSL2 の開発機 (RTX 5080)、Qwen3.5-2B bf16、コンテキスト 32768 の
デコード: 0.29.0 V2 (pinned memory 有り) 205 tok/s、0.28.0 196、
0.29.0 で V1 を強制 (pinned memory 無し) 204。

### 6. コードを変えた発見 3 — venv は uv の managed Python で作る

Ubuntu 24.04 (WSL2) で uv はシステムの `/usr/bin/python3.12` (3.12.3)
を掴んだ。python3.12-dev が無いので `Python.h` が無く、vLLM の Triton
カーネルがモデルを調べる時点で C の shim を `Python.h` に対して
コンパイルしようとして、Qwen3.5 は 0.28.0 でも 0.29.0 でも
`fatal error: Python.h: No such file or directory` で死んだ。managed の
CPython 3.12.14 (ヘッダ同梱) では起動した。

`uv venv` の呼び出しだけに `UV_MANAGED_PYTHON=1` を載せる。
`--managed-python` フラグではなく環境変数にしたのは、フラグを知らない
古いシステム uv が今日の挙動に縮退するため。pip と verify の呼び出しには
載せない — venv 自身のインタプリタを使うので不要で、システム
インタプリタで作られた既存の venv も converge できる必要がある。

Linux の Blackwell 機には PATH に python3.12 が無く、uv は既に managed
のものをダウンロードしていた — つまりこの問題はシステム Python が
在るホストでだけ出る。

### 7. hf_transfer を pin から外した理由

venv が解決する huggingface_hub 1.31.0 は hf_transfer を使わない:
`HF_HUB_ENABLE_HF_TRANSFER` を無視して FutureWarning ("hf_transfer is
not used anymore") を刷り、ダウンロードは hf_xet を通る。使われない
wheel を pin する理由が無いので、`HFTransferPinnedVersion` /
`VLLMPinSet.HFTransfer` / `InstallOpts.HFTransferVersion` / renovate の
規則を外し、`internal/download/hf.go` の FastTransfer トグルと `=0` の
フォールバックを消した。auth でも not-found でもない失敗は同じ要求で
1 回だけ再試行する。

- `HF_XET_HIGH_PERFORMANCE` は設定しない: 4.3 GB のリポジトリで有り
  59 s、無し 65 s (同じ回線の実行間のばらつきの内側で、それ以前の
  既定の実行は 61 s)。RSS は +1.3 GB。
- `"hf_transfer"` キーを持つ古い pin 記録はそのまま decode できる (JSON
  は未知キーを無視)。そういう venv は 0.28.0 以前なので、版の不一致で
  どのみち作り直される。

### 8. Model Runner V2 — デコードは動かず、KV プールが動いた

**デコード速度は不変**: gpt-oss-20b は 0.28.0 / 0.29.0 (V2) / 0.29.0 で
V1 を強制 / interactivity mode / api_server と serve のどちらの入口でも
131〜135 tok/s。Qwen3.5-4B は両リリースとも 66 tok/s。

**KV キャッシュのプール (`GPU KV cache size:` 行) は動いた**。
gpt-oss-20b:

| 条件 | 0.28.0 | 0.29.0 (V2) | 差 |
|---|---|---|---|
| その構成での初回起動 | 285,284 | 264,060 | −7.4% |
| 同じ構成の 2 回目以降の起動 | 399,082 | 379,778 | −4.8% |

0.29.0 で V1 を強制した初回起動は **285,284 ちょうど**で、0.28.0 の
初回と同じ。V2 の差は vLLM 自身のメモリ内訳 (どちらも初回起動) に出て
いる: V1 は
アクティベーションのピーク 1.36 GiB + CUDA graph 0.39 GiB (見積り
0.55)、V2 はピーク 1.63 GiB + CUDA graph は実測 0.12 GiB だが見積り・
予約が 0.81 GiB。KV に使えるメモリは 3.48 GiB 対 3.22 GiB。

Qwen3.5-4B のプールは 523,338 (0.28.0) 対 524,288 (0.29.0)。

**方法上の注意 1**: 初回起動と 2 回目以降の差 (約 30〜40%) は両
リリースにある。2 回目以降の起動では vLLM の内訳の「weights +
non-torch」が約 0.8 GiB、アクティベーションのピークが約 0.6 GiB 小さく
出る。torch.compile / CUDA graph のキャッシュの有無が効いていると推定
しているが、切り分けはしていない (0.29.0 で V1 を強制した初回起動は
起動 34 秒でキャッシュに当たっていたのに 285,284 だった)。**プールの数値を
記録するときは、その構成で何回目の起動かを添えること。** 前回の記録が
「起動時の空き VRAM の関数」と書いた事情に、もう 1 つ軸が加わった。

**方法上の注意 2**: 当初「`vllm serve` のほうがプールが大きい」ように
見えたが、起動の回数で説明がついた (同じ構成の 2 回目の api_server と
serve はどちらも 379,778)。入口の差を測るつもりなら、何回目の起動かを
揃えてから比べること。

### 9. プレフィックス再利用 — 0.29.0 の retention interval の既定

約 5,540 トークンのエージェント形の履歴の再送は、両リリースとも 5,536
トークンを再利用する (gpt-oss)。履歴を 1/3 あたりで編集した場合:

| | 再利用 | TTFT |
|---|---|---|
| 0.28.0 | 1,904 トークン | 0.38 s |
| 0.29.0 (V2 でも V1 強制でも) | 64 トークン | 0.56 s |

V1 強制でも同じなので、Model Runner V2 ではなく、0.29.0 が
sliding-window / Mamba のグループに入れた既定
`prefix_cache_retention_interval=0` (vllm-project/vllm#52216) による。
`--prefix-cache-retention-interval` を振った結果 (gpt-oss、プールは
379,778 で不変、デコードも不変):

| interval | 再利用 | TTFT |
|---|---|---|
| 256 | 1,792 | 0.39 s |
| 1024 | 1,024 | 0.49 s |
| 4096 | 64 | — |

vLLM の検証 (`vllm/v1/core/kv_cache_coordinator.py`): sliding-window /
Mamba のグループを持たないモデルに非ゼロの interval を渡すと起動時に
ValueError を上げ、スケジューラのブロックサイズの倍数でない interval も
拒否する (gpt-oss: 16、Qwen3.5 のハイブリッド: 起動時に 1,056 が
選ばれる)。Qwen3.5-4B の中間編集は両リリースとも、interval 1056 でも、
再利用 0 トークン。

このフラグはこの PR では渡していない。

### 10. tool call

gpt-oss-20b は `vllm serve` 経由で 0.29.0 でも 0.28.0 でも
`finish_reason=tool_calls` と `get_weather {"city":"Tokyo"}` を返した。
Qwen3.5 (`qwen3_xml`、WSL2 機) も tool_calls を返した。

### 11. 0.29.0 で測っただけで、この PR では採用していない選択肢

いずれもエンジンの事実として記録する。採用の判断はここには書かない。

- **`--performance-mode interactivity --max-cudagraph-capture-size 16`**:
  デコードは不変 (gpt-oss 132 tok/s、Qwen3.5-4B 65.8)。gpt-oss の初回
  起動のプールは 287,173 (既定の初回 264,060)。graph の見積り 0.53 GiB、
  アクティベーション 1.34 GiB。
- **Qwen3.5-4B の MTP 投機的デコーディング** (checkpoint に MTP 層が
  1 つ、`method=mtp`):

  | num_speculative_tokens | novel | edit | KV プール |
  |---|---|---|---|
  | 0 (基準) | 66 | 65 | 524,288 |
  | 1 | 102 | 107 | 429,624 |
  | 2 | 123 | 140 | 407,385 |
  | 3 | 130 | 165 | 391,513 |

  プールは −18〜−25%。再送のプレフィックス再利用は 5,280 から
  4,224〜4,288 トークンに落ち、再送の TTFT は 0.12〜0.13 s から
  0.23〜0.24 s に上がった。MTP でも V2 は有効のまま。
- **ngram / ngram_gpu** (製品の opt-in JSON: num_speculative_tokens 5、
  lookup 2〜4): V2 は "Model Runner V2 does not yet support
  ngram/ngram_gpu speculative decoding" と記録して V1 にフォールバック
  し、async scheduling も切れる。novel 68〜69 tok/s、edit 233〜235
  tok/s、プール 423,586 / 422,787。製品の ngram は opt-in (既定 off) の
  まま。

### 12. ollama の MTP draft を刻む実験 (この PR では採用しない)

ollama が MTP の draft を有効にするのは、タグか要求が
`draft_num_predict` を設定したときだけ (`server/routes.go` の
`modelOptions`、v0.34.0 の 137〜159 行あたり: 未設定は 0 に強制される)。
library の `-mtp-` タグはこれを持つ (qwen3.6-27b 3、qwen3.6-35b-a3b 2、
qwen3.8-27b 4)。`hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF` のタグ
(カタログの mtp-q3-gguf / mtp-q2-gguf) は nextn ブロックを持つが
`draft_num_predict` を持たないので、**今日は draft が走らない**。

WSL2 (RTX 5070 Laptop + RTX 5080、コンテキスト 16384、KV q4_0、flash
attention on)、UD-Q2_K_XL に製品の RENDERER / PARSER qwen3.5 の刻印と
`PARAMETER draft_num_predict N` を足すと、runner に `--spec-type
draft-mtp --spec-draft-n-max N --spec-draft-backend-sampling` が
乗った:

| draft_num_predict | novel | edit | llama.cpp の fit 予測 |
|---|---|---|---|
| なし | 96 | 100 | 12,247 MiB |
| 2 | 128 | 146 | 13,098 MiB |
| 3 | 137 | 145 | 13,161 MiB |
| 4 | 138 | 176 | 13,224 MiB |

`hostfit.OllamaEstimateMemory` にその variant の
`gguf.draft_max_tokens` を N として与えると、同じ 2 GPU 機で device 側
(fit target 除く) は無しに対し +1,184 (2) / +1,247 (3) / +1,310 (4) MB
増える。実測は +851 / +914 / +977。**draft 1 段あたりの +63 MiB は
ちょうど一致**し、draft を有効にする固定費が、このコンテキスト長と
GPU 数では llama.cpp の予測より約 333 MiB 大きく見積られる (安全側)。
draft 0 の絶対値もこの 2 デバイス機では予測より上 (13,272 対 12,247
MiB)。カタログのデータは proto にあるので、刻印を変えるなら別の変更に
なる。

### 13. uv 0.12.15

`UVPinnedSHA256Linux64` は `scripts/dev/update-uv-sha.sh` で再計算し、
リリースの `.sha256` サイドカーと一致する。

今日の挙動として: `UVResolver.Resolve` は PATH 上の uv か
`~/.local/share/waired/bin/uv` を版を見ずに再利用するので、uv の pin
移動が届くのは uv を持たないホストだけ。既に uv があるホストは今の
版のまま動き続ける。

### 14. 方法上の注意 — 踏んだ罠

- **CLI の進捗行を uv の書式変更と読み違えた。** `waired runtimes
  install vllm` の `[3/6 pip-install] 0% 1.3 MB / 3.8 GB (2.1 MB/s)
  Downloaded xgrammar` は CLI が自分のバイト集計を前置している行。
  パイプ越しの uv 0.12.15 の生の出力は今も `Downloading X (N MiB)` /
  ` Downloaded X` / `Prepared N packages` で、`uvDownloadTracker` は
  そのまま動く。書式が変わったと思ったら、CLI を挟まずに生の出力を
  見ること。
- **`ollama create` を HOME 無しのサブプロセスから呼ぶと exit 2。**
  刻印の実験をスクリプトから回すときは HOME を渡す。
- **計測ホストで他の負荷を動かさない。** WSL2 機の 0.29.0 で最初に
  141 tok/s と読めた値は、同じ機で別の GPU / CPU の作業が走っている
  間に採ったもので、捨てた。負荷を止めて採り直すと 205 tok/s。
- **KV プールの数値には、その構成で何回目の起動かを添える** (§8)。

## Refs
- docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md
- https://github.com/waired-ai/waired-agent/pull/1148
- https://github.com/waired-ai/waired/issues/588
- https://github.com/ollama/ollama/releases/tag/v0.34.0
- https://github.com/vllm-project/vllm/releases/tag/v0.29.0
- https://github.com/vllm-project/vllm/pull/52131
- https://github.com/vllm-project/vllm/pull/52216
- https://github.com/ggml-org/llama.cpp/pull/28330
- https://github.com/astral-sh/uv/releases/tag/0.12.15
