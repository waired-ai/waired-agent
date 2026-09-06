---
status: accepted
---

# 製品文言の規約: 有名製品の人間が書いた文字列に合わせる (20260907 03:27)

## Status

Accepted

## Context

CLI(`waired`)・Waired アプリ(`waired-tray`)・NAVI の文字列は、機能ごとに別々の
セッションが書き足してきたため、同じ画面の中で流儀が割れていた。cobra の `Short`
は 67 本すべてが末尾ピリオド付きで、警告の接頭辞は `warn:` / `warning:` /
`logout: warning:` の 3 通り、省略記号は `…` と `...` が混在、`emo()` の ASCII
フォールバックは同じ ⚠ に `!` と `[!]`、同じ ✅ に `*` と `[ok]` が付いていた。
アプリのメニューは `Recent activity`(sentence case)と `Open Admin Console…`(title
case)が並び、同じ画面を製品内で「the Waired console」「browser dashboard」「web
console」「Admin Console」の 4 通りに呼んでいた。

オーナーは 2026-09-07 に「公開 docs(waired-agent#1264)と同じ手順で、有名 SaaS の
人間が書いた文言を横に置いて書き直す」と指示した。参照したのは Microsoft Writing
Style Guide、Apple Human Interface Guidelines(Writing / Menus / Alerts)、Material 3
の Writing、Shopify Polaris、Atlassian Design System、Command Line Interface
Guidelines(clig.dev)、GitHub CLI の Primer 規約、Heroku CLI style guide、および
Tailscale / GitHub CLI / Docker / Ollama / Vercel / Cloudflare の実際の CLI・トレイ・
コンソール文字列(ソースと公式 docs から逐語で採取)。日本語は SmartHR Design System、
Apple の日本語 HIG、Microsoft / Google の日本語 UI、Bitwarden / Mattermost / Signal /
VS Code の人手翻訳ペア。

## Decision

オーナー決定(2026-09-07):

1. **sentence case** をラベル・ボタン・見出し・メニュー項目のすべてに使う。Apple
   の title case は macOS のアプリメニュー限定の慣行で、Docker / Tailscale / Ollama
   のトレイは sentence case。
2. **短縮形を使う**(`can't` / `doesn't` / `isn't`)。Microsoft / Polaris / Atlassian
   が推奨し、Ollama / Tailscale の実文字列も使う。面の中で混ぜない。
3. 名詞は **computer**(本文でその物理機。`this computer`、`Your other computers`)/
   **device**(コンソールの登録エンティティ。Devices ページ、`Add device`)/
   **node**(ルーティング・プロトコルの出力のみ。`Waired node`)。`machine` は
   使わない(`machine-wide` は設定スコープの語として残す)。Tailscale が machine /
   device / node を面で分けているのと同じ型。日本語は「コンピュータ」「デバイス」
   の 2 層(サーバーも登録されるので「パソコン」にしない。公開 docs の「パソコン」
   は既存裁定のまま)。
4. アプリの `Quit` はそのまま `Quit`(Tailscale systray と同形)。
5. `Open Admin Console…` は **`Open Waired console…`** にし、製品内でこの画面を
   **the Waired console** と呼ぶ(アプリ文言の PR で出荷)。
6. NAVI 日本語の和欧間スペースは詰める(SmartHR / Apple 型。公開 docs の決定と一致)。

規約(上の決定と参照コーパスから):

- 省略記号: アプリではウィンドウを開く行に `…`(Apple HIG「さらに情報が必要な
  項目」)。CLI は ASCII の `...` だけ(gh / Ollama / Vercel / Heroku の進行表示)。
  `cmd/waired/ascii.go` の折り畳み表が `…` を `...` に倒すので、Windows の
  非 UTF-8 コンソールでも同じ見え方になる。
- cobra の `Short`: 文頭大文字、動詞先頭、末尾ピリオドなし(gh / Tailscale / Ollama /
  Heroku)。
- 警告は `Warning:` の 1 形(Tailscale / Ollama)。エラーは `waired: <message>`
  のまま(`Error:` 接頭辞はどの製品も使わない)。
- `emo(symbol, fallback)` のフォールバックは `ascii.go` の折り畳み表と一致させる
  (`TestEmoFallbacksMatchTheFoldTable`)。
- `please` / `sorry` / `invalid` / `Oops` を使わない(Microsoft / Google / Apple /
  Atlassian / Polaris)。エラーは「何が起きたか」と「どうするか」を 1〜2 文で。
- ボタンは動詞(+目的語)。結果を伴う操作に `OK` / `Yes` / `No` を使わない(OS が
  ボタン名を変えられない Windows の `MessageBoxW` の凡例は例外)。
- お知らせは名詞句の題(句点なし)+ 一文の本文(Tailscale の message catalogue)。
- 「tray」は書かない。アプリは the Waired app(CLAUDE.md §Documentation)。
- 日本語(NAVI): ボタンは動詞の終止形でサ変は「する」を省略(「追加」「削除」
  「デバイスを削除」)、説明文は敬体で動詞まで書く、「！」「？」「（）」は全角、
  削除確認は「〜しますか？　この操作は元に戻せません。」、エラーは
  「〜できませんでした。」+ 対処、空状態は「〜はまだありません」(SmartHR)。

守り方: `scripts/ci/vocabulary-guard.py`(規則は `scripts/ci/vocabulary-rules.txt`、
免除は同じ行の `// vocab: <why>`、未使用の免除は失敗)が `cmd/waired`・
`internal/gui/tray`・`internal/notice`・`internal/management/public_use.go` の
文字列リテラルを読む。裁定の記録先は従来どおり `docs-site/TRANSLATION.md`(この
記録の要約を `## Product copy conventions` として置く)。

## Consequences

- 製品文字列の変更は、公開 docs の逐語引用と同じ PR で動く(CLAUDE.md
  §Vocabulary and provenance)。この記録を出荷する PR は CLI の機械的な部分
  (`Short` のピリオド、`Warning:`、`...`、フォールバック表、tray → the Waired app)
  と、それを引用する docs を同時に変える。
- TRANSLATION.md で逐語裁定済みの行(`/model` の項目名、`Share this computer` 系、
  `Status…`、状態記号、お知らせの題と本文、statusline の語、router の理由行)は
  この記録では動かない。動かすときは行の更新を同じ PR に入れる。
- 残る面(init の文言、status / doctor / models、アプリのメニューとダイアログ、
  NAVI)は waired-agent#1277 / waired#1329 の PR で、オーナーの before / after 承認の
  あとに適用する。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1277
- https://github.com/waired-ai/waired/issues/1329
- https://github.com/waired-ai/waired-agent/pull/1264
- docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md
- docs-site/TRANSLATION.md
