# プレフィックス再利用が効くかはモデルの記憶機構で決まる (20260819 23:30)

## Issue

`docs/knowledges/20260819/2130-local-engine-caches-the-prefix.md` で「温まったまま
生き残る大きなプレフィックスは事実上 1 本」と書いたが、**なぜ 1 本なのかを
突き止めていなかった**。ホスト RAM 側のプロンプトキャッシュ (`--cache-ram`、
既定 8192 MiB) が容量不足なのだろう、というのが当初の見立てだった。

実験したところ容量は無関係で、**モデルの記憶機構が原因**だった。そして同じ実験の
対照で、モデルを替えると結果が逆転した。

## Learnings

### 1. 分岐したプロンプトが部分再利用できるかは、モデルの記憶機構で決まる

同一ホスト (24GB discrete GPU)・同一 ollama (0.32.13)・同一の捕捉済みプロンプト 2 本
(A と B は先頭 90.4% が共通、分岐点は position 27,435) で ping-pong した実測:

| | ハイブリッド (`qwen3.5:35b-a3b`) | 密 (`qwen3:8b`) |
|---|---|---|
| A 冷 (フルプレフィル) | 16.26 s | 9.43 s |
| A 再送 (完全一致) | 0.30 s | 0.22 s |
| B (別プロンプト、冷) | 16.19 s | 10.01 s |
| **B の後に A** | **16.40 s** | **1.43 s** |
| その後 B | 16.59 s | 1.42 s |
| その後 A | 16.77 s | 1.42 s |

密モデルは共有している 90% を再利用し、分岐した末尾だけを計算し直す (冷の 15%)。
ハイブリッドは毎回ゼロから計算し直す。

### 2. 理由はエンジンのログに書いてある

```
print_info: n_layer = 40 / n_layer_all = 41
print_info: n_swa   = 0, is_swa_any = 0                       ← SWA ではない
llama_kv_cache:        size = 3920.00 MiB (200704 cells, 10 layers)
llama_memory_recurrent: size =   62.81 MiB (     1 cells, 40 layers)   ← 再帰状態
```

`qwen3.5:35b-a3b` は 40 層中 10 層だけが位置で引ける KV を持ち、残りは**セル数 1 の
再帰状態**を持つ。再帰状態は位置を巻き戻せないので、途中で分岐したプロンプトを
分岐点から再開することが原理的にできない。

失敗はログにそのまま出る:

```
slot get_availabl: selected slot by LCP similarity, f_sim_best = 0.904 (> 0.100 thold)
slot   operator(): checking checkpoint with [30356, 30356] against 27435...
slot   operator(): checking checkpoint with [29848, 29848] against 27435...
slot   operator(): erased invalidated context checkpoint (...)
slot   operator(): forcing full prompt re-processing due to lack of cache data
                   (likely due to SWA or hybrid/recurrent memory)
```

スロットは 90.4% 一致で正しく選ばれている。しかし分岐点 27,435 に使える
チェックポイントが無く (存在するのは 29,848 と 30,356 = どちらも分岐点より後ろ)、
全再計算に落ちる。

**追記だけなら効く**のは、末尾のチェックポイントがそのまま継続点になるため。
コーディングエージェントの継続ターンが速い (0.3〜0.4 秒) のはこの経路。

### 3. 20260705 の記録の 1 点を精密化する

`waired` 側の `docs/knowledges/20260705/1125-ollama-prefix-cache-behavior.md` は
「先頭側が 1 バイトでも変わると**そこから後ろは**全再計算」と書いている。これは
密モデルでは正しいが、**ハイブリッドモデルでは「そこから後ろ」ではなく「全部」**。
当時の計測対象 (`qwen3.6:35b-a3b-mtp`) もハイブリッドだが、追記のケースしか
試験されておらず途中分岐は測られていない。

### 4. 効かなかったノブ 2 つ (と、そこで分かった良い事実)

| ノブ | 子プロセスに届いたか | 効果 |
|---|---|---|
| `LLAMA_ARG_CACHE_RAM=65536` | 届いた (`prompt cache is enabled, size limit: 65536 MiB`) | **なし** (全ラウンド 16.2〜16.8 s、対照と同一) |
| `LLAMA_ARG_CHECKPOINT_MIN_SPACING_NT=2048` | 届いた (`context checkpoints enabled, max = 32, min spacing = 2048`) | **なし** (A-after-B 16.40 s) |

2 つ目が効かない理由: 間隔は**最小値**の指定であって作成契機を早めない。間隔を
詰めてもチェックポイントは末尾 500 トークン以内にしか作られなかった。

一方で副産物として、**`ollama serve` に渡した `LLAMA_ARG_*` は子の llama-server に
確実に届く**ことが 2 種類のノブで実証できた。ollama をフォークせずに llama.cpp の
ノブを回す経路は生きている。ただし llama.cpp 側が `set_env` を付けたフラグに限る。

### 5. KV をディスクに永続化する道は今日は閉じている

- ollama は `--slot-save-path` を渡さず、**このフラグにだけ `set_env` が無い**
  (`common/arg.cpp` を直読。`--cache-ram` は `LLAMA_ARG_CACHE_RAM`、
  `--ctx-checkpoints` は `LLAMA_ARG_CTX_CHECKPOINTS` を持つ)。
- llama-server のポートは ollama が動的に割り当てて公開しない。
- **ggml-org/llama.cpp#26676 (OPEN)**: プロセス再起動を跨いだ slot restore が no-op。
  モデルのアンロードはプロセスの死なので、必要なのはまさにこの経路。
- 上流 ollama 側は `ollama/ollama#17247` + PR `#17278` (`OLLAMA_PREFILL_CACHE`、
  opt-in、8 GiB LRU、fail-open) がこれをやろうとしているが停滞中。

なお、永続化する量そのものは小さい。この構成なら再帰状態 62.8 MiB + KV 約 593 MiB
(30k トークン分) で計 **約 650 MiB**。ハイブリッドは KV が 10 層分しかないので、
むしろ永続化に向いている。

## この性質のトレードオフ

ハイブリッドが劣っているという話ではない。200,704 トークンの窓で KV が 3.9 GB
しかないのは、まさにこの設計の成果である。**小さい KV と長い窓を得る代わりに、
途中分岐からの部分再利用を失っている**。どちらを取るかは用途で変わる。

## Refs
- https://github.com/waired-ai/waired-agent/issues/838
- https://github.com/waired-ai/waired-agent/issues/866
- docs/knowledges/20260819/2130-local-engine-caches-the-prefix.md
- https://github.com/ggml-org/llama.cpp/issues/26676
- https://github.com/ollama/ollama/pull/17278
