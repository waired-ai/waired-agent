---
status: accepted
---

# 同時に (V)RAM に載せるモデルは 1 つ — 計測がホストを測るために (20260811 23:40)

## Status

Accepted。オーナー決定（2026-08-10、rc8 実機検証中、waired-agent#644）。
対照実験で因果を固定済み。

## Context

「1 agent = 1 model」の不変条件は、これまで **エージェントが何を広告するか**
（`narrowPublishedModels`）を規定していただけで、**エンジンが何を常駐させるか**
は規定していなかった。serve env は `OLLAMA_CONTEXT_LENGTH` /
`OLLAMA_KV_CACHE_TYPE` / `OLLAMA_NUM_PARALLEL` / `OLLAMA_FLASH_ATTENTION` を
出すが `OLLAMA_MAX_LOADED_MODELS` は出さず、同時常駐はエンジンの既定任せだった。

その結果、インストール時のホスト速度計測が serving モデルの隣で走った。
`bootstrapAfterEngineStart` は `defer p.warmServingModel()` で serving モデルを
`keep_alive=60m` でロードし、計測も warm も非同期で、warm は静穏待ちをしない。
つまり **常駐は偶発ではなく順序として保証されていた**（waired#1139）。

実機（Ubuntu 26.04 / 121 GB RAM / RTX PRO 4000 Blackwell 24 GB、edge `be2d4b3`）で、
同一ホスト・同一 probe・同一手順（`host-speed.json` の `agent_version` を上げて
再起動 = アップグレード再計測の経路そのもの）を数分差で:

| 条件 | 計測中の常駐 | turn_seconds |
|---|---|---|
| `OLLAMA_MAX_LOADED_MODELS=1` | probe のみ | **4.4376** |
| 未設定（従来） | serving 35B + probe | **40.9954** |

このホストで測れた全6値: クリーン導入 4.452 / 4.4925、cap 付き再計測 4.4376、
cap 無し再計測 39.599 / 40.093 / 40.9954。**重なりが無い。**

`/api/ps` + `nvidia-smi` で常駐を追うと、cap 有りでは 35B が退去して probe が
3.66 GB を全て VRAM に載せ（GPU 3993/24467 MiB）、cap 無しでは同居して probe は
3.41 GB 中 0.86 GB しか VRAM に入らなかった（GPU 23954/24467 MiB = 97.9%）。

**probe を小さくする案は実測で否定した。** serving モデルが GPU を占有している間、
probe は文脈長を変えてもスピルする:

| num_ctx | probe サイズ | VRAM | システム RAM |
|---|---|---|---|
| 200,704 | 3.41 GB | 0.86 GB | 74% |
| 24,576 | 1.47 GB | 0.36 GB | 76% |
| 4,096 | 1.14 GB | 0.22 GB | **81%** |

常駐数を絞ることだけが効いた。

## Decision

**エンジンが (V)RAM に載せるモデルは同時に 1 つ**（`infruntime.MaxResidentModels`）。

### 1. 不変条件は engine 非依存の場所に 1 か所だけ書く

`internal/runtime/adapter.go`。エンジンごとに満たし方が違うのであって、
設定項目が違うのではない:

- **ollama** は 1 プロセスで複数モデルを serve するので `OLLAMA_MAX_LOADED_MODELS`
  が要る（`OllamaAdapter.processEnv`）。
- **vLLM** は `--model <one>` で 1 プロセス 1 モデルなので**既に満たしている**
  （`VLLMAdapter.commandArgs`）。設定するものが無い。

### 2. ollama へは `processEnv` から無条件に出す。`ollamaTuning.Env()` ではない

`Env()` は **serve target が解決したときにしか計算されない**。この cap が守ろうと
している計測は、まさに target がまだ無いホスト（新規インストールは untuned な
boot plan で起動する）で走る。エンジン全体の不変条件が、たまたま同時に計算される
per-model tuning の持ち物になってはいけない。

### 3. operator の上書きは残す。ただし明示的に読む

`/etc/waired/agent.env` の行は `os.LookupEnv` で読み、あれば我々は出さない。
`processEnv` は**自分が出したキーを継承環境から drop する**ので、
`ollamaTuningKeys` に入れると operator の値は消える。無条件に append しても
子プロセスの getenv の前に 2 つ並ぶだけで、どちらが勝つかは実装依存になる。
**値を空にセットするのが opt-out**（エンジン既定に戻す）。

### 4. 計測の後に serving モデルを載せ直す

この修正が計測を正しくする仕組みは「probe が常駐モデルを**追い出す**」ことで、
probe は `keep_alive:0` で自分を降ろす。つまり計測が終わった時点で**何も常駐して
いない**。warm-up（waired-agent#320）が存在理由ごと無効化されるので、
`ensureHostSpeedMeasured` は probe を投げる前に `defer p.warmServingModel()` を
積む。single-flight かつ `/api/ps` 先読みなので、何も動いていない場合は安い。

### 5. adopted エンジンは計測を見送る

adopted エンジンは前の run が spawn したもので、**環境は我々のものではない**
（waired-agent#320 が `OLLAMA_KEEP_ALIVE` について記録しているのと同じ限界）。
cap が届かないので probe は何も追い出さない。この場合だけ `/api/ps` を読み、
何か常駐していれば**前の記録を残して次の起動に回す** — `host_memory.go` の
`engineListening` が同じ問いに与えている答えと同じ形。

**`engineIsQuiet` には入れない。** あれは待ちループの条件なので、
`keep_alive=60m` の serving モデルがあるホストは `hostSpeedSettleWait` を
待ち切って永久に測らなくなる。これは待ちではなく検査。

## Consequences

- **アップグレード再計測が汚染されなくなる。** waired#1140 の 3 台
  （24 GB Blackwell / 8 GB 4070 Laptop / Apple 16 GB UMA）が揃って ~40 s =
  余裕 11〜12% に収束していた状態は、次の計測で解消する。
- **`waired init` の再実行で測り直せる**（waired-agent#599 の 1 項目、
  `POST /waired/v1/inference/host-speed/remeasure`）。クリーン計測を一度も
  持っていない Apple 16 GB 機の復旧経路はこれ。新しい CLI 動詞は足さない。
- **mesh 要求が 2 つ目のモデルを求めると serving モデルが退去する。**
  `narrowPublishedModels` が効いていればピアは広告された 1 つしか要求しないので
  起きない。効いていないホストがある件は waired-agent#656（別レーン）。
- 再計測はインストール時とエージェント更新時にしか走らないので、退去のコストは
  稀。実測では 35B が元の footprint（19.97 GB VRAM）にそのまま再ロードされた。
- **adopted エンジンのホストは、常駐がある限り図が更新されない。** 前の記録が
  残るだけで、誤った図が出ることはない。adopted の廃止自体は waired#488。

## References

- waired-ai/waired-agent#644（オーナー決定と対照実験）
- waired-ai/waired-agent#639（症状: 24 GB ワークステーションが 4.45 s → 39.6 s）
- waired-ai/waired#1139（機構: 順序として常駐が保証される）
- waired-ai/waired#1140（結果: 公開値がホストではなく常駐を表す）
- waired-ai/waired-agent#320（warm-up と、adopted エンジンの環境は我々のものではない件）
- `docs/decisions/20260807/1700-host-speed-is-an-install-time-step.md`
  （いつ測るか。§Consequences の「`waired init` に強制再計測の経路は足さない」は、
  waired-agent#599 のオーナー裁定（2026-08-09）により**再実行時に限って更新**された）
- `docs/decisions/20260809/1726-benchmark-yields-to-engine-restarts.md`（静穏ゲートの系譜）
- `docs/knowledges/20260811/2126-real-hardware-verification-method.md`（採取方法）
