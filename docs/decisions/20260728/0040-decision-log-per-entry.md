---
status: accepted
---

# 決定ログを 1 決定 1 ファイルに分割する (20260728 00:40)

## Status
Accepted

## Context

決定ログは単一の `docs/decisions.md` に新しいエントリを先頭へ積む形だった。
この形は並行する PR を全て同じ挿入点に集める。さらに全エントリが同じ見出し
（`### Status` / `Accepted` / `### Context`）を繰り返すため、行ベースのマージは
エントリの境界を認識できず、別々のエントリの断片どうしを整列させてしまう。

同じ問題を knowledge note は 20260509 に per-entry 化で解いており、決定ログだけが
単一ファイルのまま残っていた。

## Decision

- **1 決定 1 ファイル** `docs/decisions/YYYYMMDD/HHMM-<slug>.md` に分割する。
  `docs/knowledges/` と同じレイアウト。`HHMM` は 24h ゼロ埋め（時刻を持たない決定は
  `0000`）、`<slug>` は kebab-case ASCII（≤ ~44 字、日本語不可）、本文は日本語のまま。
- **単一ファイルの決定ログは二度と作らない。**
- 各エントリに機械可読な front-matter（`status`、任意の `supersedes` /
  `superseded_by`）を前置し、`scripts/ci/decision-log-guard.py` が CI で検査する:
  front-matter の存在と `status` の妥当性、`status` と `## Status` 散文の一致、
  supersede リンクの参照先実在と双方向の整合、ファイル名の形、そして
  `docs/decisions.md` が復活していないこと。
- 部分的な supersede もリンクする。どの部分が置き換わったかは `## Status` の散文が
  持ち、`status` はその散文に従う。

## Consequences

- 並行 PR は別ファイルを作るので、決定ログ由来の conflict が構造的に消える。
- front-matter は散文の射影であって二重管理ではない。ズレたら CI が落ちる。
- 分割前の決定を指す `docs/decisions.md` 参照は宙に浮くため、併せて張り替える。
  このリポジトリのコードやワークフローが引用している決定の多くは、リポジトリ分割
  (#184) より前に private 側で下されたものなので、参照先は `waired/docs/decisions/`
  になる。
- 詳細な経緯（破綻の再現条件、修復履歴）は内部ノートに記録する。

## Refs
- https://github.com/waired-ai/waired-agent/pull/267
