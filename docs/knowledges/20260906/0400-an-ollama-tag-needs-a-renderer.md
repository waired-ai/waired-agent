# ollama のタグには renderer が要る — Qwen3.8-Flash-Next (125B-A6B) を Strix Halo で測った記録 (20260906 04:00)

## Issue

waired-agent#1192 (カタログに `Qwen/Qwen3.8-Flash-Next` を追加する) の実測記録。
waired-ai/waired#1312 のレーン L100。モデルは 2026-08-26 公開の Qwen4
アーキテクチャのプレビューで、BF16 でディスク上 180B、活性 6B、ネイティブ
コンテキスト 262,144。オーナーの指示は「重いモデルなので、可能な限り軽い
量子化をカタログに入れる」。

次に大きなモデルをカタログに足す人向け。結論を先に書く: **ollama のタグは、
レジストリ config の `renderer` か `template` レイヤのどちらかを持っていない
と、Claude Code が送る形を 500 で拒否する。そしてそれは 1 バイトも落とす前に
manifest から読める** (§6)。79 GB を落として GPU ホストを 1 台押さえる前に、
その 1 フィールドを見ること。

計測環境 (2026-09-06): sv-evox2 — Windows 11 26200 / Ryzen AI Max+ 395
(Strix Halo) / ユニファイドメモリ 127.15 GB / ollama 0.33.3。

## Learnings

### 1. Hugging Face 経路は、この大きさのモデルには恒久的に閉じている

Hub 上のこのモデルの軽量 GGUF は**すべて分割 (sharded)** されている —
unsloth は UD-IQ1_S 72.5 GB から UD-Q6_K_XL 169 GB まで、bartowski は
IQ1_S 70.1 GB から Q8_0 188 GB まで。ollama のレジストリ経路は分割 GGUF を
受け付けない。HF の ollama エンドポイントに要求すると **HTTP 400** で、逐語:

> This repository only contains sharded GGUF files. Ollama does not yet
> support pulling sharded GGUF via the registry; please download the shards
> and merge them locally with `ollama create` (workaround detailed at
> https://github.com/ollama/ollama/issues/5245), or use a repository with
> single-file quantizations.

ollama/ollama#5245 は 2024-06 から open のまま。肝心なのは、これが
「再アップロード待ちの隙間」ではないこと: Hub は 1 ファイル 50 GB を上限と
しているので、180B モデルの量子化は**どの量子化でも**単一ファイルになれない。
したがって `hf.co/<org>/<repo>:<quant>` は、量子化を何にしても、大きなモデル
が使える source ではない。

別件として、`ollamaregistry.SplitTag` は `hf.co/...` を namespace `hf.co`
として読み、registry.ollama.ai に問い合わせる。つまり `catalog-sources` でも
解決しない。

### 2. 単一 blob の GGUF はユーザー namespace にあり、namespace 付きタグは今日動く

`ollama.com/frob/qwen3.8-flash-next` は GGUF タグを 7 本持つ。最軽量は
`125b-a6b-ud-q2_K_XL` で、`application/vnd.ollama.image.model` レイヤ 1 本が
**78.87 GB** (加えて projector レイヤ 0.91 GB)。ollama/ollama#18075 ("Pls
support qwen3.8 flash next for windows pc") は、まさにこのモデルを llama.cpp
runner で動かして 2026-09-05 に **completed** で閉じられている。

`ollamaregistry.SplitTag` の doc comment は既にこの形を覆っている —
「`myorg/model:tag` names its own namespace」。
`registry.ollama.ai/v2/frob/qwen3.8-flash-next/manifests/125b-a6b-ud-q2_K_XL`
は 200 を返すので、`catalog-sources` は namespace 付きタグを今日そのまま
受け付ける。

### 3. 動いた。しかもよく動いた

sv-evox2、`OLLAMA_VULKAN=1 OLLAMA_IGPU_ENABLE=1` (`ResolveOllamaBackend` の
Strix Halo on Windows の腕が既に出している組) で:

- この組が無いとエンジンは iGPU を丸ごと落とす —
  `dropping integrated GPU; to enable, set OLLAMA_IGPU_ENABLE=1`、そして
  `vram-based default context total_vram="0 B" default_num_ctx=4096`。
  有りだと `library=Vulkan ... type=iGPU total="102.2 GiB" available="97.1 GiB"`、
  `default_num_ctx=262144`。
- `load_tensors: offloaded 49/49 layers to GPU`。`/api/ps` は ctx 262144 を
  報告し、`size_vram` は `size` と等しい — spill 無し。
- prefill **293.99 tok/s** (21,034 トークンを 71.55 s、エンジン自身の
  `slot print_timing` から)、decode **19.95 tok/s**、ロード 57.4 s。
- runner の argv:
  `-c 262144 -np 1 --flash-attn auto -b 512 -ub 512 --context-shift --keep 4`。
- `ollama show`: architecture `qwen4exp`、parameters 176.9B、quantization
  Q2_K、capabilities は tools / thinking / completion / vision、projector は
  clip 448.93M。

### 4. このアーキテクチャでは、常駐量は blob のサイズではない

blob はディスク上 78.87 GB だが、常駐フットプリントは **51.3 GiB / 55.09 GB**。
独立した 3 つの読みが一致する: ollama の `runner.size` / `runner.vram`、
`/api/ps` の `size`、OS (llama-server の working set 51.1 GB、committed は
127.1 GB 中 63.7 GB)。GGUF メタデータは `qwen4exp.ple.layers arr[i32,1] = [1]`
を持つ — per-layer-embedding / n-gram テーブルは全部が常駐にはならない。

これが効くのは、`estimated_weight_gb` が `hostfit.OllamaWeightsResidentMB`
(proto/hostfit/hostfit.go) に入るからで、その doc comment は「what the
WEIGHTS alone need in GPU-addressable memory」と書いている。正直な注記は常駐
の数字のほうであり、ディスク上の要求量は別の事実で、今のカタログにはそれを
持つフィールドが無い。

### 5. このアーキテクチャは KV キャッシュを 2 つ確保し、導出は 1 つしか知らない

262,144 セルでエンジンが記録するのは:

```
llama_kv_cache: size = 6144.00 MiB (262144 cells, 12 layers, 1/1 seqs), K (f16): 3072.00 MiB, V (f16): 3072.00 MiB
llama_kv_cache: size = 2304.00 MiB (262144 cells, 12 layers, 1/1 seqs), K (f16):  768.00 MiB, V (f16): 1536.00 MiB
llama_memory_recurrent: size =  112.57 MiB (1 cells, 48 layers)
```

1 つ目は 24576 B/token で、config から導出した値と正確に一致する (48 層の
うち 12 層が `full_attention`、`num_key_value_heads` 2、`head_dim` 256、K+V、
2 バイト)。2 つ目は 9216 B/token を足し、K/V が非対称 (3072 / 6144)。Qwen
Sparse Attention の indexer (`indexer_kv_heads: 1`、`indexer_head_dim: 128`)
か MTP 層のものと**推定**しているが、それ以上は詰めていない — **推定であって
証明ではない**。

実測の合計は 33792 B/token で、`internal/catalog/scoring/catalog_kv_test.go`
の導出が出す注記の 1.375 倍。導出値だけを注記すると、実 KV を 38 % 過少に
見積もることになる。

### 6. ollama のタグには renderer か template レイヤが要る — そしてそれはレジストリから見える

モデルは agent-harness の grade を通った — 12 trial、unary、全ケース pass:
greeting、read-file (Read への構造化 tool_use)、search-then-edit (Grep への
構造化 tool_use)。同じ run の request-shape マトリクスは 6 形状中 3 つを
HTTP 500 で拒否した: `trailing-system` [user, system]、
`system-after-tool-roundtrip` [system, user, assistant, tool, system, user]、
`developer-turn` [system, user, developer, user] — Claude Code が送るものその
もので、#1035 / #1095 が qwen3.8-27b で見たのと同じ失敗。

原因は、ollama がリクエストを**モデル自身の Jinja テンプレート**で render
したこと。`ollama show --template` の出力 110 行目:

```
{{- raise_exception('System message must be at the beginning.') }}
```

— #1035 の文字列そのもの。会話途中の `developer` ターンも同じ行に当たる:
テンプレートは system / developer を先頭の連続区間にしか許さない
(`{%- if sysns.count == loop.index0 and (message.role == 'system' or message.role == 'developer') %}`)。

モデルのテンプレートに到達した理由は、runner が `--chat-template` 無し・
`--no-jinja` 無しで spawn されたから。qwen3.5 / qwen3.8 のタグは
`--no-jinja --chat-template chatml` と ollama 自身の renderer で spawn される —
0.32.14 / 0.32.15 が、非先頭の指示ターンを許容し、次いでマージするよう教え
られた、あの renderer である。

そして差は、タグのレジストリ config の 1 フィールド。ダウンロードの前に読める:

| tag | config.renderer | template layer |
|---|---|---|
| qwen3.5:0.8b-q8_0 | qwen3.5 | — |
| qwen3.6:35b-a3b-q4_K_M | qwen3.5 | — |
| qwen3.8:27b-mtp-q4_K_M | qwen3.8 | — |
| gpt-oss:20b | — | あり |
| qwen3.8-flash-next:125b-mlx (library, MLX) | qwen3.8 | — |
| frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL | **無し** | **無し** |

このカタログが出荷している ollama タグはどれも、どちらか一方を持つ。この
タグはどちらも持たない。library の同じファミリの MLX タグが
`renderer: qwen3.8` を宣言していることに注意 — ファミリ自体は今日の ollama
で render できる。つまりこれは**公開者側の省略**であって、モデルの限界では
ない。所見は**タグ**について述べること。「このモデルはコーディングエージェント
を駆動できない」とは書かない — 上の grade が示すとおり、できる。

ここから出てくるガード (manifest の renderer / template を `catalog-sources`
で見る) は waired-agent#1238 に起票した。

### 7. 罠 2 つ

(a) Windows では ollama がモデル blob を**スパースファイル**として事前確保
するので、`Measure-Object Length` は届くはるか前から 78.87 GB を報告する。
`compact /q` が実際に格納されたバイト数を出す (ある時点で allocated 73.45 GB
/ stored 14.6 GB)。正直な数字は pull ログ自身のパーセンテージ。

(b) メモリ不足のエンジンは HTTP 200 を返す。48 GB の Mac で、その機械自身の
waired エンジン (Metal 予算 37.4 GiB のうち 13.5 GB を保持) の横に
`qwen3.8:27b-mtp-q4_K_M` をロードすると、`kIOGPUCommandBufferCallbackErrorOutOfMemory`
と `decode: Compute error` を記録しながら、`/api/chat` はゼロ値のボディで 200
を返した —
`{"model":"","created_at":"0001-01-01T00:00:00Z","message":{"role":"","content":""},"done":false}`
— そして `/v1` は全カウント 0 の `usage` を返した。ステータスコードだけを
見るチェックは、これを黙って通す。

## レーンの現状

レーンは判断・ライセンス・ハードのどれでも止まっていない — オーナー判断
4 件はすべて決着して記録済み (waired-ai/waired#1317) で、モデルの計測結果は
良い。止めているのは upstream の 1 フィールド: `renderer` (または `template`
レイヤ) 付きで公開された GGUF タグか、ollama が `qwen4exp` の組み込み
renderer を得ること。どちらもレジストリから数秒で確認できる。

第 3 の答えが無い理由は
`docs/decisions/20260828/1930-arm-the-request-shape-gate.md` の項目 2:
コーディングエージェントが送る形を render できないモデルは、このプロジェクト
が offer しないモデルであり、その決定が挙げる逃げ道は、エンジン pin を動かす
(既に最新リリースに居る) か、その変種を offer しないか、の 2 つだけ。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1192
- https://github.com/waired-ai/waired-agent/issues/1193
- https://github.com/waired-ai/waired-agent/issues/1233
- https://github.com/waired-ai/waired-agent/issues/1238
- https://github.com/waired-ai/waired-agent/issues/1035
- https://github.com/waired-ai/waired-agent/issues/1095
- https://github.com/waired-ai/waired-agent/issues/823
- https://github.com/waired-ai/waired/issues/1312
- https://github.com/waired-ai/waired/issues/1317
- https://github.com/ollama/ollama/issues/5245
- https://github.com/ollama/ollama/issues/18075
- docs/decisions/20260828/1930-arm-the-request-shape-gate.md
- docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md
