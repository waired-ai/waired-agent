# モデルカタログ（提供モデル一覧）

Waired が bundled で提供するローカル LLM の一覧。固定エイリアス、ファミリ概要、全バリアントの数値（量子化・VRAM/RAM 下限・品質ティア・vendor support）を `internal/catalog/bundled` から機械生成する。

このページは Waired のエージェントが**標準で扱えるモデル**の一覧である。「どのモデルが用意されているか」「`waired/default` が実際にどのモデルへ解決されるか」を一望できる。

- 一覧の正本は `internal/catalog/bundled/*.json`（バイナリに `//go:embed` される）。型は `internal/catalog/manifest.go` の `Manifest` / `Variant`。
- 下表は `catalog-tool docs`（`cmd/catalog-tool/docs.go`）が bundled manifest から**機械生成**する。`<!-- BEGIN GENERATED ... -->` / `<!-- END GENERATED ... -->` の間だけが生成対象で、その外側の本文は手書き。
- 鮮度保証: bundled JSON を変更したのに本ページを再生成し忘れると CI（`catalog-tool docs --check`）が落ちる。週次の catalog-radar（monorepo #413、.github/workflows/catalog-radar.yml）が出す draft PR も同じ経路で本ページを更新する。手で表を編集しないこと。

コーディングエージェントが提示する固定別名は `waired/default`（コーディング既定）・`waired/coding`・`waired/small` の 3 つ。**旧 `waired/auto` は #422/#478 で `waired/default` に改称済み**で、現行のプラグイン / 同梱 UI はすべて `waired/default` を emit する。

**表の構成**: 「ファミリ概要」「全バリアント（数値）」はいずれも **エンジン（Ollama / vLLM）→ アーキテクチャ（Dense → MoE）** で分割する。エンジン（`runtime_support`）はバリアント単位なので、両エンジン向けのビルドを持つファミリは Ollama 節と vLLM 節の両方に再掲される（自分のハードに対応する節だけ読めばよい）。Dense / MoE はファミリ単位（`active_params`）で、Dense=全パラメータが毎トークン計算で計算 / VRAM に余裕がある環境向き、MoE=総サイズは大きいがアクティブ少でメモリリッチな Unified メモリ機（Apple Silicon・Strix Halo）向き。エンジン自動判定の規則は dev-docs の「推論層 → engine picker」を参照。

モデルの**選び方**（hardware 適合・auto 選択・ピア間フォールバック・品質ティアの算出）は dev-docs の「推論層」を、コーディングエージェントから別名で叩く仕組みは「コーディングエージェント連携」を参照。

## bundled カタログ

<!-- BEGIN GENERATED: catalog-tool docs -->

> この節は `internal/catalog/bundled/*.json` から `catalog-tool docs` が機械生成する。**手で編集しない** — モデルを追加・更新したら `make catalog-docs`（または `catalog-tool docs`）で再生成してコミットする。catalog-radar（#413）の自動更新も同じ経路を使う。空欄は `—`。

bundled 済み: **21 ファミリ / 34 バリアント**。

ファミリ概要・全バリアント表は **エンジン（Ollama / vLLM）→ アーキテクチャ（Dense → MoE）** で分割する。エンジンはバリアント単位（`runtime_support`）なので、両エンジン向けにビルドを持つファミリは両節に再掲される。Dense=全パラメータが毎トークン計算（計算 / VRAM 余裕がある環境向き）、MoE=総サイズ大だがアクティブ少（メモリリッチな Unified メモリ機向き・デコード高速）。

### 固定エイリアス

コーディングエージェント連携が提示する 3 つの固定別名と、それが解決する bundled モデル。

| エイリアス | 解決先 model_id | 表示名 |
| --- | --- | --- |
| `waired/default` | 動的: このホストの既定コーディングモデル（preferred > active > bundled） |  |
| `waired/coding` | 動的: waired/default と同じ解決 |  |
| `waired/small` | `qwen2.5-coder-3b-instruct` | Qwen2.5 Coder 3B Instruct |

### ファミリ概要

#### Ollama 経路（Mac / Windows / CPU / 内蔵・低VRAM GPU）

**Dense**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen2.5-coder-0.5b-instruct` | Qwen2.5 Coder 0.5B Instruct | `waired/tiny` | 32,768 | chat, tool_use, json_mode | 0.5B | ollama | 1 |
| `qwen2.5-coder-14b-instruct` | Qwen2.5 Coder 14B Instruct | `waired/medium` | 32,768 | chat, tool_use, json_mode | 14.7B | ollama | 2 |
| `qwen2.5-coder-3b-instruct` | Qwen2.5 Coder 3B Instruct | `waired/small` | 32,768 | chat, tool_use, json_mode | 3.1B | ollama | 2 |
| `qwen2.5-coder-7b-instruct` | Qwen2.5 Coder 7B Instruct | — | 32,768 | chat, tool_use, json_mode | 7.6B | ollama | 2 |
| `qwen3.5-0.8b` | Qwen3.5 0.8B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 0.8B | ollama | 1 |
| `qwen3.5-27b` | Qwen3.5 27B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 1 |
| `qwen3.5-2b` | Qwen3.5 2B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 2B | ollama | 1 |
| `qwen3.5-4b` | Qwen3.5 4B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 4B | ollama | 1 |
| `qwen3.5-9b` | Qwen3.5 9B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 9B | ollama | 1 |
| `qwen3.6-27b` | Qwen3.6 27B (Dense, Hybrid Linear+Full Attention) | `waired/dense-large` | 262,144 | chat, tool_use, json_mode | 27B | ollama | 3 |

**MoE（総 / 活性）**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `gpt-oss-120b` | OpenAI gpt-oss 120B (MXFP4) | `waired/flagship` | 131,072 | chat, tool_use, json_mode | 116.8B / A5.1B | vllm | 2 |
| `gpt-oss-20b` | OpenAI gpt-oss 20B (MXFP4) | `waired/oss-small` | 131,072 | chat, tool_use, json_mode | 20.9B / A3.6B | ollama | 2 |
| `qwen3-coder-30b-a3b-instruct` | Qwen3 Coder 30B-A3B Instruct (MoE) | `waired/moe-small` | 262,144 | chat, tool_use, json_mode | 30.5B / A3.3B | ollama | 2 |
| `qwen3-coder-480b-a35b-instruct` | Qwen3 Coder 480B-A35B Instruct (MoE) | `waired/moe-large` | 262,144 | chat, tool_use, json_mode | 480B / A35B | ollama | 2 |
| `qwen3-coder-next-80b-a3b-instruct` | Qwen3 Coder Next 80B-A3B Instruct (Hybrid Mamba) | `waired/moe-mid` | 262,144 | chat, tool_use, json_mode | 80.1B / A3.3B | vllm | 3 |
| `qwen3.5-122b-a10b` | Qwen3.5 122B-A10B (MoE) (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 122B / A10B | ollama | 1 |
| `qwen3.5-35b-a3b` | Qwen3.5 35B-A3B (MoE) (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 1 |
| `qwen3.6-35b-a3b` | Qwen3.6 35B-A3B (MoE, Hybrid Linear+Full Attention) | `waired/moe-coding` | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 2 |

#### vLLM 経路（NVIDIA / AMD GPU サーバ）

**Dense**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen2.5-coder-14b-instruct` | Qwen2.5 Coder 14B Instruct | `waired/medium` | 32,768 | chat, tool_use, json_mode | 14.7B | ollama | 2 |
| `qwen2.5-coder-3b-instruct` | Qwen2.5 Coder 3B Instruct | `waired/small` | 32,768 | chat, tool_use, json_mode | 3.1B | ollama | 2 |
| `qwen2.5-coder-7b-instruct` | Qwen2.5 Coder 7B Instruct | — | 32,768 | chat, tool_use, json_mode | 7.6B | ollama | 2 |
| `qwen3.6-27b` | Qwen3.6 27B (Dense, Hybrid Linear+Full Attention) | `waired/dense-large` | 262,144 | chat, tool_use, json_mode | 27B | ollama | 3 |

**MoE（総 / 活性）**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `deepseek-v4-flash` | DeepSeek-V4-Flash 284B-A13B (MoE, MIT, 1M context) | `waired/moe-dual-gpu` | 1,048,576 | chat, tool_use, json_mode | 284B / A13B | vllm | 1 |
| `glm-4.5-air-106b-a12b` | GLM-4.5-Air 106B-A12B (MoE, MIT) | `waired/moe-mit` | 131,072 | chat, tool_use, json_mode | 106B / A12B | vllm | 1 |
| `glm-5.2` | GLM-5.2 744B-A40B (MoE, MIT, 1M context) | `waired/moe-frontier` | 1,048,576 | chat, tool_use, json_mode | 744B / A40B | vllm | 2 |
| `gpt-oss-120b` | OpenAI gpt-oss 120B (MXFP4) | `waired/flagship` | 131,072 | chat, tool_use, json_mode | 116.8B / A5.1B | vllm | 2 |
| `gpt-oss-20b` | OpenAI gpt-oss 20B (MXFP4) | `waired/oss-small` | 131,072 | chat, tool_use, json_mode | 20.9B / A3.6B | ollama | 2 |
| `qwen3-coder-30b-a3b-instruct` | Qwen3 Coder 30B-A3B Instruct (MoE) | `waired/moe-small` | 262,144 | chat, tool_use, json_mode | 30.5B / A3.3B | ollama | 2 |
| `qwen3-coder-480b-a35b-instruct` | Qwen3 Coder 480B-A35B Instruct (MoE) | `waired/moe-large` | 262,144 | chat, tool_use, json_mode | 480B / A35B | ollama | 2 |
| `qwen3-coder-next-80b-a3b-instruct` | Qwen3 Coder Next 80B-A3B Instruct (Hybrid Mamba) | `waired/moe-mid` | 262,144 | chat, tool_use, json_mode | 80.1B / A3.3B | vllm | 3 |

### 全バリアント（数値）

vendor_support の状態略号: `S`=stable / `E`=experimental / `C`=community / `×`=unsupported。weight GB は概算（`estimated_weight_gb`）、min VRAM は vLLM 経路、min RAM は ollama 経路の下限。数値の導出根拠は dev-docs の「推論層」と `internal/catalog/scoring/` を参照。

#### Ollama 経路（Mac / Windows / CPU / 内蔵・低VRAM GPU）

**Dense**

| model_id | variant | format | quant | runtime | 品質 | 量子 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/活性） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen2.5-coder-0.5b-instruct` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 10 | 4 | 0.4 | 2 | — | 0.5B | gqa | 12,288 | nv:ollama=S · amd:ollama=S · mac:ollama=S | ollama:qwen2.5-coder:0.5b-instruct-q4_K_M | — |
| `qwen2.5-coder-14b-instruct` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 55 | 4 | 9.0 | 16 | — | 14.7B | gqa | 196,608 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen2.5-coder:14b-instruct-q4_K_M | — |
| `qwen2.5-coder-3b-instruct` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 30 | 4 | 2.0 | 4 | — | 3.1B | gqa | 36,864 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen2.5-coder:3b-instruct-q4_K_M | — |
| `qwen2.5-coder-7b-instruct` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 45 | 4 | 4.7 | 8 | — | 7.6B | gqa | 28,672 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen2.5-coder:7b-instruct-q4_K_M | — |
| `qwen3.5-0.8b` | `q8-gguf` | ollama-tag | Q8_0 | ollama | 12 | 6 | 1.0 | 2 | — | 0.8B | hybrid_mamba | 12,288 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:0.8b-q8_0 | — |
| `qwen3.5-27b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 69 | 4 | 17.0 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:27b-q4_K_M | — |
| `qwen3.5-2b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 27 | 4 | 1.9 | 4 | — | 2B | hybrid_mamba | 12,288 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:2b-q4_K_M | — |
| `qwen3.5-4b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 42 | 4 | 3.4 | 8 | — | 4B | hybrid_mamba | 12,288 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:4b-q4_K_M | — |
| `qwen3.5-9b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 52 | 4 | 6.6 | 12 | — | 9B | hybrid_mamba | 32,768 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:9b-q4_K_M | — |
| `qwen3.6-27b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 71 | 4 | 18.0 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:27b-mtp-q4_K_M | 0.30.0 |
| `qwen3.6-27b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 70 | 4 | 16.3 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:27b-q4_K_M | — |

**MoE（総 / 活性）**

| model_id | variant | format | quant | runtime | 品質 | 量子 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/活性） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `gpt-oss-120b` | `mxfp4-gguf` | ollama-tag | MXFP4 | ollama | 85 | 4 | 62.0 | 96 | — | 116.8B / A5.1B | sliding_window | 98,304 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=E | ollama:gpt-oss:120b | — |
| `gpt-oss-20b` | `mxfp4-gguf` | ollama-tag | MXFP4 | ollama | 60 | 4 | 14.0 | 16 | — | 20.9B / A3.6B | sliding_window | 73,728 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:gpt-oss:20b | — |
| `qwen3-coder-30b-a3b-instruct` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 65 | 4 | 18.4 | 32 | — | 30.5B / A3.3B | gqa | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3-coder:30b-a3b-q4_K_M | — |
| `qwen3-coder-480b-a35b-instruct` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 92 | 4 | 290.0 | 320 | — | 480B / A35B | gqa | 122,880 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3-coder:480b-a35b-q4_K_M | — |
| `qwen3-coder-next-80b-a3b-instruct` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 86 | 4 | 48.5 | 56 | — | 80.1B / A3.3B | hybrid_mamba | 24,576 | nv:ollama=S · amd:ollama=S · mac:ollama=C | ollama:hf.co/unsloth/Qwen3-Coder-Next-GGUF:Q4_K_M | — |
| `qwen3.5-122b-a10b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 83 | 4 | 81.0 | 128 | — | 122B / A10B | hybrid_mamba | 24,576 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:122b-a10b-q4_K_M | — |
| `qwen3.5-35b-a3b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 73 | 4 | 24.0 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:35b-a3b-q4_K_M | — |
| `qwen3.6-35b-a3b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 90 | 4 | 22.6 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:35b-a3b-mtp-q4_K_M | 0.30.0 |
| `qwen3.6-35b-a3b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 89 | 4 | 23.9 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:35b-a3b-q4_K_M | — |

#### vLLM 経路（NVIDIA / AMD GPU サーバ）

**Dense**

| model_id | variant | format | quant | runtime | 品質 | 量子 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/活性） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen2.5-coder-14b-instruct` | `awq-int4` | safetensors | AWQ-int4 | vllm | 58 | 4 | 10.0 | — | 16,000 | 14.7B | gqa | 196,608 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen2.5-Coder-14B-Instruct-AWQ | — |
| `qwen2.5-coder-3b-instruct` | `awq-int4` | safetensors | AWQ-int4 | vllm | 31 | 4 | 2.2 | — | 4,096 | 3.1B | gqa | 36,864 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen2.5-Coder-3B-Instruct-AWQ | — |
| `qwen2.5-coder-7b-instruct` | `awq-int4` | safetensors | AWQ-int4 | vllm | 50 | 4 | 5.5 | — | 8,000 | 7.6B | gqa | 28,672 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen2.5-Coder-7B-Instruct-AWQ | — |
| `qwen3.6-27b` | `awq-int4` | safetensors | AWQ-int4 | vllm | 72 | 4 | 17.0 | — | 24,000 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.6-27B-AWQ | — |

**MoE（総 / 活性）**

| model_id | variant | format | quant | runtime | 品質 | 量子 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/活性） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `deepseek-v4-flash` | `fp8-safetensors` | safetensors | FP8 | vllm | 93 | 8 | 160.0 | — | 196,608 | 284B / A13B | mla | 124,928 | nv:vllm=S · amd:vllm=E · mac:ollama=×,mlx=× | hf:deepseek-ai/DeepSeek-V4-Flash | — |
| `glm-4.5-air-106b-a12b` | `fp8-safetensors` | safetensors | FP8 | vllm | 75 | 4 | 108.0 | — | 120,000 | 106B / A12B | standard | 188,416 | nv:vllm=S · amd:vllm=E · mac:ollama=×,mlx=× | hf:zai-org/GLM-4.5-Air | — |
| `glm-5.2` | `fp8-safetensors` | safetensors | FP8 | vllm | 97 | 8 | 755.0 | — | 1,130,000 | 744B / A40B | mla | 89,856 | nv:vllm=S · amd:vllm=E · mac:ollama=×,mlx=× | hf:zai-org/GLM-5.2-FP8 | — |
| `glm-5.2` | `nvfp4-safetensors` | safetensors | NVFP4 | vllm | 96 | 4 | 465.0 | — | 560,000 | 744B / A40B | mla | 89,856 | nv:vllm=S · amd:vllm=× · mac:ollama=×,mlx=× | hf:nvidia/GLM-5.2-NVFP4 | — |
| `gpt-oss-120b` | `mxfp4-safetensors` | safetensors | MXFP4 | vllm | 88 | 4 | 62.0 | — | 80,000 | 116.8B / A5.1B | sliding_window | 98,304 | nv:vllm=S · amd:vllm=E · mac:mlx=E | hf:openai/gpt-oss-120b | — |
| `gpt-oss-20b` | `mxfp4-safetensors` | safetensors | MXFP4 | vllm | 62 | 4 | 14.0 | — | 20,000 | 20.9B / A3.6B | sliding_window | 73,728 | nv:vllm=S · amd:vllm=E · mac:mlx=E | hf:openai/gpt-oss-20b | — |
| `qwen3-coder-30b-a3b-instruct` | `awq-int4` | safetensors | AWQ-int4 | vllm | 68 | 4 | 20.0 | — | 24,000 | 30.5B / A3.3B | gqa | 65,536 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3-Coder-30B-A3B-Instruct-AWQ | — |
| `qwen3-coder-480b-a35b-instruct` | `fp8-safetensors` | safetensors | FP8 | vllm | 95 | 4 | 480.0 | — | 560,000 | 480B / A35B | gqa | 122,880 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8 | — |
| `qwen3-coder-next-80b-a3b-instruct` | `fp16-safetensors` | safetensors | bfloat16 | vllm | 80 | 8 | 160.0 | — | 180,000 | 80.1B / A3.3B | hybrid_mamba | 24,576 | nv:vllm=S · amd:vllm=E · mac:mlx=C | hf:Qwen/Qwen3-Next-80B-A3B-Instruct | — |
| `qwen3-coder-next-80b-a3b-instruct` | `awq-int4` | safetensors | AWQ-int4 | vllm | 82 | 4 | 48.0 | — | 56,000 | 80.1B / A3.3B | hybrid_mamba | 24,576 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3-Next-80B-A3B-Instruct-AWQ | — |

<!-- 自動生成セクションここまで。編集は `catalog-tool docs` 経由で。 -->
<!-- END GENERATED: catalog-tool docs -->

## モデルの追加・更新

新しいモデルを bundled に加える流れ（詳細は monorepo dev-docs の「CI/CD & リリース」catalog-radar 節）:

1. `catalog-tool radar` が HuggingFace を走査して候補を surface（週次 `catalog-radar.yml`、#413）。
2. `catalog-tool compute` / `tier` / `draft` が VRAM/KV/FLOPs と `quality_tier` を**決定論的に**算出し、`internal/catalog/bundled/<id>.json` の manifest を組み立てる。
3. `catalog-tool validate --all` が manifest 妥当性 + catalog 全体での `quality_tier` 一意性を検査。
4. `catalog-tool docs`（= `make catalog-docs`）が本ページの生成ブロックを更新。
5. bot は **draft PR** を開くだけで自動マージはしない。GPU レーンの検証 + 人手レビューを経てマージ。

数値は手計算せず常に `catalog-tool` が再導出する設計のため、本ページの表もコミットに含めれば実装と乖離しない。

## 関連ページ

- dev-docs「推論層 (Inference)」 — Router / Catalog / Runtime / Auto Selector
- dev-docs「コーディングエージェント連携」 — `waired/default` 等の別名解決
- dev-docs「CI/CD & リリース」 — catalog-radar パイプライン
- dev-docs「パラメータ」/「ポート一覧」
