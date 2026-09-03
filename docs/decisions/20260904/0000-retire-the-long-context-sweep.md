---
status: accepted
---

# 長文コンテキストの sweep を廃止し、深い段で置き換えない (20260904 00:00)

## Status
Accepted。オーナー裁定（2026-09-04、作業セッション中）:

> このsweepの仕組みについてはやはり廃止、sv-evox2型で性能劣化に気が付けないのもやむなしとしたいです。（つまり新規の深さを追加はしない）

この記録がその裁定の引用元になる（waired-ai/waired-agent#1169）。

## Context

waired-ai/waired#624（200k コンテキスト床のプログラム）が入れた「長文コンテキストの
ベンチマーク」— 以下 sweep と呼ぶ。セットアップ完了後にバックグラウンドで、ローカルの
ollama エンジンに 64k / 128k / 約 200k トークンの合成プロンプトを 3 段順に流し、深さ
ごとの prefill と decode を測る仕事だった（`cmd/waired-agent/inference_bench_depth.go`）。

rc5 の実機検証（waired-ai/waired#1309、#1312）で次が確認された。

- **1 スロットのホストではエンジンを占有する。** sv-evox2（Windows の Strix Halo APU、
  ollama 0.33.2、qwen3.6-35b-a3b — エンジンはこのモデルを 1 スロットに絞る）で、sweep は
  セットアップウィザードが「ready」と報告した直後の 03:43 から 03:57 まで、**14 分間**
  エンジンを飽和させ続けた。その間、ピアからの 1 語の推論リクエストが **134.7 秒**と
  **145.6 秒**かかった。兆候は何も出ず、`waired status` はずっと `ready` だった。
- **段ごとの prefill 値を読んでいたのは `waired runtimes status` の `long-context:`
  ブロックだけ**だった。
- **軽いモデルの判定は誰にも届いていなかった。** ターミナルのセットアップ経路が推奨を
  読むのは、セットアップ側のベンチマークが完了した 1 回だけ。このホストではそれが
  03:44 で、sweep が終わる 13 分前。sweep の完了時に再評価も通知もしない。後から読む
  唯一の面はデスクトップの tray で、サーバには無い。
- **メモリ不足の検出は、実リクエストが既に到達しているハンドラに届くだけ**だった
  （`onEngineFitFailure`、後述）。
- **そもそも測る深さが違っていた。** 1 行あたりのトークン数を 35 に固定していたが、この
  モデルでの実測は 42.25。全段が 21 % 深く、最深段は 200,704 トークンのコンテキスト
  ウィンドウに対して約 239,853 トークンを要求していた。
- **置き換え案の費用。** #1127 の prefill 梯子に配信中のコンテキストウィンドウの段を
  1 つ足すと、このホストでは 1 サンプル約 8.2 分（196k トークン ÷ 約 400 tok/s）、
  梯子の「2 サンプルが一致するまで」の規則では 16 分かかる。

## Decision

- sweep を削除する。一緒に消えるもの: `waired runtimes status` の `long-context:`
  ブロック、`waired init` / `waired runtimes benchmark` が出していた
  `Local inference ran out of memory on a long prompt: …` の表示、bench キャッシュの
  `depth` マップ、`router.CodingAgentDepthFloorFraction`（深さの床 = 60 × 0.8 = 48 tok/s）。
- #1127 の prefill 梯子に深い段を**足さない**。
- 対話床（`router.CodingAgentSelectionFloorTokps` = 60 tok/s）の判定は、ブート
  ベンチマークの浅い decode **だけ**で決める
  （`cmd/waired-agent/inference_recommendation_test.go`
  `TestInteractiveFloorVerdict_RestsOnTheShallowRateAlone` が pin する）。
- bench キャッシュのスキーマ版（`cmd/waired-agent/inference_bench_cache.go`
  `benchCacheSchemaVersion`）は**上げない**。削除にスキーマ版の bump は不適切で、
  `encoding/json` は読み込み時に未知のキーを無視し、次の Store で `depth` は落ちる。
  bump すると全ホストのブートベンチマークのエントリが捨てられ、誰も読まないフィールドを
  片付けるために全機で測り直すことになる。bump は「もう信用してはいけない数値」の
  ためのもので、残る数値はいまも正しい（同ファイルのコメントに同じ理由を残した）。

## Consequences

失うもの:

- 深さ 0 では対話床を超えるが、深さで這うホストは、もう気づかれない。持ち主に軽い
  モデルも提案されない。実測例は sv-evox2 の 1 件: 浅い decode 81.5 tok/s（床 60）に
  対して 128k で 42.05 tok/s（深さの床は 48 だった）。裁定はこれを「やむなし」とした。

残るもの:

- 実リクエストで GPU がメモリ不足になると、エンジンの 5xx は
  `onEngineFitFailure`（`cmd/waired-agent/inference.go`）に届き、適用中の tuning に
  `Degraded` / `WindowFits = false` とエンジン自身の一文を警告として記録する。警告は
  `waired models ls --detail`（FIT 列 `! running here with a warning` と表の下の一文）、
  `waired status`、`waired doctor`（`EngineTuningWarning`、
  `cmd/waired/doctor_observability.go`）に出る。このハンドラは**何も下げない**:
  バッチはエンジン自身の選択（waired-agent#1079）で、コンテキストウィンドウは verify
  pass の段であってリクエストハンドラの段ではない。
  `cmd/waired-agent/inference_fit_failure_test.go`
  `TestOnEngineFitFailure_SurvivesTheRetiredSweep` が pin する。
- `WindowFits = false` はメッシュへの宣言を止めない。waired-ai/waired-agent#657 以降、
  `DeclaredContextWindow` は適用中のコンテキストウィンドウの大きさだけを見る。
  メモリ不足を記録したホストも、そのウィンドウを宣言し続ける。
- セットアップが `ready` を返した直後にエンジンを占有する仕事が無くなった。
- `docs/decisions/20260830/0230-measure-once-per-selection-not-on-a-schedule.md`
  の Consequences にある「depth sweep も判定に付いてくる」は、この記録で空になった
  （ループ自体はそのまま）。

戻すなら:

- 測り方より先に**届け方**を解くこと。セットアップの面が閉じた後に終わる計測は誰にも
  届かない。この sweep が役に立たなかったのは測定の欠陥ではなく、結果を読む相手が
  居なかったことによる。誰が・いつ・どの面で読むかが決まってから、何をどの深さで
  測るかを決める。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1169
- waired-ai/waired#624（200k コンテキスト床のプログラム。private monorepo、番号のみ）
- waired-ai/waired#1309、waired-ai/waired#1312（rc5 実機検証の記録とレーン追跡。
  private monorepo、番号のみ）
- docs/decisions/20260830/0230-measure-once-per-selection-not-on-a-schedule.md —
  sweep をブートベンチマークの判定に相乗りさせた記録
- docs/decisions/20260829/1740-speed-is-measured-at-fixed-depths.md — #1127 梯子の決定
- cmd/waired-agent/inference_bench_cache.go（`benchCacheSchemaVersion` のコメント）
