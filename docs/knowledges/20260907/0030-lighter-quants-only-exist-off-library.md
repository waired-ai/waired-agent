# 軽い量子化は `library` の外にしか無い (20260907 00:30)

## Issue

カタログ(`proto/catalog/bundled/*.json`)が出荷していた ollama タグは 15 本、
すべて Q4_K_M 以上、すべて公式の `library` namespace のものだった。その結果、
16 GB のディスクリート GPU と 24 GB の Mac では推奨ゲート(`proto/hostfit`、
`ReasonWeightsSpill` / `ReasonWindowExceedsMemory`)が 27B と 35B の全項目を
拒否し、提示できる最良は `qwen3.5-9b` の quality_tier 52 — 27B 帯の下限 67 に
届かない。waired はハードウェアが制約になっている人のための製品なので、
オーナーから「もっと軽い量子化を足せないか、その出所を信頼できる 1 つの
公開者に揃えられないか」と問われた(waired-agent#1265)。

以下はすべて 2026-09-06〜07 にレジストリを直接読んで確かめた事実である。

## Learnings

### 1. 公式 `library` は Q4_K_M より軽い GGUF を 1 本も公開していない

qwen3.5(0.8b / 2b / 4b / 9b / 27b / 35b-a3b / 122b-a10b)、qwen3.6(27b、
35b-a3b、いずれも mtp)、qwen3.8(27b)、gpt-oss(20b、120b)、granite4(350m)の
全タグを列挙した。GGUF の段は `q4_K_M → q8_0 → bf16` だけで、それ以外の
MLX / mxfp8 / nvfp4 は別ランタイム向けであって軽い GGUF ではない。

つまり「軽い量子化」は必然的に `library` を出ることを意味する。このノートで
いちばん役に立つ事実はこれで、次に同じ問いが出たときにレジストリを列挙し直す
必要は無い。

### 2. カタログ全体を 1 つの公開者で覆えるのは `hf.co/unsloth` だけ

`hf.co/unsloth/<Model>-GGUF` は、出荷中の ollama 向けモデルすべて(0.8B から
122B まで、qwen3.6 の MTP 2 本、gpt-oss 2 本)に存在し、リポジトリはどれも
apache-2.0。unsloth の「UD」動的量子化は `frob/` が再梱包しているものの上流
なので、unsloth を指せば再梱包者ではなく出所を指すことになる。

代替はこの軸で劣る。`frob/` は非常に大きいモデルしか公開しておらず、
それ以外にレジストリ検索が返すものは個人の再アップロードかマージである。

### 3. Hub は ollama 互換レジストリを持つが、分割 GGUF は引けない

Hugging Face Hub は同じ `/v2/` パスで ollama 互換のレジストリを認証なしで
提供しており、`ollama pull hf.co/<org>/<repo>:<quant>` は実在する経路である。
ただし分割 GGUF は HTTP 400 で
`This tag is a sharded GGUF. Ollama does not yet support pulling sharded GGUF via the registry`
と答える。Hub はファイル 1 本を 50 GB で打ち切るので、候補になるのはそれ未満の
量子化だけ。具体的には:

| タグ | 応答 |
|---|---|
| `unsloth/Qwen3.5-122B-A10B-GGUF:UD-Q2_K_XL`(41.85 GB) | 200 |
| `unsloth/Qwen3.5-122B-A10B-GGUF:UD-IQ3_S`(46.56 GB) | 200 |
| `unsloth/Qwen3.5-122B-A10B-GGUF:UD-Q3_K_XL`(3 ファイル) | 400 |
| `unsloth/Qwen3.5-122B-A10B-GGUF:Q4_K_M`(3 ファイル) | 400 |

### 4. template レイヤは「render できる」証拠ではない

Hub のタグの config blob は docker 形式で、どのタグにも `renderer` フィールドが
無い。加えて一部のリポジトリは `application/vnd.ollama.image.template` レイヤを
持つ — ここが罠である。`ollamaregistry.Rendering.Renders()` はそのレイヤで
true を返すので #1240 のガードは通るが、`unsloth/Qwen3.8-27B-GGUF` のその
レイヤは 410 バイトの**旧式 3 フィールド ollama テンプレート**
(`{{ if .System }}` / `.Prompt` / `.Response`)で、`.Messages` のループも
ツール呼び出しの扱いも無く、コーディングエージェントの会話を一切 render
できない。

したがって template レイヤの存在を根拠に出荷してはならない。正しい
renderer / parser の値は公式 `library` タグ自身の config blob から読める:

| library タグ | renderer | parser |
|---|---|---|
| qwen3.5 / qwen3.6 の各タグ | `qwen3.5` | `qwen3.5` |
| `qwen3.8:27b-mtp-q4_K_M` | `qwen3.8` | `qwen3.5` |

`internal/catalog/sources_integration_test.go` のガードは、この理由で Hub の
タグが template レイヤに頼ることを拒否する(#1265)。

### 5. 実機: レジストリホスト名付きのローカル名で `ollama create` できる

ホストクラス nvidia-24gb-discrete、素の ollama 0.33.3(waired 無し)で、
`ollama create hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL -f Modelfile`
(`RENDERER` / `PARSER` 行あり)は**成功する**。ollama はレジストリホスト
要素を含むローカルモデル名を受け付け、全 blob を再利用し、その後の config は
renderer=`qwen3.8` parser=`qwen3.5` と読める。モデルストア全体のディスク差分は
-239 バイト。この経路を潰しうる唯一の未知はここだった。

刻印後、request-shape の 6 形(leading-system / no-system / trailing-system /
double-system / system-after-tool-roundtrip / developer-turn)はすべて 200 を
返し、agent grade は greeting / read-file / search-then-edit の 3/3 で pass。
`/api/ps` は size == size_vram == 14.96 GB で、システム RAM には何も載らない。
Q2 側(UD-Q2_K_XL)も 6 形すべて accepted で grade は pass、常駐 9.87 GB。

### 6. Hub のタグを grade するときは `NO_PULL=1`

`make e2e-agentgrade` は `NO_PULL=1` で走らせる。`startStack` が
`puller.Pull(ctx, tag, download.Rendering{}, ...)` を呼び、再 pull はローカル
manifest を公開時の config に書き戻して刻印を消す
(`docs/decisions/20260906/1900-stamp-the-renderer-on-the-pulled-tag.md` に
記録した機構)。刻印が消えた状態で測れば、フリートが実際に serve するもの
ではなく、§4 の旧式テンプレートで刷られたものを測ったことになる。それが
何形の 500 になるかはこのタグでは測っていない — 測る意味が無いからで、
`NO_PULL=1` を落としたときの結果は「刻印済みのタグの測定」ではない、
というのがここで要る事実である。

### 7. 軽くして効くのは「もともと自動選択される帯」だけ

軽い variant を足す前に確かめること: **その帯は今日どこかで自動選択されて
いるか。** されていないなら、軽くしても自動選択は 1 つも動かない。

`qwen3.5-122b-a10b` の UD-Q2_K_XL(41.85 GB)で実際にそうなった。ピッカーに
通すと 48 / 64 / 96 / 128 / 192 GB のどのユニファイドメモリでも選択は変わら
ない。理由は量子化ではなく順位で、122B 帯の quality_tier は 83、
qwen3.6-35b-a3b は 90、qwen3.8-flash-next は 91 で、後の 2 つは 122B が載る
どのホストにも載る。**今日すでに 122B はどこでも自動選択されていない。**

軽くして動くのは別の軸である。「動く」下限が 96 GB → 48 GB、「推奨の対象に
なる」下限が 128 GB → 64 GB に下がる — つまり**名指しでそのモデルを選ぶ人の
到達範囲**。それが欲しい改善なのかどうかは、variant 1 本につき GPU ホストでの
採点 1 回というコストと並べて決めることになる(#1265 では見送った)。

### 8. gpt-oss はこの経路から何も得ない

gpt-oss は MXFP4 ネイティブなので、リポジトリ内のどの量子化も重さが同じ
(20b は一律 ~11.5 GB、120b は ~62.6 GB)。軽い variant を探しても無い。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1265
- docs/decisions/20260907/0030-lighter-builds-come-from-one-upstream.md
- docs/decisions/20260906/1900-stamp-the-renderer-on-the-pulled-tag.md
- docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md
