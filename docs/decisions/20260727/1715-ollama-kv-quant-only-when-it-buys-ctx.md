---
status: accepted
---

# 量子化 KV とフラッシュアテンションは「文脈長を買えるとき」だけ要求する (20260727 17:15)

## Status
Accepted

## Context

`routing-sentinel` が 2026-07-20 以降 6〜8% の確率で赤くなっていた
(waired-agent#29)。真因は ollama の子プロセス `llama-server` の segfault で、
`opencode` レグだけが真実を出していた:

```
HTTP 500 {"error":{"message":"llama-server process has terminated: signal: segmentation fault (core dumped)"}}
```

#621 の serve tuning は、ホストを問わず常に
`OLLAMA_KV_CACHE_TYPE=q8_0` と `OLLAMA_FLASH_ATTENTION=1` を出していた。
FA を強制するのは、量子化 KV がフラッシュアテンション無しでは黙って f16 に
劣化するため。

ところが CI ランナー (CPU のみ、16 GB、GPU なし) で bundled の
qwen2.5-coder-0.5b-instruct を動かすと、この収支は成立していなかった:

| | KV 実サイズ | 予算 12 GB に対して |
|---|---|---|
| `q8_0` | 402 MB | 3.3 % |
| `f16` | 805 MB | 6.7 % |

節約額は約 400 MB / 予算の 3%。f16 でも 943,616 トークン分が入る予算に対し、
実際に使うのは 32768 × 2 スロット = 65,536 トークン (14 倍の余裕)。
**買えている文脈長はゼロ**なのに、対価として llama.cpp で最も実行例の少ない
「CPU + フラッシュアテンション + 量子化 KV」経路を強制していた。

post-load verify の f16 degrade 検出 (`inference_ollama_verify.go`) は
`expF16-expQ8 >= 1.5GB` を要求するため、このモデル (差分 402 MB) では
そもそも発火しない。既存のセーフティネットはこのケースを覆っていなかった。

## Decision

量子化 KV (と、それに付随するフラッシュアテンション) を要求するのは
次のいずれかを満たすときだけとする — `planOllamaKV`:

1. **GPU / UMA 予算があるホスト** (`hw.EffectiveVRAMMB() > 0`)、または
2. **f16 予算では窓とスロットを賄えないホスト**
   (`f16Max < ollamaMaxAutoParallel * want`)

それ以外は `f16` を使い、`OLLAMA_FLASH_ATTENTION` を**出力しない**
(エンジンに選ばせる)。

「CPU なら f16」という単純な規則は採らなかった。`proto/hostfit` の
discrete オーバーヘッド係数 (`OllamaVRAMOverheadBaseDiscreteMB` /
`PerWeightGBMB`) は FA 前提で較正されており、GPU ホストで FA を外すと
spill 予約が黙って壊れる。また GPU 側には post-load verify という
セーフティネットがある。

閾値に `ollamaMaxAutoParallel` (= 2、自動採番するスロット上限) を使うのは
意図的で、**f16 への切替が `ContextLength` も `NumParallel` も変えないことが
証明になる**。

明示的な KV 型指定は「ピン」として扱う。verify pass の f16 degrade も、
下記の nightly override も、この形で意図を表現する。

## Consequences

- CI ランナー相当のホストは `OLLAMA_KV_CACHE_TYPE=f16`、FA なしになる。
  `ContextLength=32768` / `NumParallel=2` は不変。
- GPU / UMA ホストは bit 単位で従来どおり (`gpu-auto-matches-pinned-q8` が固定)。
- 本当に窮屈な CPU ホスト (例: 28 GB 予算で 35B q4) は q8_0 を維持する
  (`cpu-only-uses-ram-budget` が固定)。
- サイジング入力が不明なホストは挙動を変えない (q8_0 のまま)。
- **失われる実エンジンカバレッジ**: ollama を動かす CI レグは全て CPU-only
  (GPU ランナーは vLLM 専用) なので、本変更により CPU + q8_0 + FA の
  実エンジン実行が CI から消える。macOS nightly は UMA なので自動的に維持。
  CPU 側は `IT_AGENT_ENV_EXTRA` + `WAIRED_OLLAMA_KV_CACHE_TYPE=q8_0` を
  nightly の install+inference (linux) レグに置いて**意図的に**残した。
  routing-sentinel レグには置かない — そこは実ユーザーと同じ設定を検証する。
- `OLLAMA_FLASH_ATTENTION` を出さなくなったため、`processEnv` の drop セットを
  「出力するキー」から「tuning キーの集合」に変えた。そうしないと
  `/etc/waired/agent.env` や開発シェルの継承値が、opt-out したはずの組み合わせを
  黙って復活させる。
- segfault そのものが本変更で消える保証はない (llama.cpp 側の不具合であり、
  engine.log による裏付けは waired-agent#29 の診断 PR 待ち)。ただし
  「3% のために最も脆い経路を強制する」構図は、再現の有無と独立に解消される。

## Refs
- waired-ai/waired-agent#29
- `cmd/waired-agent/inference_ollama_tuning.go` (`planOllamaKV`)
- `docs/decisions/20260714/2131-ollama-tuning-verify-per-model.md`
