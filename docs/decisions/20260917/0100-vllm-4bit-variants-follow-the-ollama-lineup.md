---
status: accepted
---

# vLLM にも ollama と同じ並びで 4bit の variant を置き、org の外の量子化は revision を固定して採る (20260917 01:00)

## Status
Accepted。オーナー判断 2026-09-16（waired-ai/waired#1427）。

## Context

- vLLM の variant は、qwen3.5-0.8b / 2b / 4b の BF16 と、qwen3.8-27b / qwen3.6-27b の FP8 だけだった。vLLM を選んだホストには 4bit の選択肢が無く、24〜48 GB のカードでは 9B 以上のモデルが動かなかった。
- オーナーの言葉:
  - 「q4 モデルがほしいですね。ollama と同じラインナップはできませんか？」
  - 「nemotron はなくていいです。」
  - 量子化の出所について「有志の量子化も認める」。
- それまでの規則: AWQ の variant は `Qwen/` の org のリポジトリに限る（`proto/catalog` の `Validate` と、agent の `TestBundledManifests_AWQOrgConstraint`）。

## Decision

1. **足す 4bit の vLLM variant は 9 本。** 候補は、vLLM v0.29.0 のソースを読んで、製品の起動引数のまま読み込めることを確かめた（GPU では動かしていない）。

   | モデル | リポジトリ（revision は manifest に固定） | 量子化 | 重み GB | `min_vram_mb`（推測） | tier |
   |---|---|---|---:|---:|---:|
   | qwen3.5-0.8b | kaitchup/Qwen3.5-0.8B-autoround-W4A16 | W4A16 | 1.13 | 7,168 | 12 |
   | qwen3.5-2b | cyankiwi/Qwen3.5-2B-AWQ-4bit | W4A16 | 2.50 | 9,216 | 26 |
   | qwen3.5-4b | RedHatAI/Qwen3.5-4B-quantized.w4a16 | W4A16 | 5.52 | 15,360 | 41 |
   | qwen3.5-9b | RedHatAI/Qwen3.5-9B-quantized.w4a16 | W4A16 | 11.44 | 23,552 | 53 |
   | qwen3.5-27b | cyankiwi/Qwen3.5-27B-AWQ-4bit | W4A16 | 20.06 | 37,888 | 66 |
   | qwen3.5-35b-a3b | Qwen/Qwen3.5-35B-A3B-GPTQ-Int4 | GPTQ-int4 | 24.42 | 38,912 | 74 |
   | qwen3.6-27b | nvidia/Qwen3.6-27B-NVFP4 | NVFP4 | 21.92 | 39,936 | 70 |
   | qwen3.6-35b-a3b | nvidia/Qwen3.6-35B-A3B-NVFP4 | NVFP4 | 23.42 | 36,864 | 83 |
   | qwen3.8-27b | nvidia/Qwen3.8-27B-NVFP4 | NVFP4 | 21.92 | 39,936 | 88 |

   - qwen3.5-0.8b は、最初に挙げた Intel/Qwen3.5-0.8B-int4-AutoRound にライセンスの表記が無かったので、同じ形式（AutoRound int4、group 128）で apache-2.0 を明記した kaitchup のリポジトリにした（オーナー確認 2026-09-17）。chat template は Intel のものとバイト単位で一致した。
   - cyankiwi の「AWQ」のリポジトリは、中身が compressed-tensors の pack-quantized（重みだけ int4）なので、量子化の名前は `W4A16` とした。
   - 出せないもの:
     - qwen3.8-flash-next: n-gram の埋め込み層が vLLM 0.29.0 では 4bit にならず、どのビルドも参照機の予算（98,304 MB）を超える。
     - qwen3.5-122b-a10b: 元の org とベンダーの 4bit 版は 76〜84 GB で、推測は予算を超える。予算に収まるのは、エキスパートを刈り込んだものと個人の変換版だけだった。

2. **`min_vram_mb` は推測で決める。**
   - 規則: `hostfit.VLLMMaxModelLen` の式で、fp8 KV と 200,704 トークンのコンテキストウィンドウが載る最小の VRAM に 2,048 MiB を足し、1,024 の倍数に丸める。この規則は、qwen3.5-0.8b / 2b / 4b の BF16 の実測値 8,192 / 12,288 / 20,480 を再現する。
   - 同じ規則で、qwen3.8-27b / qwen3.6-27b の FP8 を 38,912 から 52,224 に直した。38,912 MB では、式の L が 0 になる（200k のコンテキストウィンドウを張れない）。
   - 参照機（Strix Halo 128 GB）は AMD なので、NVFP4 の variant はそこで測れない。W4A16 と GPTQ の ROCm での動作も確かめていないので、`vendor_support.amd.vllm` は NVFP4 を `unsupported`、それ以外を `experimental` とした。

3. **org の外で公開された量子化も採る。そのかわり revision を 40 桁のコミットで固定する。**
   - `Validate` の「AWQ は `Qwen/` だけ」をやめた（waired-ai/waired-agent#1426）。
   - `TestBundledManifests_OutsideQuantizationsPinARevision` を置いた。対象は、モデルの別名が名指す org 以外のリポジトリ。コミュニティのリポジトリは同じ名前のまま中身が変わりうる（#1305 では ollama のタグが 55 GB から 79 GB に変わった）。
   - 同じ理由で、既存の nvidia/GLM-5.2-NVFP4 にも revision を固定した。

4. **tier は、同じモデルの中で精度の順を崩さない位置に置く**（`TestBundledManifests_QualityTierFollowsPrecisionWithinAModel`）。既存の番号は次の 3 つだけ動かした:
   - qwen3.6-27b の fp8: 70 → 71。
   - qwen3.5-0.8b の bf16: 13 → 14。
   - qwen3.5-0.8b の q8-gguf: 12 → 13。

5. **既定の variant**
   - すでに vLLM の variant を持つモデル（0.8b / 2b / 4b の BF16、27B の FP8）は、既定を変えない。
   - 新しく vLLM の variant を持つモデル（qwen3.5-9b / 27b / 35b-a3b、qwen3.6-35b-a3b）は、その 4bit を既定にし、`runtime.fallback` に vllm を足した。

## Consequences

- vLLM を選んだ 8 GB の NVIDIA カードで、qwen3.5-0.8b の W4A16 が選ばれるようになる（推測の値で、8 GB のカードでは測っていない）。vLLM の自動選択は無効のままなので、エンジンを明示しないホストの挙動は変わらない。
- tool parser の表に qwen3.5-9b / 27b / 35b-a3b、qwen3.6-35b-a3b の行を足した。どのリポジトリも、固定した revision の `chat_template.jinja` が、同じ世代の Qwen 公式のテンプレートとバイト単位で一致した。
- 参照機の秒数（`internal/catalog/turnspeeds.json`）は、`docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md` の決定 2 に従い、同じモデルの ollama の既定 variant の値を借りる。
- MTP の項（waired-ai/waired-agent#1425）は、固定した revision の `config.json` と重みの索引から埋めた。
  - 8 本は MTP 層を 1 つ持つ（`mtp_layers` 1）。`mtp_kv_bytes_per_token_fp16` は、35B-A3B / 2B が 2,048、ほかは 4,096。
  - kaitchup の 0.8B 版は、config では MTP 層が 1 だが、重みに MTP のテンソルが無い。そのため `mtp_layers` を 0 にした。
  - `mtp_draft_tokens` はどれも 0（draft を走らせない）。draft の長さは 1 本ごとに測ってから決める項目で、4bit の variant ではまだ誰も測っていない。

## Refs
- waired-ai/waired#1427
- waired-ai/waired-agent#1426
- `docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md`
