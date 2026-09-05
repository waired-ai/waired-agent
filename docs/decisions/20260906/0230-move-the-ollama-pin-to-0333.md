---
status: accepted
---

# ollama pin を 0.33.3 へ動かす — アーキテクチャのために (20260906 02:30)

## Status

Accepted。waired-agent#1193 (ollama 0.33.3 の検証と pin 移動、
waired-ai/waired#1312 の L100)。ollama 0.33.2 → 0.33.3 を **#1193 単独の
1 本の PR** で出し、waired-agent#1192 のカタログ追加は後続の別 PR にする。

## Context

`OllamaPinnedVersion` (internal/runtime/ollama_version.go) は 0.33.2
(docs/decisions/20260829/1600-move-both-engine-pins.md)。waired-agent#1193
は「v0.33.3 が stable になるまで blocked」として起票されたが、GitHub の
release API は 2026-09-06 時点で v0.33.3 を prerelease=false、公開日
2026-09-02 と報告する。つまりその日以降、実際には何も塞いでいなかった。

pin を動かす必要があるのは waired-agent#1192 のため。Qwen3.8-Flash-Next の
カタログエントリは `min_engine_version` 0.33.3 を名乗る必要があり、
`TestBundledEngineFloorsNeverExceedThePin`
(internal/router/engine_floor_pin_test.go) は床 <= pin を要求する。pin が
先に動かない限り、エントリは着地できない。

前回同様、pin 移動は 1 行の変更では済まない。この製品は upstream が
約束していない挙動をエンジンから読み出しており、その 1 つが変わっても
何もエラーにならない。何を測ったかは
`docs/knowledges/20260906/0230-ollama-pin-0333.md` に全部ある。

## Decision

1. **0.33.3 を取る。理由はアーキテクチャであって、キャッシュではない。**
   同梱の llama.cpp が b10630 → b10760 に動き、qwen4exp は b10666
   (ggml-org/llama.cpp#27742) で入った。つまりこれは、llama.cpp runner が
   Qwen3.8-Flash-Next ファミリーをそもそもロードできる最初の pin 版で
   ある。「Report cached prompt tokens」も製品に届く変更だが (下の
   Consequences)、pin を動かす理由ではない。
2. **variant ごとの `MinEngineVersion` の床は pin と一緒に動かさない。**
   docs/decisions/20260829/1600-move-both-engine-pins.md の 5 と
   docs/decisions/20260816/2024-qwen3-8-takes-the-27b-band.md が置いた
   規則の再確認であり、出荷中のカタログで床を変えるものは 1 つも無い。
   0.33.3 を床に名乗るのは #1192 の新エントリだけで、それは別 PR。
3. **`amdROCmSupportedRes` と `ResolveOllamaBackend` の Strix Halo Windows
   分岐は変えない。** upstream はその下で動いた — Windows の ROCm overlay
   は ROCm 7.1 になり、rocBLAS カーネルは Strix Halo (gfx1150/1151) と
   RDNA4 (gfx1200/1201) を含む — が、測定は waired-agent#1233 に起票して
   そこで行う。理由は 2 つ。今ある 2 つのギャップはどちらも安全側に
   落ちる (Vulkan で終わり、Vulkan は動く)。そして、ファイル一覧を根拠に
   ホストが優先する backend を変えるのは、成果物から挙動を推論する
   一歩であり、同じ関数の Linux 分岐が主張せず probe することで取らずに
   いる一歩そのものである。
4. **測定がコメントを偽にした箇所は、挙動でなくコメントを狭める。**
   狭めたのは 3 つ: internal/gateway/convert.go の「ollama はどの面にも
   cached_tokens 相当を持たない」、cmd/waired-agent/inference_ollama_tuning.go
   の「pin 版エンジン自身の既定は 32768」、internal/runtime/ollama_backend.go
   の `amdROCmSupportedRes` の「ROCm v6.1 overlay」と削除済みファイルへの
   参照。0.33.2 の bump が convert.go の instruction ターン畳みの主張を
   狭めたのと同じ先例に従う。
5. **PR は 1 本 (#1193 単独)。** #1192 のカタログエントリは proto/ に
   触るので別 PR、かつ後。CLAUDE.md §Modules は proto 変更を独立した
   小さい PR で出すと定めており、この順序にすれば床 <= pin のガードも
   自然に満たされる。

## Consequences

- `OpenAIUsage.CachedPromptTokens` が ollama 経路でも実際の再利用量を
  返し始める (internal/gateway/anthropic.go の `rr.setCachedInput` に
  流れる)。コードは変えていない — convert.go は #885 で vLLM 向けに
  この field を既にパースしており、0.33.3 がフラグ無しで埋めるように
  なっただけ。ただし実測では、ヒット直後のプロンプトに追記した要求が
  cached_tokens 0 を返した。再利用の**深さ**の尺度としてはまだ使えない
  (#1125 / #1127 はこの点を踏まえること)。
- エンジン自身の既定コンテキスト窓はホスト依存になった (`vram-based
  default context`)。agent は常に `OLLAMA_CONTEXT_LENGTH` を export する
  ので waired が名乗る窓は 1 つも動かないが、`ollamaContextFloor` の doc は
  「範囲の一端」を指す文に狭めた。
- 深度ベンチのキャッシュは EngineVersion でキーされる (#1131) ので、
  全ホストが一度ミスして測り直す。旧エンジンの値なので、それが正しい。
- waired-agent#1192 は `min_engine_version` 0.33.3 を宣言できる。
- waired-agent#1233 が Windows ROCm 判定の測定を持つ。`amdROCmSupportedRes`
  のコメントは、bump ごとの点検手順をリストの隣に置き、release notes
  でなく overlay を読むよう次の担当者に指示する形に書き換えた。
- 変更ファイル: internal/runtime/ollama_version.go (定数と changelog
  段落)、internal/runtime/ollama_backend.go (コメントのみ、regex 不変)、
  internal/gateway/convert.go と cmd/waired-agent/inference_ollama_tuning.go
  (doc を狭めた)。docs-site の製品出力引用を 0.33.3 へ:
  reference/cli.md と ja/reference/cli.md、troubleshooting.md と
  ja/troubleshooting.md (この 2 つは 0.32.15 で止まっていたので同じ手で
  揃えた)、getting-started/doctor.mdx と ja ミラー。テストのリテラル
  "0.33.2" → "0.33.3" (ベンチキャッシュのテストと他 2 つ — pin を記述して
  いるが定数を import していないので、放置すると製品がもう pin して
  いない版を記述し続ける)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1193
- https://github.com/waired-ai/waired-agent/issues/1192
- https://github.com/waired-ai/waired-agent/issues/1233
- https://github.com/waired-ai/waired-agent/issues/885
- https://github.com/waired-ai/waired-agent/issues/1125
- https://github.com/waired-ai/waired-agent/issues/1127
- https://github.com/waired-ai/waired-agent/issues/1131
- https://github.com/waired-ai/waired/issues/1312
- https://github.com/ggml-org/llama.cpp/pull/27742
- docs/decisions/20260829/1600-move-both-engine-pins.md
- docs/decisions/20260816/2024-qwen3-8-takes-the-27b-band.md
- docs/knowledges/20260906/0230-ollama-pin-0333.md
