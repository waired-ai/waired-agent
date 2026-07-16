# Claude Code の /model でバックエンド + 窓を切替える (route directives) (20260716 23:25)

## Issue

`/waired-route` の2つの体感問題:(1) context window が model id の文字列だけで
決まり `/model` 切替（または CC 起動）時にしか再解決されないため、`waired`
route でも `[1m]` モデルだと `/context` は 1M のまま（ローカルは実質 256k）。
route を変えても窓は追従しない。(2) route 切替はサーバ側は即時だが CC の UI が
backend を一切反映しないので効いて見えない。#52 はこれを「`/model` に Waired を
出し、選ぶと backend と窓が1アクションで揃う」opt-in 機能で解決する
（既存 `/waired-route` と並行動作、default off）。

## Learnings

- **窓は `/model` 切替が唯一の実務レバー**: 窓は model id から決まり、gateway が
  discovery の `max_input_tokens` で push しても効かず（picker 専用、CC v2.1.207
  ではコンパイル時 off）、env は起動時固定。実機でも route を `waired` に変えても
  `/context` は 1M のまま（`[1m]` モデル据え置き）で、route では窓は動かない。
  よって設計は `/model` 切替を窓レバーとして使う。詳細は
  `docs/knowledges/20260714/0241-claude-code-context-window-internals.md`。
- **予約 id を discovery で広告 + intercept が route 指令として解釈**:
  - `anthropic-waired-local` → intercept が **route=waired 強制**（厳格ローカル、
    fallback 無し）。非 `claude-` 始まりなので `CLAUDE_CODE_MAX_CONTEXT_TOKENS`
    （`claude-` 以外の id にだけ効く）が適用され窓 ~256k（値 250000）。
  - `claude-waired-cloud[1m]` → **route=anthropic 強制**。`[1m]` で窓 1M。
    passthrough で実 Anthropic モデルへ rewrite（`rewritePassthroughModel` を
    予約 cloud id にも拡張、`observeMainModel` は予約 id を無視）。
  - 優先順位: **予約 id > `/waired-route` の per-class policy**。通常 id は従来
    どおり policy に従う（並行動作）。
- **CC クライアント依存（要 on-device 検証）**: ①picker は discovery の id を
  `^(claude|anthropic)` で filter（→ 予約 id はこの prefix 必須、`display_name`
  は自由）②`[1m]` サフィックス→1M ③`CLAUDE_CODE_MAX_CONTEXT_TOKENS` は非
  `claude-` id のみ。いずれもドキュメント外の binary 挙動で canary
  （`scripts/ci/claude-code-canary.sh`、③の literal を追加）で監視。③が外れても
  安全側（既定 200k）に落ちる。**discovery 由来のカスタム id で窓が id 文字列
  から解決されるか**は本 PR 時点で未検証 → enable + CC 再起動 +
  `~/.claude/cache/gateway-models.json` 削除 → `/model` 選択 → `/context` で確認
  する gating。
- **#771 の再来ではない**: `CLAUDE_CODE_MAX_CONTEXT_TOKENS` は非 `claude-` id の
  窓を SET するだけで実 `claude-*` Anthropic セッションには無影響。1M を 200k に
  潰した #771 の `CLAUDE_CODE_AUTO_COMPACT_WINDOW`（min キャップ）とは別物。
- **opt-in の実装位置**: `agentconfig.InferenceConfig.ClaudeModelRouteDirectives`
  （env `WAIRED_INFERENCE_CLAUDE_MODEL_ROUTE_DIRECTIVES` / flag
  `--inference-claude-model-route-directives`、default off）。agent は intercept
  （`intercept.Config.ModelRouteDirectives`）+ gateway
  （`gateway.Deps.ClaudeModelDirectives`）に、CLI は managed-settings 書込
  （`claudemanaged.WriteWithOptions`）に伝播。全て shared portable コードで
  cross-OS 追加ファイル不要。
- **buffer コスト**: 予約 id 判定のため flag on 時は main path でも body を
  bounded buffer + parse する（off では従来の fast path 不変）。over-cap /
  unreadable / 非予約 id は fail-open で policy に落ちる。

## Refs

- #52（本機能） / #623（discovery + overflow 400） / #771（auto-compact 撤去）
- docs/knowledges/20260714/0241-claude-code-context-window-internals.md
- internal/proxy/intercept/{server.go,model_rewrite.go} /
  internal/gateway/anthropic_models.go /
  internal/integration/claudemanaged/managedsettings.go /
  internal/agentconfig/config.go
