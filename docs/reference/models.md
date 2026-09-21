# モデルカタログ（提供モデル一覧）

Waired が同梱するローカル LLM の一覧。エイリアス、ファミリ概要、全バリアントの数値（量子化・VRAM/RAM 下限・品質スコア・vendor support）を `proto/catalog/bundled` から自動生成する。

このページは Waired のエージェントが**標準で扱えるモデル**の一覧である。「どのモデルが用意されているか」「`waired/default` が実際にどのモデルへ解決されるか」を一望できる。

- 下表の **品質** 列（`quality_tier`）は **maintainer 向けの序列**であり、製品面には出さない（#537）。ユーザーが見るのは `small` / `medium` / `large` のサイズクラスで、これは `proto/hostfit.ModelSize` が重み注記から導出する。
- 一覧の単一の情報源（source of truth）は `proto/catalog/bundled/*.json`（バイナリに `//go:embed` される）。型は `internal/catalog/manifest.go` の `Manifest` / `Variant`。
- 下表は `catalog-tool docs`（`cmd/catalog-tool/docs.go`）が bundled manifest から**自動生成**する。`<!-- BEGIN GENERATED ... -->` / `<!-- END GENERATED ... -->` の間だけが生成対象で、その外側の本文は手書き。
- 生成物の同期チェック: bundled JSON を変更したのに本ページを再生成し忘れると CI（`catalog-tool docs --check`）が落ちる。週次の catalog-radar（monorepo #413、.github/workflows/catalog-radar.yml）が出す draft PR も同じ手順で本ページを更新する。手で表を編集しないこと。

コーディングエージェントが提示するエイリアスは `waired/default`（コーディング既定）の 1 つ。**旧 `waired/auto` は #422/#478 で `waired/default` に改称済み**、**`waired/coding` / `waired/small` は #521 で退役**（前者は `waired/default` と同一解決、後者は退役する世代を指していた）。いずれも openclaw 側の `legacyModelRefs()` が re-link 時にユーザー設定から削除する。既定以外のモデルは model_id で直接指名する。

**表の構成**: 「ファミリ概要」「全バリアント（数値）」はいずれも **エンジン（Ollama / vLLM）→ アーキテクチャ（Dense → MoE）** で分割する。エンジン（`runtime_support`）はバリアント単位なので、両エンジン向けのビルドを持つファミリは Ollama 節と vLLM 節の両方に再掲される（自分のハードに対応する節だけ読めばよい）。Dense / MoE はファミリ単位（`active_params`）で、Dense=毎トークン全パラメータを計算するため計算 / VRAM に余裕がある環境向き、MoE=総サイズは大きいがアクティブパラメータが少なく、大容量のユニファイドメモリを積んだマシン（Apple Silicon・Strix Halo）向き。エンジン自動判定の規則は dev-docs の「推論層 → engine picker」を参照。

モデルの**選び方**（ハードウェア要件との適合判定・自動選択・ピア間フォールバック・品質スコアの算出）は dev-docs の「推論層」を、コーディングエージェントから別名で叩く仕組みは「コーディングエージェント連携」を参照。

## bundled カタログ

<!-- BEGIN GENERATED: catalog-tool docs -->

> この節は `proto/catalog/bundled/*.json` から `catalog-tool docs` が自動生成する。**手で編集しない** — モデルを追加・更新したら `make catalog-docs`（または `catalog-tool docs`）で再生成してコミットする。catalog-radar（#413）の自動更新も同じ手順で再生成する。空欄は `—`。

同梱: **11 ファミリ / 31 バリアント**。

ファミリ概要・全バリアント表は **エンジン（Ollama / vLLM）→ アーキテクチャ（Dense → MoE）** で分割する。エンジンはバリアント単位（`runtime_support`）なので、両エンジン向けにビルドを持つファミリは両節に再掲される。Dense=全パラメータが毎トークン計算（計算 / VRAM 余裕がある環境向き）、MoE=総サイズは大きいがアクティブパラメータが少ない（大容量のユニファイドメモリを積んだマシン向き・デコード高速）。

### エイリアス

コーディングエージェント連携が提示する 1 つのエイリアスと、それが解決する bundled モデル。

| エイリアス | 解決先 model_id | 表示名 |
| --- | --- | --- |
| `waired/default` | 動的: このホストの既定コーディングモデル（ユーザー指定 > 起動中のモデル > 同梱既定 の順で解決） |  |

### ファミリ概要

#### Ollama で動かす場合（Mac / Windows / CPU / 内蔵・低VRAM GPU）

**Dense**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-0.8b` | Qwen3.5 0.8B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 0.8B | ollama | 3 |
| `qwen3.5-27b` | Qwen3.5 27B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 2 |
| `qwen3.5-2b` | Qwen3.5 2B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 2B | ollama | 3 |
| `qwen3.5-4b` | Qwen3.5 4B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 4B | ollama | 3 |
| `qwen3.5-9b` | Qwen3.5 9B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 9B | ollama | 2 |
| `qwen3.6-27b` | Qwen3.6 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 4 |
| `qwen3.8-27b` | Qwen3.8 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 5 |

**MoE（総 / アクティブ）**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-122b-a10b` | Qwen3.5 122B-A10B (MoE) (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 122B / A10B | ollama | 1 |
| `qwen3.5-35b-a3b` | Qwen3.5 35B-A3B (MoE) (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 2 |
| `qwen3.6-35b-a3b` | Qwen3.6 35B-A3B (MoE, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 5 |
| `qwen3.8-flash-next` | Qwen3.8 Flash Next (177B-A6B, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 176.9B / A6B | ollama | 1 |

#### vLLM で動かす場合（NVIDIA / AMD GPU サーバ）

**Dense**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-0.8b` | Qwen3.5 0.8B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 0.8B | ollama | 3 |
| `qwen3.5-27b` | Qwen3.5 27B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 2 |
| `qwen3.5-2b` | Qwen3.5 2B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 2B | ollama | 3 |
| `qwen3.5-4b` | Qwen3.5 4B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 4B | ollama | 3 |
| `qwen3.5-9b` | Qwen3.5 9B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 9B | ollama | 2 |
| `qwen3.6-27b` | Qwen3.6 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 4 |
| `qwen3.8-27b` | Qwen3.8 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 5 |

**MoE（総 / アクティブ）**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-35b-a3b` | Qwen3.5 35B-A3B (MoE) (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 2 |
| `qwen3.6-35b-a3b` | Qwen3.6 35B-A3B (MoE, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 5 |

### 全バリアント（数値）

vendor_support の状態略号: `S`=stable / `E`=experimental / `C`=community / `×`=unsupported。weight GB は概算（`estimated_weight_gb`）、min VRAM は vLLM で動かす場合、min RAM は ollama で動かす場合の下限。数値の導出根拠は dev-docs の「推論層」と `proto/catalog/scoring/` を参照。

#### Ollama で動かす場合（Mac / Windows / CPU / 内蔵・低VRAM GPU）

**Dense**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-0.8b` | `q8-gguf` | ollama-tag | Q8_0 | ollama | 13 | 6 | 1.0 | 2 | — | 0.8B | hybrid_mamba | 12,288 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:0.8b-q8_0 | — |
| `qwen3.5-27b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 67 | 4 | 17.0 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:27b-q4_K_M | — |
| `qwen3.5-2b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 27 | 4 | 1.9 | 4 | — | 2B | hybrid_mamba | 12,288 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:2b-q4_K_M | — |
| `qwen3.5-4b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 42 | 4 | 3.4 | 8 | — | 4B | hybrid_mamba | 32,768 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:4b-q4_K_M | — |
| `qwen3.5-9b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 52 | 4 | 6.6 | 12 | — | 9B | hybrid_mamba | 32,768 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:9b-q4_K_M | — |
| `qwen3.6-27b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 69 | 4 | 18.0 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:27b-mtp-q4_K_M | 0.30.0 |
| `qwen3.6-27b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 68 | 4 | 17.4 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:27b-q4_K_M | — |
| `qwen3.8-27b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 89 | 4 | 17.7 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.8:27b-mtp-q4_K_M | 0.32.13 |
| `qwen3.8-27b` | `q3-gguf` | ollama-tag | UD-Q3_K_XL | ollama | 87 | 3 | 14.1 | 16 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S · amd:ollama=S · mac:ollama=S | ollama:hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL | 0.33.3 |
| `qwen3.8-27b` | `q2-gguf` | ollama-tag | UD-Q2_K_XL | ollama | 86 | 2 | 10.8 | 12 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S · amd:ollama=S · mac:ollama=S | ollama:hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q2_K_XL | 0.33.3 |

**MoE（総 / アクティブ）**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-122b-a10b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 78 | 4 | 81.0 | 128 | — | 122B / A10B | hybrid_mamba | 24,576 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:122b-a10b-q4_K_M | — |
| `qwen3.5-35b-a3b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 73 | 4 | 24.0 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:35b-a3b-q4_K_M | — |
| `qwen3.6-35b-a3b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 82 | 4 | 22.6 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:35b-a3b-mtp-q4_K_M | 0.30.0 |
| `qwen3.6-35b-a3b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 81 | 4 | 23.9 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:35b-a3b-q4_K_M | — |
| `qwen3.6-35b-a3b` | `mtp-q3-gguf` | ollama-tag | UD-Q3_K_XL | ollama | 80 | 3 | 18.1 | 24 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S · amd:ollama=S · mac:ollama=S | ollama:hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF:UD-Q3_K_XL | 0.33.3 |
| `qwen3.6-35b-a3b` | `mtp-q2-gguf` | ollama-tag | UD-Q2_K_XL | ollama | 79 | 2 | 13.5 | 16 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S · amd:ollama=S · mac:ollama=S | ollama:hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF:UD-Q2_K_XL | 0.33.3 |
| `qwen3.8-flash-next` | `q2-gguf` | ollama-tag | UD-Q2_K_XL | ollama | 91 | 2 | 79.8 | 128 | — | 176.9B / A6B | hybrid_mamba | 27,648 | nv:ollama=S · amd:ollama=S · mac:ollama=S | ollama:frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL | 0.33.3 |

#### vLLM で動かす場合（NVIDIA / AMD GPU サーバ）

**Dense**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-0.8b` | `bf16` | safetensors | BF16 | vllm | 14 | 8 | 1.8 | — | 8,192 | 0.8B | hybrid_mamba | 12,288 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.5-0.8B | — |
| `qwen3.5-0.8b` | `w4a16` | safetensors | W4A16 | vllm | 12 | 4 | 1.1 | — | 7,168 | 0.8B | hybrid_mamba | 12,288 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:kaitchup/Qwen3.5-0.8B-autoround-W4A16 | — |
| `qwen3.5-27b` | `w4a16` | safetensors | W4A16 | vllm | 66 | 4 | 20.1 | — | 37,888 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:cyankiwi/Qwen3.5-27B-AWQ-4bit | — |
| `qwen3.5-2b` | `bf16` | safetensors | BF16 | vllm | 28 | 8 | 4.5 | — | 12,288 | 2B | hybrid_mamba | 12,288 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.5-2B | — |
| `qwen3.5-2b` | `w4a16` | safetensors | W4A16 | vllm | 26 | 4 | 2.5 | — | 9,216 | 2B | hybrid_mamba | 12,288 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:cyankiwi/Qwen3.5-2B-AWQ-4bit | — |
| `qwen3.5-4b` | `bf16` | safetensors | BF16 | vllm | 43 | 8 | 9.3 | — | 20,480 | 4B | hybrid_mamba | 32,768 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.5-4B | — |
| `qwen3.5-4b` | `w4a16` | safetensors | W4A16 | vllm | 41 | 4 | 5.5 | — | 15,360 | 4B | hybrid_mamba | 32,768 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:RedHatAI/Qwen3.5-4B-quantized.w4a16 | — |
| `qwen3.5-9b` | `w4a16` | safetensors | W4A16 | vllm | 53 | 4 | 11.4 | — | 23,552 | 9B | hybrid_mamba | 32,768 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:RedHatAI/Qwen3.5-9B-quantized.w4a16 | — |
| `qwen3.6-27b` | `fp8` | safetensors | FP8 | vllm | 71 | 8 | 30.9 | — | 52,224 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.6-27B-FP8 | — |
| `qwen3.6-27b` | `nvfp4` | safetensors | NVFP4 | vllm | 70 | 4 | 21.9 | — | 39,936 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=× · mac:mlx=× | hf:nvidia/Qwen3.6-27B-NVFP4 | — |
| `qwen3.8-27b` | `fp8` | safetensors | FP8 | vllm | 90 | 8 | 30.9 | — | 52,224 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.8-27B-FP8 | — |
| `qwen3.8-27b` | `nvfp4` | safetensors | NVFP4 | vllm | 88 | 4 | 21.9 | — | 39,936 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=× · mac:mlx=× | hf:nvidia/Qwen3.8-27B-NVFP4 | — |

**MoE（総 / アクティブ）**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-35b-a3b` | `gptq-int4` | safetensors | GPTQ-int4 | vllm | 74 | 4 | 24.4 | — | 38,912 | 35B / A3.3B | hybrid_mamba | 20,480 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.5-35B-A3B-GPTQ-Int4 | — |
| `qwen3.6-35b-a3b` | `nvfp4` | safetensors | NVFP4 | vllm | 83 | 4 | 23.4 | — | 36,864 | 35B / A3.3B | hybrid_mamba | 20,480 | nv:vllm=S · amd:vllm=× · mac:mlx=× | hf:nvidia/Qwen3.6-35B-A3B-NVFP4 | — |

<!-- 自動生成セクションここまで。編集は `catalog-tool docs` 経由で。 -->
<!-- END GENERATED: catalog-tool docs -->

## モデルの追加・更新

新しいモデルを bundled に加える流れ（詳細は monorepo dev-docs の「CI/CD & リリース」catalog-radar 節）:

1. `catalog-tool radar` が HuggingFace を走査して候補を洗い出す（週次 `catalog-radar.yml`、#413）。
2. `catalog-tool compute` / `tier` / `draft` が VRAM/KV/FLOPs と `quality_tier` を**決定論的に**算出し、`proto/catalog/bundled/<id>.json` の manifest を組み立てる。
3. `catalog-tool validate --all` が manifest 妥当性 + catalog 全体での `quality_tier` 一意性を検査。
4. `catalog-tool docs`（= `make catalog-docs`）が本ページの生成ブロックを更新。
5. bot は **draft PR** を開くだけで自動マージはしない。GPU を使う CI ジョブでの検証 + 人手レビューを経てマージ。

数値は手計算せず常に `catalog-tool` が再導出する設計のため、本ページの表もコミットに含めれば実装と乖離しない。

## 関連ページ

- dev-docs「推論層 (Inference)」 — Router / Catalog / Runtime / Auto Selector
- dev-docs「コーディングエージェント連携」 — `waired/default` 等の別名解決
- dev-docs「CI/CD & リリース」 — catalog-radar パイプライン
- dev-docs「パラメータ」/「ポート一覧」
