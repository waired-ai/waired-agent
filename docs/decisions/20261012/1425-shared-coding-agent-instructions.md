---
status: accepted
---

# 共通の開発指示を AGENTS.md に置く (20261012 14:25)

## Status

Accepted — オーナー指示 2026-10-12。

## Context

開発指示を `CLAUDE.md` だけに置くと、Codex の標準の指示ファイル探索では読まれない。
worktree に移る説明も Claude Code 専用のツール名を前提としていた。

## Decision

- 正本を通常ファイルの `AGENTS.md` に移す。`CLAUDE.md` には `@AGENTS.md` の
  import を置き、同じ本文を Claude Code・Codex・OpenCode から読めるようにする。
  Windows の clone でも使えるよう、symlink にはしない。
- worktree でコマンドを実行する手順は `cd`、作業ディレクトリ指定、`git -C` で記述する。
  Claude Code 専用ツールは利用できる場合の補助手段とする。
- 既存の決定記録が `CLAUDE.md` の節を参照するときは、対応する `AGENTS.md` の節を読む。
  Claude Code との製品連携を扱う記録は、そのクライアント固有の記録として読む。
- ローカルメモリの有無にかかわらず、知見は repo docs、進捗は issue / PR に記録する。

## Consequences

開発規則の本文は 1 つとなり、クライアントによる内容の食い違いを避けられる。
単独 clone では本リポジトリの `AGENTS.md` と docs を使う。ワークスペース内で作業するときは、
利用可能な共通指示も明示的に読む。各クライアントによる親ディレクトリの自動継承は仮定しない。

## Refs

- `AGENTS.md`
- `CONTRIBUTING.md`
- https://developers.openai.com/codex/guides/agents-md/
- https://opencode.ai/docs/rules/
- https://code.claude.com/docs/en/memory#share-one-file-with-other-coding-tools
- https://opencode.ai/docs/ja/rules/（日本語の参照資料）
- https://code.claude.com/docs/ja/memory（日本語の参照資料）
