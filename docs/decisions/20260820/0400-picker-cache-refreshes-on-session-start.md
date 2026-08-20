---
status: accepted
---

# picker キャッシュの更新は Claude Code の SessionStart フックが行う (20260820 04:00)

## Status

Accepted。オーナー裁定（2026-08-20、waired-ai/waired#1227 レーン L64）。
`docs/decisions/20260728/1444-init-daemon-path-owns-claude-routing.md` の
「書き込むのは昇格した CLI、daemon ではない」を**維持したまま**、更新の契機だけを
足す。裁定の途中経過として「Stop + SessionStart の両方」が一度選ばれたが、下記の
実測を受けて **SessionStart のみ**に確定した。

## Context

`/model` の picker エントリはユーザー所有の
`~/.claude/cache/gateway-models.json` から読まれ、これを書いているのは
`waired claude enable` / `waired init` の一度きり（waired-agent#407）。peer 別
エントリ（waired-agent#830）はフリートの現況を映すので、一度きりでは意味を成さない。

オーナーの要望は「`/model` が叩かれるたびに最新」だったが、**これはクライアント側の
制約で達成できない**。実測（docs/knowledges/20260820/0300-...）:

- Claude Code は picker キャッシュを**プロセスあたり1回**しか読まない。走行中に
  ファイルを書き換えて `/model` を開き直しても内容は変わらない。

daemon に書かせる案は成立しない。Linux の daemon は `User=waired`
（`packaging/systemd/waired-agent.service`）で他ユーザーのホームに書けず、書けたと
しても mode 0600 のファイルを本人が読めない。Windows / macOS では書けてしまうが、
それは `cmd/waired/claude_models_cache.go` が「a root-written file in the user's
home is a support ticket」として避けている当のもの。

## Decision

1. **更新は Claude Code の `SessionStart` フックが行う。** managed settings に
   1 行入れ、`waired claude _models-cache write --from-managed` を走らせる。
   行を書き込むのは昇格した CLI、走るのは**ユーザー権限のフック本体**なので、
   2026-07-28 の裁定が要求する所有権の形をそのまま満たす。

2. **Stop フックには相乗りしない。** picker はプロセス起動時にしか読まないので、
   毎ターン書き直しても読まれる回数は変わらない。実測で **SessionStart フックは
   その起動のキャッシュ読み込みに間に合う**ことが確認できた（歩哨エントリが同一
   セッションの `/model` に出た）ので、Stop 側は 0600 ファイルの書き換えを毎ターン
   増やすだけの純損になる。

3. **tray は採らない。** tray はユーザー権限で常駐しており所有権は正しいが、
   **`CLAUDE_CONFIG_DIR` を継承できるのはフックだけ**（`gatewaycache.go` は自
   プロセスの環境変数を読む）。tray や systemd timer は `~/.claude/cache/` に書き、
   Claude Code は `$CLAUDE_CONFIG_DIR/cache/` を読む、という**永久に成功する
   no-op** になる。加えて tray はヘッドレス機に居らず、picker の鮮度が GUI
   セッションの有無に依存することになる。オーナーの条件（tray 非依存）とも一致。

4. **変化があるときだけ書く。** `ReadGatewayCache` で比較し、同一なら書かない。
   起動のたびに 0600 ファイルを書き換えるのは、1 回しか読まれない値に対する churn で、
   同時に起動した 2 つのセッションのレースにもなる。

5. **フックは標準出力に何も出さない。** Stop フックの stdout は
   `{"systemMessage": ...}` の制御チャネル（`cmd/waired/claude_statusline.go`）であり、
   SessionStart の stdout はセッションのコンテキストに足される。成功メッセージは
   ユーザーの会話に貼り付けられてしまう。

6. **peer 上限はフックのコマンド文字列に埋め込む。** フックが machine-wide な
   `agent.json` を読みに行かないで済む。値を解決するのは既に昇格側なので、
   読み手を 1 つに保てる。

## Consequences

- 到達可能な鮮度は「**Claude Code を起動するたびに最新**」。docs にはそう書き、
  同時に「セッション中に `/model` を開き直しても更新されない」ことも書く。
- directives の opt-out 時はフックも外れる。出していないエントリを維持し続ける
  フックは、維持している対象が無い。
- フックは 2 本になった（Stop = フォールバック可視化、SessionStart = picker 更新）。
  OS ごとのシェル差（waired-agent#787）を二重に書かないよう、コマンド生成・設置・
  除去・状態読み出しは 1 実装に一般化し、Stop 側は薄い呼び出しとして残した。
- `waired claude status` は 2 本を別行で報告する。片方だけが未設置・実行不能な
  状態があり得るので、まとめるとどちらが壊れているか分からない。

## Refs

- waired-ai/waired-agent#830 / waired-ai/waired#1223 / waired-ai/waired#1227 レーン L64
- `docs/knowledges/20260820/0300-model-picker-measured-on-device.md`
- `docs/decisions/20260728/1444-init-daemon-path-owns-claude-routing.md`
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md`
