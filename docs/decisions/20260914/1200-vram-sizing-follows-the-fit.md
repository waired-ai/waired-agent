---
status: accepted
---

# VRAM の見積りは llama.cpp の fit と同じ項で足し、配置の証拠はエンジンのログで読む (20260914 12:00)

## Status
Accepted。オーナー決定（2026-09-13、waired-ai/waired#1357）の決定 8「1 台のホストの実測から導いた定数は機構に置き換える。0.20 の意味の読み替えも L113 の裁量」を受けた L113（#1337）の実装判断。

次の記録を**散文で狭める**（本文は凍結のまま）。

- `docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md` の決定 8: 置き換えの中身をこの記録が決める。
- private monorepo の `docs/decisions/20260714/0245-deliberate-spill-cap-0-075-to-0-20.md`: 0.20 は「期待スピル（/api/ps 基準の較正値）」から「llama.cpp の fit がシステム RAM に置く重みのバイト比（予測）」に読み替わる（下の決定 3）。リポ間なので guard の双方向リンクは張らない。

## Context

24 GB 級 GPU × `qwen3.8-27b` MTP-Q4 × 200,704 トークン × KV q8_0 で、`hostfit` は期待スピル 11% を出し、llama.cpp の fit は 66 層のうち 13 層を CPU に置いた（176k で decode 28 倍遅い）。製品の不足 915 MiB に対し fit の不足は 3,794 MiB。差は項の欠落で、`OllamaSpillCalibration = 3.0`（#625 の単点較正）がまとめて肩代わりしていた:

- fit の余裕（`LLAMA_ARG_FIT_TARGET`、既定 1 GiB。ollama は projector を載せるとき projector + 1 GiB）
- 再帰状態（gated DeltaNet の R + S。MTP の draft が動くと 1 + draft 数ぶん）
- MTP の draft 用コンテキスト（KV は常に f16、compute バッファ、セットアップ分）
- KV のブロック係数（q8_0 は 34/64 = 0.53125 で、半分ではない）
- 入力層（`token_embd`、`per_layer_token_embd`）は空き VRAM に関係なく CPU に残る

ollama の `/api/ps` の `size` / `size_vram` は ollama 自身がバッファ行を解析した値で、draft コンテキストと `CPU_Mapped` を数えない（MTP ビルドで実 21.2 GB を 14.97 GB と報告、Strix Halo で 36% が CPU にあるモデルを 100% GPU と報告）。

## Decision

1. **見積りは fit の項ごとに足す**（`proto/hostfit/estimate.go`）。重み（GGUF のテンソル表から。draft が動かないタグでは nextn ブロックを除き、projector を足す）− 入力層 + KV（ggml のブロック係数）+ 再帰状態 +（主）compute + draft（KV・compute・セットアップ）+ プロセスのデバイスコンテキスト + fit の余裕。項は工学的事実（ブロック係数、fit の既定）を公開定数、実測値（compute、コンテキスト）を非公開定数にし、較正し直しを const 値の変更にしない（additive-only の guard）。
2. **入力はカタログの `gguf` 配置**（ブロック数、full-attention 層数、1 シーケンスの再帰状態、draft 数、テンソル合計、projector、nextn、repeating、expert）と `host_resident_weight_gb`。すべて `catalog-tool layout` が GGUF ヘッダとタグのマニフェストから導き、`--check` が登録値と突き合わせる。手で書かない。
3. **0.20（`OllamaMaxExpectedSpillFraction`）は「fit がシステム RAM に置く重みのバイト比」として読む。** 導出元の #664 のモデル `1/rate = (1-s)/158.6 + s/21.25` は s をシステム RAM から読む重みの比として書かれており、較正倍率を挟まない比の方が導出と同じ量になる。推奨からは退場済み（決定記録 2355 の決定 10）で、手で選んだモデルの rung 規則 2 だけに残る。
4. **配置の予測は層単位**（`OllamaPredictPlacement`）。fit は後ろのブロックから GPU に詰めるので、CPU に残るのは前のブロック。各ブロックが解放するのは重みと、線形層なら再帰状態、full-attention 層なら KV。MoE は expert テンソルを前から先に動かす。full-attention 層の KV が CPU に出ると compute バッファが増える分を足す。
5. **ロード後の証拠はエンジンのログ**（`internal/runtime.ParseLlamaPlacement`）。最後の runner 起動以降の `offloaded N/M layers`、モデルバッファの device / host 別の大きさ、主 KV の型、fit の projected / 不足を読む。コンテキストウィンドウ（n_ctx）と runner の `--model` が今のチューニングと一致しないロードは証拠にしない（adopt したエンジン、上限に達したログ）。証拠が無いときは spill を判定しない（`/api/ps` は spill と自分の数え漏れを区別できない）。
6. **spill の判定はバイト**: fit が動かした重み（host 側の重み − 入力層）が予測 + 1 層ぶんを超えたら spill。MoE は層数が変わらずに expert だけ動くので、層数では比べない。
7. **GPU を使わなかったロードは spill と別の判定**（#71）: 証拠が `offloaded 0/M`（証拠が無ければ `size_vram == 0`）なら、コンテキストウィンドウを下げても再起動しても変わらないので、どちらもせず Degraded と警告だけを残す。コンテキストウィンドウは宣言し続ける（#657）。
8. **compute バッファは ubatch 512 で見積る。** ollama は ubatch を自分で選ぶ（`server/sched.go` の `automaticGenerationBatch`、v0.33.3）。コンテキストウィンドウが 32,768 を超えると 2048 から始め、ollama 自身の予測（ファイルサイズ + f16 の KV）が空きの 60 % / 75 % 以下のときだけ 2048 / 1024 を保ち、それ以外は 512 に下げる。大きい ubatch は空きに余裕があるときだけ起きるので、載るかどうかの判定は変わらない。compute の土台は ubatch 512 あたり 80 MiB で、どのバックエンドでも同じ扱いにする（ubatch 512 で 52〜80、1024 で 117、2048 で 96〜233 MiB が実測）。以前 Metal / unified に置いた 850 / 950 MiB の土台は、ubatch 2048 で動いていたロードを 512 として読んだ誤りだった。
9. **表示は GB のまま、内訳を分ける**（オーナー判断 2026-09-14、このセッション）。ピッカーの事実行は「重み + KV キャッシュ + エンジンのオーバーヘッド」（`Presentation.device_weights_mb` / `kv_cache_mb`、残りがオーバーヘッド）。CLI / tray の接尾辞は `· N GB in system RAM`（KV キャッシュと呼ばない。fit がシステム RAM に移すのは KV ではなく層）。決定記録 2355 の決定 10 のうち「行には spill % でなく GPU に載る層数 / 全層を出す」を**この記録が散文で狭める**: 層数は `Presentation.gpu_layers` / `total_layers` とチューニングの判断理由に出し、行の接尾辞は GB で出す。
10. **fit の余裕（`LLAMA_ARG_FIT_TARGET`）は Waired から設定しない**（オーナー確認 2026-09-14）。1 GiB は llama.cpp 作者が「保守的」と呼ぶ値で実測の根拠は無い（ggml-org/llama.cpp#16653、#23772）が、24 GB 級 GPU × 27B UD-Q3 × 200k で余裕を 256 MiB にすると、10 万トークンのプロンプトは通り、最初の画像入力で `cudaMalloc failed: out of memory`（469 MiB）となり llama-server が落ちた。既定の余裕でも、画像処理のピークで空きは 501 MiB まで減った。OS の画面描画が持つ VRAM が予算（エンジンが重みを持つ前に測った free）と fit のロード時の free から引かれるのは、その読み値がデスクトップの立った後に取られたときだけ: agent はサービスとして起動し `Profiler.freezeVRAMFree` はプロセス最初の読み値を凍結するので、再起動した Windows / Linux 機ではログイン画面の値が予算になり、あとから立つデスクトップの分（実測: 3 画面の Windows 機でアプリ込み 2.7 GB、Optimus 機の dGPU は 0、ヘッドレスは 43 MiB）は引かれない。Mac は Metal の上限（RAM の 2/3〜3/4）が OS の分を構造的に残し、Strix Halo は固定枠で枠の外。無視できる量かもしれず、OS 単体の量と予備の値は詳細なベンチマークが要る。この予算の入力（`waired init` が測った空き VRAM の保存、ディスプレイ駆動 GPU への予備）は L113 の範囲外で、waired-ai/waired-agent#1375 で扱う。

## Consequences

- 推奨・容量・rung・Presentation（`gpu_layers` / `total_layers`）がこの見積りを読む。CP は proto のタグと `waired/go.mod` の bump で同じ答えになる。
- 代表ホストの推奨が一段動く帯がある（8 / 16 GB discrete、unified の 32 GB など）。表は PR 本文と #1347 に置く。
- 実測値の定数（compute、デバイスコンテキスト、draft のセットアップ）は測ったバックエンドの値。別のバックエンドで外れていれば、proto のタグと CP の bump で直す。
- `qwen3.8-flash-next` は `gguf` 配置を持たない（サイズの訂正 #1305 と `host_resident_weight_gb` の値 #1349 が一緒に入るまで、旧来の項で見積る）。

## Refs
- waired-ai/waired-agent#1337（L113）、#1347、#1346、#1330、#71、#1375（予算の入力）
- waired-ai/waired#1357（fit ログの実測、決定 8）
- `docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md`
- `docs/knowledges/20260914/0120-input-layer-tensors-stay-on-the-cpu.md`
- `docs/knowledges/20260912/2130-ollama-ps-hides-cpu-mapped-weights.md`
