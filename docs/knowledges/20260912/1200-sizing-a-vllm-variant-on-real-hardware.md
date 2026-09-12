# vLLM variant の min_vram_mb は実測で決める (20260912 12:00)

## Issue

`proto/catalog/bundled/*.json` に vLLM（safetensors）variant を足すとき、
`min_vram_mb` を重みのサイズから見積もると外れる。waired-agent#575 で
qwen3.5 系に bf16 ビルドを足したときの実測と、そこで踏んだ罠を残す。

計測環境: Linux / RTX PRO 4000 Blackwell（24,467 MiB）/ vLLM 0.28.0（pin）。
すべて `--max-model-len 200704`（コーディング窓）・`--kv-cache-dtype fp8`・
`--max-num-seqs 16`。

## Learnings

### 1. コーディング窓では KV が重みを上回る

200,704 token を持たせると、KV は `kv_bytes_per_token_fp16` × 窓 × 0.5（fp8）。
4B / 9B は 32,768 B/token なので **3.3 GB**、0.8B / 2B は 12,288 B/token で 1.2 GB。
重みだけを見ると順序が逆転する:

| build | 重み | 起動する予算 | 起動しない予算 | 起動時の KV |
|---|---|---|---|---|
| `Qwen/Qwen3.5-0.8B` bf16 | 1.75 GB | 8 GB | — | 363,129 token |
| `Qwen/Qwen3.5-2B` bf16 | 4.55 GB | 12 GB | 10 GB | 430,375 token |
| `RedHatAI/Qwen3.5-4B-quantized.w4a16` | 5.54 GB | 16 GB | 12 GB | 314,809 token |
| `Qwen/Qwen3.5-4B` bf16 | 9.32 GB | 20 GB | 16 GB | 307,678 token |
| `Qwen/Qwen3.5-9B` bf16 | 19.31 GB | — | 24 GB | — |

4B の w4a16（5.54 GB）が 16 GB を要求し、2B の bf16（4.55 GB）は 12 GB で足りる。
重みは 1 GB しか違わないのに床が 4 GB 違うのは KV の差（32,768 対 12,288）。

失敗はすべて vLLM が KV を確保できないという形で、`No available memory for
the cache blocks` か `To serve at least one request with the model's max seq
len (200704), N GiB KV cache is needed` のどちらか。vLLM は spill しないので、
`proto/hostfit` の予算式どおりに拒否する。

### 2. 小さいカードは `--gpu-memory-utilization` で近似できる（近似だと書くこと）

1 枚しか無いカードで別サイズの床を探すには、予算式
（`gpuMemUtil × (VRAM − 1024 MiB)`、`proto/hostfit/vllm.go`）が目標カードと
同じ値になる util を渡す:

    util = 0.85 × (目標VRAM_MiB − 1024) / (実カードVRAM_MiB − 1024 + 1024)
         ≈ 0.85 × (目標VRAM_MiB − 1024) / 実カードVRAM_MiB

24,467 MiB のカードなら 8 GB → 0.2490、12 GB → 0.3913、16 GB → 0.5336、
20 GB → 0.6759。**再現するのはメモリ予算だけ**で、演算性能もドライバの確保量も
目標カードのものではない。だから得られるのは「その予算で起動したか」であって
「そのカードの能力」ではない。PR や issue に数値を出すときは近似だと明記する。

### 3. hybrid-mamba は `--max-num-seqs` の既定 256 で起動を拒否する

qwen3.5 / 3.6 / 3.8 系はすべて `attention_arch: hybrid_mamba`。vLLM は
sequence 1 本につき Mamba state block を 1 つ起動時に確保するので、残りメモリが
256 個分に足りないとエンジンが**起動を拒否**する:

    ValueError: max_num_seqs (256) exceeds available Mamba cache blocks (85).

KV とは別枠なので waired-agent#675 の `--max-model-len` clamp では届かない。
メモリとも窓とも言わないエラーが出る。waired-agent#1312 で `--max-num-seqs 16`
を渡すようにした。床を測るときも同じ値で測らないと、製品と違う数字が出る
（同じ 0.8B で KV が 300,021 → 363,129 token に増えた）。

### 4. 起動後 1 本目は flashinfer の JIT を払う

同じモデル・同じプロンプトで **cold 13.66 s / warm 0.69 s**（first token まで）。
速度を測るなら必ずウォームアップを 1 本入れる。入れずに測ると、予算を絞った方が
速いという逆さまの表ができる（実際に一度そうなった）。

### 5. 量子化はサイズだけでなく速度にも効く

同じ 4B で、w4a16（5.54 GB）が 103 tok/s、bf16（9.32 GB）が 64 tok/s。
床も 16 GB 対 20 GB。量子化を「小さくなるが遅い/劣る」と説明しない。

（warm・21k token プロンプト・200 token 生成の decode 速度。参考までに
0.8B bf16 は 303 tok/s、2B bf16 は 141 tok/s。）

## Refs
- https://github.com/waired-ai/waired-agent/issues/575
- https://github.com/waired-ai/waired-agent/issues/1298
- https://github.com/waired-ai/waired-agent/issues/1312
- `proto/hostfit/vllm.go` — 予算式と `VLLMMaxModelLen`
- `internal/router/vllm_tuning.go` — `VLLMMaxNumSeqs`
