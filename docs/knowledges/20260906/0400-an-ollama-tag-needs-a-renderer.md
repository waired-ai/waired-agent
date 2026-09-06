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

## 補足 (2026-09-06 18:30) — 「upstream の 1 フィールド待ち」は誤り。待つ先が存在しない

上の「レーンの現状」は、このレーンが upstream で止まっていると書いた —
`renderer` 付きで公開された GGUF タグか、ollama が `qwen4exp` の組み込み
renderer を得ること。**両方とも誤り**で、訂正すると自分でできることを指す。

### 1. `qwen4exp` の renderer は存在しないし、必要でもない

`v0.34.0-rc1` (prerelease、2026-09-05) と pin 中の `v0.33.3` の
`model/renderers/renderer.go` を突き合わせると、登録名は**完全に同一**。
`qwen3.5` / `qwen3.8` は両方にあり、`qwen4exp` はどちらにも無い。

そして要らない。ollama 自身の library タグがそう言っている:

```
library/qwen3.8-flash-next:125b-mlx   config blob
  renderer = qwen3.8      parser = qwen3.5
```

このモデルの会話形式は qwen3.8 のそれで、その renderer は pin に入っている。
つまり「ollama が qwen4exp の renderer を持つまで待つ」は、**起票されることの
無い変更を、既に持っている機能のために待つ**という形をしていた。

### 2. §6 の表の読み方を 1 段深くする

§6 は「タグは renderer か template のどちらかを持つ」で止まっていたが、
どちらも持たないときにサーバが何をするかまで読むと分岐が 1 本しかない
(`server/prompt.go`):

- `Config.Renderer != ""` → 組み込み renderer
- `Config.Renderer == ""` → `m.Template.Execute`、すなわち GGUF 内蔵 Jinja

`model_family` から renderer を推論する経路は**無い**。だから
`model_family: qwen4exp` は何の助けにもならず、内蔵 Jinja の 110 行目に落ちる。

### 3. GGUF 経路の構造的な穴 (registry 全数調査)

ollama.com でこのモデルを持つ namespace 6 つ、GGUF タグ 24 本すべての config を読んだ:

| namespace | GGUF | `renderer` 有り | template レイヤ |
|---|---|---|---|
| `frob/` | 8 (78.87–354 GB) | **0** | — |
| `metalspork/…-ud` | 9 (**72.55**–169 GB) | **0** | — |
| `waowao/` | 2 (111.33 GB) | 0 | **有り** |
| `kwmcglon/` | 1 (111.33 GB) | 0 | — |
| `wcamaralopes/` | 1 (91.95 GB) | 0 | — |
| `orcarouter/` / `library/` | 無し (MLX のみ) | **全部** `qwen3.8` | 有り |

**`renderer: qwen3.8` が付くのは MLX/safetensors タグだけ**。ollama の
safetensors 変換経路が自動で刻むからで、GGUF の `create` 経路は刻まず、
publisher の誰も手で書いていない。これは 1 人の不注意ではなく経路の性質なので、
**次に大型のコミュニティ量子化を採るときも同じ穴に落ちる**。

例外は `waowao/…:q4` の Go template レイヤ 1 本だけ。Go template は例外を
投げないので非先頭 system は黙って畳まれ、6 形とも通る — が 111.33 GB で
128 GB ホストの 96 GB 予算に入らない。

### 4. 「第 3 の答え」は在った

`RENDERER` / `PARSER` は pin 中のエンジンの Modelfile の正式命令
(`parser/parser.go:132-134`、`isValidCommand` に登録)。

```
FROM frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL
RENDERER qwen3.8
PARSER   qwen3.5
```

で派生させると Jinja 分岐でなく renderer 分岐に入る。`make e2e-agentgrade` は
これを測る前提を既に持っている (`NO_PULL=1` = "a locally derived model, not in
the registry")。

したがって上で引いた `docs/decisions/20260828/1930-arm-the-request-shape-gate.md`
の項目 2 は**見直す必要が無い**。「コーディングエージェントが送る形を render
できないモデルは offer しない」はそのまま正しく、renderer を刻めばこのモデルは
render できる。ゲートは免除ではなく**通過**で解ける。

### 5. 測った結果

sv-evox2、ollama 0.33.3(pin 中のもの)、同一ホスト・同一 blob。変えたのは
config 2 行のみ:

| shape | 素のタグ | 刻印後 |
|---|---|---|
| leading-system | accepted 200 | accepted 200 |
| no-system | accepted 200 | accepted 200 |
| **trailing-system** | **rejected 500** | **accepted 200** |
| double-system | accepted 200 | accepted 200 |
| **system-after-tool-roundtrip** | **rejected 500** | **accepted 200** |
| **developer-turn** | **rejected 500** | **accepted 200** |

agent-harness grade は **pass** のまま(12 trial / 5m33s、Read と Grep への
構造化 tool_use を含む)。レンダラを刻んでも tool 形式は劣化しない。

ローカル manifest の config blob を直接読んだ刻印の実費:

| | 元タグ | 刻印後 |
|---|---|---|
| `renderer` | `''` | `qwen3.8` |
| `parser` | `''` | `qwen3.5` |
| layers | projector, model, license, params | **同一の 4 本** |
| ディスク増 | — | **0.00 GB** |

`ollama create` は全 blob に `using existing layer` を出す。projector も
license レイヤ(frob タグだけが持つ Qwen Community License 1.0 全文)も残り、
差分は config blob 1 個。**同名で create し直せば下流の識別子は 1 つも動かない** —
出荷経路(`download.Puller.Stamp`)はこの形を採っている。

### 6. 副産物 — 派生でも見落としでもない穴が 1 つ

`proto/catalog/variant_sha.go` の payload は **frozen** と明記されている
(「Changing it does not 'migrate' anything: every persisted key silently stops
matching」)。よって `renderer` は VariantSHA に入れられず、**刻んだ変種と
刻んでいない変種が同じ SHA になる**。manifest から renderer を落としても
「6 形 accepted」の記録が有効に見え、ゲートは緑のままになる。

塞ぎ方は SHA を広げることではなく、**計測の側に renderer を記録して照合する**
こと。`VariantRequestShapes` は同じ理由で既に `engine_version` を持っている
(「the engine build IS the finding」)。その隣に置いた。

なお import 時点で manifest の値を写しているので、これが捕まえるのは
「あとから manifest が変わった」場合であって、「刻まずに計測したものを
刻んだ manifest に対して import した」場合ではない。後者を捕まえるには
probe 側が実際に使われた renderer を報告する必要がある — 未実装。

### 7. 刻印は pull を跨いで残らない — だから Pull の中に置いた

実機で確かめた(sv-evox2):

```
before re-pull   renderer='qwen3.8' parser='qwen3.5'
re-pull took 2s, disk delta 0.00 GB
after re-pull    renderer=''        parser=''
after re-stamp   renderer='qwen3.8' parser='qwen3.5'
```

既に在るタグへの `ollama pull` は、重みを 1 バイトも動かさずに **2 秒**で
ローカル manifest を公開時の config に書き戻す。つまり刻印は消える。

安いので頻繁に起こり得る。そして消えた状態は「良くなっていない」ではなく
**「3 形で 500 を返す」** であり、しかもストアには accepted と記録されている。

したがって刻印は `Pull` の外に置けない。`Puller.Stamp` を公開メソッドとして
呼び出し側に任せる形だと、pull する経路が 1 つ増えるたびに忘れられる余地が
できる。`Pull(ctx, tag, want, onProgress)` の内側に畳んで、**分離できない**
ようにした。
