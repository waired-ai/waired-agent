---
status: accepted
supersedes:
  - docs/decisions/20260819/1910-an-engine-floor-degrades-with-a-reason.md
  - docs/decisions/20260819/2140-no-engine-is-a-state-not-an-engine.md
  - docs/decisions/20260821/1308-engine-power-is-per-engine.md
---

# ユーザー向け文面は確立した LLM 用語で書く — "the AI engine" と内部名非表示の裁定を撤回 (20260822 20:29)

## Status

Accepted — オーナー指示 2026-08-22(セッション内。一次発言と用語マップは私設リポ
waired-ai/waired#1272 に逐語記録。対になる決定は同リポの
`docs/decisions/20260822/1931-user-copy-uses-standard-llm-terms.md` — リポ間の
supersede は guard が解決できないので、ここでは散文で指す)。
上の `supersedes` は**文言の部分のみ**: `20260819/1910` は §3(「ユーザー向け文面は
エンジンの内部名を出さない」)、`20260819/2140` は §4 の「AI エンジン」という語、
`20260821/1308` は「ユーザー向け文面に内部名を出さない」に従うとしたラベル名の箇所。
エンジン床の機械可読な理由、エンジン不在を状態として扱うこと、電源軸がエンジン単位で
あることは、すべて有効のまま。

## Context

2026-08-19 の #836 / #850 / #862 で、ユーザー向け文面は推論エンジンを "the AI engine"
と呼び、`ollama` / `vllm` を出さないことにした(TRANSLATION.md の「the AI engine」行、
「not available on this computer」行)。根拠は「`ollama` は人が選ぶものではなく、名前を
知っても操作に結びつかない」と、「対象ユーザーはローカル推論に詳しくない」という
読者前提(waired-agent#58 の平易化文言に始まり、waired#1108 で navi に広がった)。

2026-08-22 の再検討でオーナーが読者前提を変更した: 対象は「ある程度の知識を持った人」。
加えて「AI エンジン」のような一般に通用しない語は、造語した時点で誰にとっても分かり
にくい。調査すると製品は既に二層に割れていた — `waired runtimes install` の Short
(`Install an inference engine (ollama / vllm)`)、`waired doctor` の Subject
(`inference engine`)、gateway の 503、docs-site glossary の `Inference engine` 定義、
README は元から **inference engine + 実名**で、"AI engine" だったのは `init` /
`login_client` / `models_fit` / tray ダイアログの narration 層だけだった。

## Decision

1. ユーザー向け文面(CLI・tray・installer・docs-site)は確立した LLM / インフラ用語で
   書く。平易化のための造語はしない(CLAUDE.md §Vocabulary and provenance)。
2. 用語マップ(正本は waired#1272 と私設リポの決定。要点):
   AI engine / AI software → **inference engine**、AI model → **model**(総称 LLM)、
   local AI → **local inference**、Run AI models on this computer → **Run models on
   this computer**、session/context cache → **KV cache**、graphics memory → **VRAM**、
   graphics card → **GPU**、system memory → **system RAM**、
   not available on this computer / has no build of it → **no Ollama variant**、
   needs AI engine X → **needs Ollama X**、Keep model in memory / Model stays in
   memory → **Keep-alive**、per coding question / comfortable → **per request /
   target**、Measuring how fast this computer runs AI → **Benchmarking this computer
   with a small model**、first token(散文)→ **TTFT** の gloss、coordination service →
   **control plane**、helper machine → **worker**。
3. **エンジンの実名は出す。** 事実がエンジン固有の行(版の床、variant の有無、導入、
   `Engine:` 状態)は Ollama / vLLM を名指しする。`internal/router.EngineDisplayName`
   が表示名の単一ソースで、`engineFloorLabel` と no-variant の `DeficitLabel`
   (`no Ollama variant`)がそれを使う。CLI・picker・tray はこのラベルを**そのまま繰り返す**
   (「not available on this computer」で上書きしていた分岐を撤去)。
4. 変えない: `this computer`、model size の値、`Not recommended`、`allocatable`、
   `Unload model (free memory)`、`Model not loaded`、`(loaded)` / `(not loaded)`、
   `downloading`、`serving now`、`kept until unloaded`、`first token:` 行そのもの
   (20260821/1130 の決定は supersede しない)、識別子・フラグ名・ワイヤのキー。
5. 記録: `docs-site/TRANSLATION.md` の該当行を改訂し(Ruling 列に本決定と #1272)、
   新語(LLM / KV cache / variant / keep-alive / target)の行を足す。凍結記録は凍結のまま。

## Consequences

- 製品文字列 35 箇所、テスト pin 90 行、installer(sh/ps1)、installtest ハーネス、
  docs-site 12+ ページ(en/ja)、glossary の標準語追加が 1 PR で動く(製品出力と引用を
  同時に変える規則)。
- 旧方針を pin していたテスト 4 本(`state_catalog_test.go`、`tray_unfit_switch_test.go`、
  `init_model_picker_test.go`、`models_catalog_reason_test.go` の「variant / ollama を
  出さない」断言)は反転し、本決定を引用する。
- navi(私設リポ)は tray と byte-identical な判定文をマージ済みの Go から写す(PR-2)。
- docs-site の `quickstart.mdx` と `first-run.mdx` で食い違っていた同じ製品行の引用
  (`Starting the inference engine…` vs `Starting the AI engine…`)は前者に揃う。

## Refs

- waired-ai/waired#1272(批准元・用語マップ)、waired-ai/waired#1108(反転される裁定)
- #836、#850、#862、#473、#58
- `docs-site/TRANSLATION.md`、私設リポ dev-docs glossary の廃止語彙表
