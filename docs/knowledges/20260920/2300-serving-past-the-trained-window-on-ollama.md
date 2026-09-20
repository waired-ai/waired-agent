# 学習した長さを超えて配信する — ollama 0.34.0 / llama.cpp b10760 で確かめたこと (20260920 23:00)

## Issue

waired-ai/waired#1456 は「YaRN で 1M まで広げられるモデルを、警告を出したうえで 1M で出す」。
起票時点の本文は、ollama が rope の指定を runner に渡さないので GGUF にキーを足すしかない、
という前提で書かれていた。**その前提は違った。** pin されたエンジンのソースと参照機での実測で
確かめたことを、次に pin を動かす人のために並べる。

「根拠」は**ソース**(版を固定して読んだ)と**実測**(sv-evox2、Strix Halo 128 GB、Vulkan)を分ける。

## Learnings

| # | 事実 | 場所 | 根拠 |
|---|---|---|---|
| 1 | **ollama 0.34.0 は上流の `llama-server` をそのまま subprocess として起動し、`os.Environ()` を丸ごと渡す。** だから llama.cpp 自身の `LLAMA_ARG_*` が runner に届く。ollama 自身も `LLAMA_ARG_FIT_TARGET` で同じ仕組みを使っている | `llm/llama_server.go`(冒頭コメント / `FindLlamaServer` / `exec.Command` / `cmd.Env = os.Environ()`) | ソース + 実測 |
| 2 | **YaRN の値は GGUF ではなく context のパラメータから来る。** graph が `freq_scale` / `ext_factor` / `attn_factor` / `beta_*` / `n_ctx_orig` を `cparams` から取り、context は CLI / 環境変数を優先して GGUF の hparams をフォールバックにする | `src/llama-graph.cpp` / `src/llama-context.cpp` | ソース |
| 3 | **YaRN は IMROPE と合成される。** Vulkan の `rope_multi()` は IMROPE の分岐でも `rope_norm` / `rope_neox` と同一の `rope_yarn()` を呼ぶ。YaRN は rope の変種に後から掛かる層で、`qwen35` / `qwen35moe` だけ外れる構造ではない | `ggml/src/ggml-vulkan/vulkan-shaders/rope_funcs.glsl` | ソース |
| 4 | **到達値はエンジンが計算する**: 独自の YaRN を渡すと `n_ctx_train = n_ctx_orig_yarn / rope_freq_scale` を再計算し、ログに `custom YaRN scaling detected` と `n_ctx_train adjusted to <値>` を出す。262,144 ÷ 0.25 = **1,048,576** | `src/llama-context.cpp` | ソース + 実測 |
| 5 | **`attn_factor` を渡してはいけない。** エンジンが係数から mscale を導き、カーネル側の分を打ち消す。渡すと二重に効く | `src/llama-context.cpp` | ソース |
| 6 | **唯一の壁は切り詰め**: `num_ctx` が GGUF 自身の `context_length` を超えると警告を出して落とす。`-c` は CLI に明示されるので `LLAMA_ARG_CTX_SIZE` では上げられない | `llm/server.go` / `llm/llama_server.go` | ソース + 実測(`WARN ... msg="requested context size too large for model" num_ctx=1048576 n_ctx_train=262144`) |
| 7 | **`context_length` の u32 を 1 個書き換えれば切り詰めは消える。** ollama は読み込み時に blob の digest を再検証しない(検証は pull のときだけで、既に在る blob は `skipVerify`) | `server/images.go` | ソース + 実測 |
| 8 | **同じ(書き換えた)blob で 200,704 も 1,048,576 も配信できる。** 環境変数を外すと `freq_scale = 1` に戻り、短い入力は今と同一 | — | 実測 |
| 9 | **`ollama show --modelfile` は blob のパスを出さない。** `FROM <タグ名>` を出す。`/api/show` にもパスは無い。タグ → パスは自分で導くしかない | `server/routes.go` | ソース |
| 10 | Qwen の GGUF に `rope.scaling.*` のキーは**無い**(`freq_base` / `dimension_count` / `mrope_interleaved` / `mrope_section` だけ)。waired-ai/waired-agent#451 の全サイズ確認と一致 | — | 実測 |

## タグ → blob の導出(#9 の帰結)

ollama の `types/model.ParseName` と同じ 4 分割を再現する。**最後の `:` が最後の `/` より
後ろにあるときだけ**タグを切り、そこから右から host / namespace / model を切って、
足りない分を `registry.ollama.ai` / `library` / `latest` で埋める。パスは
`<store>/manifests/<host>/<namespace>/<model>/<tag>`、blob は
`<store>/blobs/sha256-<hex>`(`:` を `-` に)。カタログが使う 2 形で実機の store と一致した:

```
qwen3.6:35b-a3b-mtp-q4_K_M                 -> registry.ollama.ai/library/qwen3.6/35b-a3b-mtp-q4_K_M
hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL  -> hf.co/unsloth/Qwen3.8-27B-GGUF/UD-Q3_K_XL
```

`localhost:5000/library/m` のようにポートを含む形があるので、**最初の `:` で切ってはいけない**。

## 参照機での数字

`qwen3.6-35b-a3b/mtp-q4-gguf`(q4_0 KV、flash attention on、並列 1):

| 窓 | KV | MTP draft KV (f16) | 再帰状態 | 常駐合計 | 読み込み+応答 |
|---|---|---|---|---|---|
| 200,704 | 1,102.50 MiB | 392.00 MiB | 188.44 MiB | 23,638 MB | 19.5 s |
| 1,048,576 | 5,760.00 MiB | 2,048.00 MiB | 188.44 MiB | 29,560 MB | 24.1 s |

- **`hostfit` の見積りは、エンジン自身の確保とバイト単位で一致した。** 200,704 でも
  1,048,576 でも、主 KV も MTP draft の KV も、llama.cpp のログの値そのものになる:

  | 窓 | `hostfit` の主 KV | エンジン | `hostfit` の draft KV | エンジン |
  |---|---|---|---|---|
  | 200,704 | 1,102.50 MiB | 1,102.50 | 392.00 MiB | 392.00 |
  | 1,048,576 | 5,760.00 MiB | 5,760.00 | 2,048.00 MiB | 2,048.00 |

  主 KV は `KVBytesPerTokenFP16 × OllamaKVBlockFactorQ4_0`(= 18/64。ggml の q4_0 は 32 個の
  値を 18 バイトに入れるので、単純な 1/4 ではない)。draft KV は
  `KVBytesPerTokenFP16 / GGUF.FullAttentionLayers` — draft head は 1 層ぶんで、`--cache-type-k`
  に関わらず f16(`proto/hostfit/estimate.go`)。qwen3.6-35b-a3b では 20,480 / 10 = 2,048 B/token。

  **この一致は 1M では誰も確かめていなかった。** ollama の経路は
  `Variant.MTPKVBytesPerTokenFP16` を**読まない**(あれは vLLM 用で、ollama 側は層数から
  導く)ので、その欄が空なのを見て「1M の価格付けに項が足りない」と読むのは誤り — この
  記録の最初の版がその誤りを書いていた。
- 8 タグはすべて model blob が別だった(共有は license blob だけ)。alias タグがある家族では
  共有し得るので、書き換える前に**同じ blob を指すタグを数える**こと。

## プレフィルの時間 — これが本当の制約

同じ機械、1M の窓、瞬間スループット:

| 累積トークン | tok/s |
|---|---|
| 6,144 | 286 |
| 22,528 | 109 |
| 47,104 | 57 |
| 71,680 | **38** |

経過時間はトークン数の約 **n^1.9**。33 万トークンの投入は 5.9 時間コースで打ち切った。
**1,048,576 を一度に埋めると数十時間**になる。読み込みは問題ではなく(1M で 24.1 秒)、
メモリも問題ではない。**窓を埋める時間が制約**で、鋭い縁はプレフィックスが外れたとき。

計測の条件: `qwen3.5-0.8b-q8_0`、製品エンジンが 59 GB 常駐して稼働中の競合下。出荷モデル・
空き機なら定数は改善するが、n² の法則は変わらない。

## Why

起票時の前提(「GGUF にキーを足すしかない」)で実装に入ると、配布元のファイルをヘッダごと
書き換える = 20 GB のコピーになる。**実際に要るのは u32 1 個**で、しかも YaRN 自体は
環境変数で足りる。#1 と #6 を読み分けられるかで、設計の規模が 2 桁変わる。

## How to apply

- 次に **ollama の pin を動かす**ときは #1(llama-server を起動するか)と #6(切り詰めの位置)を
  読み直す。この 2 つのどちらが変わっても 1M の経路は壊れる。
- 次に **1M の資源を見積もる**ときは MTP draft の f16 KV を忘れない。
- 1M を「使える」と言う前に、**埋める時間**を見る。メモリが足りることは足りることの半分。

## Refs
- https://github.com/waired-ai/waired/issues/1456
- https://github.com/waired-ai/waired-agent/issues/451
- https://github.com/waired-ai/waired-agent/issues/1443
- https://github.com/ollama/ollama/tree/v0.34.0
- https://github.com/ggml-org/llama.cpp/tree/b10760
- `docs/decisions/20260920/2300-the-long-window-is-asked-for-and-the-stored-file-is-raised-to-allow-it.md`
- `docs/knowledges/20260917/1050-engine-overrides-ollama-0340-vllm-0290.md`
