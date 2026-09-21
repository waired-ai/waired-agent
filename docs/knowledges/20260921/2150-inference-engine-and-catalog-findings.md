# 推論エンジンとカタログで読み違えやすい点 (20260921 21:50)

## Issue

推論エンジン（ollama / llama.cpp）の挙動と、同梱カタログにモデルを足す作業について、
すでに在る note・決定記録・コードのコメントが書いていない点だけを集めた。
正本が在る主題は写さずにパスを指している。
出自: セッションのメモリに在った知見を、オーナーの指示（2026-09-21、
waired-ai/waired-ai#12）でリポジトリへ移した。

## Learnings

### 1. プレフィックス再利用を測る実験で `tools` を落とすと結果が無意味になる

いつ読むか: プレフィックス再利用（prompt cache）の効き方を、捕捉したリクエストを
エンジンへ直接投げて測るとき。

機構と実測の正本は `docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md`
（冒頭の訂正と §1-bis）。要点だけ: ハイブリッド / 再帰のモデル（qwen3.5 は
0.8b / 4b / 35b-a3b の全部）はコンテキストチェックポイントからしか再開できず、
チェックポイントは末尾の狭い範囲（実測で約 500 トークン）にしか作られない。
分岐点がその範囲の中なら再利用が効き、外なら全再計算になる。中間は無い。
効くかどうかを決めるのは分岐点までの距離で、モデルの種類でも大きさでもない。
範囲の幅がモデルで変わるかは未確定のまま。

その note に無い点:

- Anthropic 形式のリクエストを OpenAI 形式へ変換するときに `tools` を落とすと、
  Claude Code のプロンプトが約 30k トークンから約 2k トークンになる
  （`tools` は 54 本で 110,738 文字）。分岐点の位置も変わるので、
  「どの分岐でも再利用が効く」という誤った結果が出る。
  変換するときは `tools` も写す。

### 2. カタログにモデルを足すとき、手順に書いていない点

いつ読むか: `proto/catalog/bundled/*.json` にモデルを足す・差し替えるとき。

手順の正本は `CONTRIBUTING.md` の「Adding a model to the catalog」
（compute → draft → tier → validate → `make catalog-docs` → agentgrade / shapes /
turnspeeds の 3 つのゲート）。実例は waired-agent#825（qwen3.8-27b）。
エンジンの pin が古いとレジストリが `412` を返すこと、`-dirty` の付いたレポートは
import できないことは、そこに書いてある。以下はそこに無い点。

- 同梱カタログは `//go:embed` なので、ビルド済みの `catalog-tool` は古いカタログを読む。
  manifest を編集した後は `go run ./cmd/catalog-tool ...` で呼ぶ。
- `catalog-tool draft --spec` と `catalog-tool tier --format text` は stdout に出すだけで、
  ファイルへのリダイレクトと、tier の整数を manifest へ書き写すのは手作業になる。
- 手で保守している表が 2 つ在り、足し忘れるとテストが落ちる:
  `proto/hostfit/model_size_test.go` の `shippedSizes` と、ハイブリッドのモデルなら
  `internal/catalog/scoring/catalog_kv_test.go` の `hybridArchConfigs`。
  KV キャッシュが 1 本だとは限らない（qwen4exp は 3 本で、K と V の幅が違う。
  `docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md` §5）。
- agentgrade のゲートは、ollama で serve できる同梱 variant(量子化ビルド)の全部に
  verdict を求める（`cmd/catalog-tool/agentgrade.go` の gaps の判定）。
  測るときは `make e2e-agentgrade MODEL=<tag> TRIALS=24` を、そのままと `STREAM=1` の
  2 回走らせ、`catalog-tool agentgrade --import` に 2 本まとめて渡す
  （`--import` は繰り返せて、同じモデルの複数回を 1 つの verdict にまとめる）。
  24 GB の NVIDIA GPU のホストで 1 variant あたり約 4〜5 分だった。
  そのホストで測れるモデルを `unmeasurable` に入れて済ませることはできない
  （`unmeasurable` は「どの runner にも載らない」の宣言）。
- ollama の pin を動かしたら
  `go test -tags integration -run TestPinnedReleasePublishesEveryAssetChecksum ./internal/runtime/`
  を走らせる。
- `waired update` はエンジンも pin に合わせるが、エンジンの入っていないホストには
  入れない。入れるのは `waired init` だけ（`cmd/waired/runtimes_upgrade.go` のコメント）。
  手で合わせるなら `waired runtimes upgrade ollama`。
- tier は、両隣の variant の番号が詰まっていると、間に空きが無い。`tier` の freeze モードは、
  間に空きが無いと 1〜100 の空いている最小の整数を割り当てる（`internal/catalog/tier.go` の
  `freeTier`）。空いている最小が 1 なら、新しい variant は順位の最下位に置かれる。
  エラーにはならないので、出力の整数を見て気づくしかない。同じ系列の世代交代なら、
  相対順序を保ったまま、その範囲を丸ごと振り直してよい。
- GGUF のタグには manifest の `Variant.Renderer` / `Parser` を書く。
  無いと request-shape のゲートが 500 で落ちる（§7）。
- `docs-site/src/data/model-sizes.json` が古いことを、CI は検出しない。
  unit のジョブが回す `TestModelCatalogPageFresh` が見るのは
  `docs/reference/models.md` だけで、両方を見る `make verify-catalog-docs` を
  呼ぶ workflow は無い。`make catalog-docs` を忘れると、公開サイトのモデル表の
  Size 列が何も言わずに `—` になる。
- `proto/` 配下の変更なので、PR ごとの testnet（`testnet-pr.yml`、約 25 分）が起動する。
  マージすると `proto-tag.yml` が patch タグを切る。
- コントロールプレーンに新しいモデルを認識させるには、private 側で proto 依存の
  bump が別に要る。bump するまで、コントロールプレーンはそのモデルの指定を 400 で拒む。
  bump は `go get` で終わりではなく、private 側のフルテストまで回してから PR にする。
  新しいエントリの tier や下限の値によって、カタログと突き合わせるテストが
  落ちた実例が在る（waired-ai/waired#1327）。

### 3. `manual_only` は「自動では選ばれない」で、退役とは別

いつ読むか: あるモデルを自動の選択から外したいとき、またはカタログから無くしたいとき。

- 自動では選ばれないようにするのは、manifest の `manual_only: "<理由>"`。
  効くのは `proto/modelrank/rank.go` の Step 1.5 の 1 か所だけで、`proto/` は
  コントロールプレーンも読む共有モジュールなので、コントロールプレーン側の
  推奨にも同じ skip が効く。一覧・名前の解決・pull・serve・明示の指定は生きたまま。
- 退役（カタログから無くす）は `proto/catalog/retired.go`。名前は永久に予約され、
  `bundled` の JSON は削除する。
- `internal/management/inference_catalog.go` の `CatalogFamily` は `manual_only` を
  運ばない。だから Waired アプリと Waired コンソールには、その理由が届かない。

### 4. variant id はモデルの中でしか一意でない

いつ読むか: 「この記録は、今提供しているものの記録か」「同じ選択か」を判定するコードや
テストを書くとき。

`variant_id` は 1 つのモデルの中でだけ一意で、`qwen3.6-35b-a3b`・`qwen3.6-27b`・
`qwen3.8-27b` はどれも `mtp-q4-gguf` を持つ。waired-agent#1357 の `servedSpeed` は
「最後の計測の variant == 提供中の variant」で判定していたため、実機でモデルを
35B-A3B から 27B に切り替えた直後の十数秒、`model_speed` と `/healthz` が
35B-A3B の 67 秒を 27B の値として公開した（2026-09-14 に実機で見つかった）。
それより前に在った `PrefillRateForHealth` も同じ比較をしていて、その形を写した結果だった。
修正後の判定は `cmd/waired-agent/inference_prefill_state.go` の `servedSpeed` に在る。

- 単体テストは全部緑だった。フィクスチャのモデルごとに variant の名前が違ったので、
  重なりが再現しなかった。同一性を判定するコードのフィクスチャには、
  同じ variant id を持つ 2 つのモデルを入れる。
- 比べるのは model id と variant id の組か、`catalog.VariantSHA`。
- 名前に「id」と付いていても、一意である範囲は確かめないと分からない。

### 5. ollama は生成バッチを自分で決める — 見るのは runner の argv

いつ読むか: ollama のバッチ・VRAM・OOM まわりを触る前、または
「バッチを上げれば速くなる」と考えたとき。

機構と実測の正本は `docs/decisions/20260828/1900-retire-the-forced-generation-batch.md` と
`docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md` §7。
ollama 0.32.12 以降は `server/sched.go` がコンテキストウィンドウから生成バッチを選び
（> 32768 → 2048、> 4096 → 1024、それ以外 512）、収まらなければ 2048 → 1024 → 512 と
自分で下げる。モデルに `PARAMETER num_batch` が在ると、この仕組みが丸ごと止まる。
強制バッチ（`-wb2048` の派生タグ）とロード後の空き VRAM の下限（768 MiB）は、
この理由でオーナー裁定により廃止された（上の決定記録）。以下はそこから引き出した見方。

- バッチまわりを触る前に、稼働中の `llama-server` の argv から `-b` / `-ub` を読む。
  エンジンが何を選んだかが 1 コマンドで確定する。コンテキストウィンドウ 200704 で
  `-b 512 -ub 512` になっているのは、エンジンが 2 回下げた結果であって、
  チューニングが効いていないのではない（24 GB の NVIDIA GPU の Linux ホストと、
  Strix Halo の Windows ホストで同じだった）。
- 「既定は 512 だから、上げれば速い」は 0.31.1 の頃の話。今上げると、
  エンジンが残した余裕を使い切ることになる。
- 空き VRAM の量では成否を分けられない。空き 506 MiB で 171k トークンが通り、
  空き 52 MiB では 2k トークンが通らなかった。システム RAM へあふれた割合でも分けられない。
  `/api/ps` の `size` / `size_vram` は重みの話で、KV のバッファと compute バッファは出ない。
- 確かめる順: argv を読む → 複数の ubatch にまたがる長さの実プロンプトを 1 回投げる →
  空き VRAM は最後の補助。
- OOM のたびに runner が死んでモデルが降ろされ、空き VRAM が全量に戻る。
  空きが全量に戻っていたら、それは「直った」ではなく「runner が死んだ」。
  次のリクエストはモデルのロードからやり直して、同じ失敗をする。
- AMD の OOM の文言は `ROCm error: out of memory`（MUSA では "MUSA"）。
  CUDA の文言だけで分類すると、AMD では何も当たらない
  （製品の分類は `internal/runtime/load_memory.go` と `internal/runtime/ollama.go`）。
- 1 点だけの測定を記録に格上げしない。条件（argv、`/api/ps`、コンテキスト長、
  毎回ロードし直したか）を併記して、再現できる形で採る。

### 6. ollama の `keep_alive` — OpenAI 互換の口は 200 で受けて捨てる

いつ読むか: ollama を手で叩いて計測するとき、keep-alive まわりを変えるとき。

正本はコードのコメントに在る:

- `/v1/chat/completions` は `keep_alive` を HTTP 200 で受けて捨て、`/api/generate` /
  `/api/chat` は尊重する。waired の配信は `/v1/chat/completions` を通るので、
  リクエストがロードしたモデルの keep-alive を決めるのは spawn 時の
  `OLLAMA_KEEP_ALIVE` だけ。メモリに載っているモデルには、もう一度ロードを
  要求すれば再ロード無しで `expires_at` だけが動く。載っているモデルが無ければ
  エンジンを再スポーンするしかない。プローブのループで打ち直す案は、有限の設定では
  永久に失効しなくなるので採らなかった
  → `cmd/waired-agent/inference_residency.go` の `ApplyResidency`。
- `{"keep_alive": "-1"}` は HTTP 400（モデルはロードすらされない）、`-1`（数値）と
  `"-1s"` は 200 で無期限。`"-1"` は環境変数の綴りとしては正しく、リクエストの
  duration 文字列としては単位が無いので不正。製品は `ResolveKeepAlive`（環境変数）と
  `ResolveRequestKeepAlive`（リクエスト、`-1s`）の 2 本を持つ
  → `internal/runtime/ollama.go`。3 OS で同じだった。
- この非対称は、pin を動かすたびに測り直している項目
  → `internal/runtime/ollama_version.go` の pin の上のコメント。
- 設計の決定は `docs/decisions/20260820/0130-model-residency-is-a-setting.md`。

コメントに無い点:

- 捨てられてもエラーにならないので、フェイクのエンジンを相手にしたユニットテストは
  緑のまま、実機でだけ間違う。
- 製品が `keep_alive` を送るのは、warm・keep-alive の設定変更の打ち直し・各種プローブだけで、
  通常のリクエストには乗らない。
- 計測スクリプトから無期限を渡すときは、数値の `-1` で渡す。

### 7. GGUF タグの renderer と、pull の失敗の読み方

いつ読むか: コミュニティが公開した GGUF のタグをカタログに採るとき、
数十 GB のモデルを pull するとき。

正本は `docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md`（§6・§7・補足）と
`docs/knowledges/20260907/0030-lighter-quants-only-exist-off-library.md`（§5〜§7）。
そこに在るもの: config の `renderer` が空のタグは GGUF 内蔵の Jinja で刷られ、Qwen 系は
先頭以外の system を拒んで 500 になること。`renderer` を持つのは safetensors / MLX の
タグだけであること。正しい値は ollama 公式 library のタグの config に在ること
（flash-next は `renderer: qwen3.8`）。manifest の `Variant.Renderer` / `Parser` に書けば
`Pull` がタグに書き込むこと。既に在るタグへの `ollama pull` は 2 秒でローカルの
manifest を公開時の config に書き戻すので、書き込み済みのタグを測るときは
`NO_PULL=1` であること。`ollama create hf.co/<org>/<repo>:<quant> -f Modelfile` が通ること。
`VariantSHA` の payload は frozen で renderer を入れられないので、ガードは計測の側に
置いたこと。`Puller.Pull` は `exit status 1` としか返さず、真因はエンジンのログに在ること。
Windows では blob がスパースファイルとして事前確保されること。

そこに無い点:

- `model_family` から renderer を推論する経路は、エンジンに無い。
- config blob の段で落ちる pull（サーバのログに `download.go` の
  "failed to get direct URL"）は、タグの欠陥なのか、短時間にリクエストを
  送りすぎたのかを区別できない。Hugging Face は
  `ratelimit-policy: "fixed window";"api";q=500;w=300`（5 分で 500 リクエスト）を
  宣言している。判定するなら、1 タグずつ、間隔を空けて、ホストがほかに何も
  引いていないときに行う。ディスク上の partial が 44 バイトなら、落ちたのは
  重みの本体ではなく config blob。
- 79 GB の pull が 74 GB 付近で止まったことが在る。TCP 接続は established のままで、
  何も流れなくなった。進捗が 150 秒無ければ kill して再実行すると、
  完了した part から再開する。
- 進捗をファイルのサイズや空き容量の差分で測らない。全長が事前確保されるので
  `Measure-Object Length` は最初から全量を返し、Windows の空き容量は実際に
  ダウンロードした量の 3 倍以上減って見えた。差分は「止まっていない」ことの
  判定にだけ使う。完了は、`ollama list` にタグが出たか、ログの `success` で見る。

### 8. PowerShell から ollama を呼ぶとき

いつ読むか: Windows のホストで `ollama pull` / `ollama create` をスクリプトから回すとき。

- `Start-Process` は `$LASTEXITCODE` を設定しない。成功した `ollama create` を
  exit 1 と読み違えたことが在る。成否は出力の `success` で見る。
- `$ErrorActionPreference="Stop"` の下で `& ollama pull` を呼ぶと、進捗の ANSI
  シーケンスが stderr に出た時点で `NativeCommandError` になり、manifest を取得した
  直後に落ちる。`Start-Process -RedirectStandardError` か、HTTP の `/api/pull` を使う。

### 9. CPU が全スレッド 100% で GPU が 0% — CPU 推論ではなく、あふれた dense モデルの decode

いつ読むか: 生成が遅いホストで、htop と nvidia-smi を見て「GPU を使っていない」と
判断しそうになったとき。

24 GB の NVIDIA GPU（RTX PRO 4000 Blackwell）と 16 コアの CPU（Ryzen 9 9950X）を積んだ
Linux ホストで、`qwen3.8:27b-mtp-q4_K_M`（コンテキストウィンドウ 200,704、KV は q8_0）を
動かしたときの見え方:

| | htop | nvidia-smi | 生成 |
|---|---|---|---|
| dense 27B（66 層のうち 13 層が CPU 側） | `llama-server` の 16 スレッドが各 101% | 使用率 0%、53 W | 22 tok/s |
| 切り替えた後の MoE `qwen3.6:35b-a3b`（13% がシステム RAM 側） | — | 使用率 61% | 133 tok/s |

これは CPU 推論ではない。dense のモデルは、VRAM からあふれた層の重みをトークンごとに
システム RAM から読む（帯域は GPU の約 1/10）。MoE のモデルは、llama.cpp の fit が
expert だけを CPU 側に置くので、あふれても安い。

配置の読み方の正本は `docs/knowledges/20260914/1230-llamacpp-fit-memory-terms.md`
（fit の項、`-ngl` を渡さないこと、`LLAMA_ARG_FIT_TARGET` = projector + 1 GiB）、
`docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md`、決定は
`docs/decisions/20260914/1200-vram-sizing-follows-the-fit.md`（waired-agent#1337）。
読むのは `llama-server` のログの次の行で、ollama の `/api/ps` ではない:
`common_params_fit_impl: projected … vs free`、`need to reduce device memory by`、
`load_tensors: offloaded N/M layers`、`CUDA0` / `CPU` の `model` / `KV` / `RS` /
`compute buffer size`。`/api/ps` の `size` / `size_vram` は ollama がそれらの行を
解析して作る値で、MTP のビルドでは実際の GPU 使用 21.2 GB を 14.97 GB と報告した。
MoE の一部があふれた場合は `offloaded N/M` にも出ない。

## Refs

- waired-ai/waired-ai#12（メモリからリポジトリへ移す指示）
- https://github.com/waired-ai/waired-agent/pull/825
- https://github.com/waired-ai/waired-agent/pull/1357
- https://github.com/waired-ai/waired-agent/issues/1337
- waired-ai/waired#1327
- CONTRIBUTING.md「Adding a model to the catalog」
- docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md
- docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md
- docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md
- docs/knowledges/20260906/2100-the-qsa-indexer-adds-a-third-kv-cache.md
- docs/knowledges/20260907/0030-lighter-quants-only-exist-off-library.md
- docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md
- docs/knowledges/20260914/1230-llamacpp-fit-memory-terms.md
- docs/decisions/20260820/0130-model-residency-is-a-setting.md
- docs/decisions/20260828/1900-retire-the-forced-generation-batch.md
- docs/decisions/20260914/1200-vram-sizing-follows-the-fit.md
- proto/modelrank/rank.go, proto/catalog/retired.go
- cmd/waired-agent/inference_prefill_state.go, cmd/waired-agent/inference_residency.go
- internal/runtime/ollama.go, internal/runtime/ollama_version.go, internal/runtime/load_memory.go
