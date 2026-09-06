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

同梱: **15 ファミリ / 22 バリアント**。

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
| `qwen3.5-0.8b` | Qwen3.5 0.8B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 0.8B | ollama | 1 |
| `qwen3.5-27b` | Qwen3.5 27B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 1 |
| `qwen3.5-2b` | Qwen3.5 2B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 2B | ollama | 1 |
| `qwen3.5-4b` | Qwen3.5 4B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 4B | ollama | 1 |
| `qwen3.5-9b` | Qwen3.5 9B (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 9B | ollama | 1 |
| `qwen3.6-27b` | Qwen3.6 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 3 |
| `qwen3.8-27b` | Qwen3.8 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 2 |

**MoE（総 / アクティブ）**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `gpt-oss-120b` | OpenAI gpt-oss 120B (MXFP4) | — | 131,072 | chat, tool_use, json_mode | 116.8B / A5.1B | vllm | 2 |
| `gpt-oss-20b` | OpenAI gpt-oss 20B (MXFP4) | — | 131,072 | chat, tool_use, json_mode | 20.9B / A3.6B | ollama | 2 |
| `qwen3.5-122b-a10b` | Qwen3.5 122B-A10B (MoE) (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 122B / A10B | ollama | 1 |
| `qwen3.5-35b-a3b` | Qwen3.5 35B-A3B (MoE) (Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 1 |
| `qwen3.6-35b-a3b` | Qwen3.6 35B-A3B (MoE, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 35B / A3.3B | ollama | 2 |
| `qwen3.8-flash-next` | Qwen3.8 Flash Next (180B-A6B, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 180B / A6B | ollama | 1 |

#### vLLM で動かす場合（NVIDIA / AMD GPU サーバ）

**Dense**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.6-27b` | Qwen3.6 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 3 |
| `qwen3.8-27b` | Qwen3.8 27B (Dense, Hybrid Linear+Full Attention) | — | 262,144 | chat, tool_use, json_mode | 27B | ollama | 2 |

**MoE（総 / アクティブ）**

| model_id | 表示名 | waired 別名 | context | capabilities | パラメータ | preferred | variants |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `deepseek-v4-flash` | DeepSeek-V4-Flash 284B-A13B (MoE, MIT, 1M context) | — | 1,048,576 | chat, tool_use, json_mode | 284B / A13B | vllm | 1 |
| `glm-5.2` | GLM-5.2 744B-A40B (MoE, MIT, 1M context) | — | 1,048,576 | chat, tool_use, json_mode | 744B / A40B | vllm | 2 |
| `gpt-oss-120b` | OpenAI gpt-oss 120B (MXFP4) | — | 131,072 | chat, tool_use, json_mode | 116.8B / A5.1B | vllm | 2 |
| `gpt-oss-20b` | OpenAI gpt-oss 20B (MXFP4) | — | 131,072 | chat, tool_use, json_mode | 20.9B / A3.6B | ollama | 2 |

### 全バリアント（数値）

vendor_support の状態略号: `S`=stable / `E`=experimental / `C`=community / `×`=unsupported。weight GB は概算（`estimated_weight_gb`）、min VRAM は vLLM で動かす場合、min RAM は ollama で動かす場合の下限。数値の導出根拠は dev-docs の「推論層」と `internal/catalog/scoring/` を参照。

#### Ollama で動かす場合（Mac / Windows / CPU / 内蔵・低VRAM GPU）

**Dense**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.5-0.8b` | `q8-gguf` | ollama-tag | Q8_0 | ollama | 12 | 6 | 1.0 | 2 | — | 0.8B | hybrid_mamba | 12,288 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:0.8b-q8_0 | — |
| `qwen3.5-27b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 67 | 4 | 17.0 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:27b-q4_K_M | — |
| `qwen3.5-2b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 27 | 4 | 1.9 | 4 | — | 2B | hybrid_mamba | 12,288 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:2b-q4_K_M | — |
| `qwen3.5-4b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 42 | 4 | 3.4 | 8 | — | 4B | hybrid_mamba | 32,768 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:4b-q4_K_M | — |
| `qwen3.5-9b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 52 | 4 | 6.6 | 12 | — | 9B | hybrid_mamba | 32,768 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:9b-q4_K_M | — |
| `qwen3.6-27b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 69 | 4 | 18.0 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:27b-mtp-q4_K_M | 0.30.0 |
| `qwen3.6-27b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 68 | 4 | 16.3 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:27b-q4_K_M | — |
| `qwen3.8-27b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 71 | 4 | 17.7 | 24 | — | 27B | hybrid_mamba | 65,536 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.8:27b-mtp-q4_K_M | 0.32.13 |

**MoE（総 / アクティブ）**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `gpt-oss-120b` | `mxfp4-gguf` | ollama-tag | MXFP4 | ollama | 85 | 4 | 62.0 | 96 | — | 116.8B / A5.1B | sliding_window | 98,304 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=E | ollama:gpt-oss:120b | — |
| `gpt-oss-20b` | `mxfp4-gguf` | ollama-tag | MXFP4 | ollama | 60 | 4 | 14.0 | 16 | — | 20.9B / A3.6B | sliding_window | 73,728 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:gpt-oss:20b | — |
| `qwen3.5-122b-a10b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 83 | 4 | 81.0 | 128 | — | 122B / A10B | hybrid_mamba | 24,576 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:122b-a10b-q4_K_M | — |
| `qwen3.5-35b-a3b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 73 | 4 | 24.0 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.5:35b-a3b-q4_K_M | — |
| `qwen3.6-35b-a3b` | `mtp-q4-gguf` | ollama-tag | Q4_K_M | ollama | 90 | 4 | 22.6 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:35b-a3b-mtp-q4_K_M | 0.30.0 |
| `qwen3.6-35b-a3b` | `q4-gguf` | ollama-tag | Q4_K_M | ollama | 89 | 4 | 23.9 | 32 | — | 35B / A3.3B | hybrid_mamba | 20,480 | nv:ollama=S,vllm=S · amd:ollama=S,vllm=E · mac:ollama=S,mlx=S | ollama:qwen3.6:35b-a3b-q4_K_M | — |
| `qwen3.8-flash-next` | `q2-gguf` | ollama-tag | UD-Q2_K_XL | ollama | 91 | 2 | 55.1 | 128 | — | 180B / A6B | hybrid_mamba | 27,648 | nv:ollama=S · amd:ollama=S · mac:ollama=S | ollama:frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL | 0.33.3 |

#### vLLM で動かす場合（NVIDIA / AMD GPU サーバ）

**Dense**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `qwen3.6-27b` | `fp8` | safetensors | FP8 | vllm | 70 | 8 | 30.9 | — | 38,912 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.6-27B-FP8 | — |
| `qwen3.8-27b` | `fp8` | safetensors | FP8 | vllm | 72 | 8 | 30.9 | — | 38,912 | 27B | hybrid_mamba | 65,536 | nv:vllm=S · amd:vllm=E · mac:mlx=× | hf:Qwen/Qwen3.8-27B-FP8 | — |

**MoE（総 / アクティブ）**

| model_id | variant | format | quant | runtime | 品質 | 量子化 | weight GB | min RAM GB | min VRAM MB | パラメータ（総/アクティブ） | attn | KV B/tok | vendor_support | source | min engine |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `deepseek-v4-flash` | `fp8-safetensors` | safetensors | FP8 | vllm | 93 | 8 | 160.0 | — | 196,608 | 284B / A13B | mla | 124,928 | nv:vllm=S · amd:vllm=E · mac:ollama=×,mlx=× | hf:deepseek-ai/DeepSeek-V4-Flash | — |
| `glm-5.2` | `fp8-safetensors` | safetensors | FP8 | vllm | 97 | 8 | 755.0 | — | 1,130,000 | 744B / A40B | mla | 89,856 | nv:vllm=S · amd:vllm=E · mac:ollama=×,mlx=× | hf:zai-org/GLM-5.2-FP8 | — |
| `glm-5.2` | `nvfp4-safetensors` | safetensors | NVFP4 | vllm | 96 | 4 | 465.0 | — | 560,000 | 744B / A40B | mla | 89,856 | nv:vllm=S · amd:vllm=× · mac:ollama=×,mlx=× | hf:nvidia/GLM-5.2-NVFP4 | — |
| `gpt-oss-120b` | `mxfp4-safetensors` | safetensors | MXFP4 | vllm | 88 | 4 | 62.0 | — | 80,000 | 116.8B / A5.1B | sliding_window | 98,304 | nv:vllm=S · amd:vllm=E · mac:mlx=E | hf:openai/gpt-oss-120b | — |
| `gpt-oss-20b` | `mxfp4-safetensors` | safetensors | MXFP4 | vllm | 62 | 4 | 14.0 | — | 20,000 | 20.9B / A3.6B | sliding_window | 73,728 | nv:vllm=S · amd:vllm=E · mac:mlx=E | hf:openai/gpt-oss-20b | — |

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
