# Claude Code のコンテキスト窓・auto-compact 解決の内部実装 (20260714 02:41)

## Issue

waired#771 — managed settings の静的 `CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000`
が anthropic ルートの 1M セッションを 200k に潰していた問題の調査で、
Claude Code v2.1.207 のリリースバイナリから該当コードを抽出して確定させた
挙動のまとめ。公式ドキュメントだけでは確定できない(または誤解しやすい)点が
多い。メジャーアップグレード時は再検証すること(週次の `claude-code-canary`
ワークフローが文字列レベルの不変条件を監視している)。

## Learnings

- **窓の解決優先順位**(バイナリ内の resolver、ターン毎に評価):
  1. env `CLAUDE_CODE_AUTO_COMPACT_WINDOW` — `window = min(モデル窓, env値)`。
     **プロセス起動時に固定される唯一の層**。
  2. userSettings `autoCompactWindow`(`/autocompact` コマンドが書く)
  3. clientdata のモデル別チューン値(例: Sonnet 5 1M ≈ 967k)
  4. experiment → モデル既定 → auto(= モデル窓そのまま)
- **モデル窓はモデル id だけから決まる**: `[1m]` サフィックス → 1M、
  内蔵モデル表の native_1m → 1M、`CLAUDE_CODE_MAX_CONTEXT_TOKENS` は
  `claude-` で始まらない id のみ有効、既定 200000。id はターン毎に
  再評価されるため、`/model` 切替には env 以外の層は即追従する。
- **gateway model discovery は /model ピッカー専用**:
  `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` で起動時に
  `{base_url}/v1/models?limit=1000`(3 秒タイムアウト、リダイレクト拒否)を
  取得し `~/.claude/cache/gateway-models.json` に保存するが、使うのは
  `id` / `display_name` のみで、id は `^(claude|anthropic)` フィルタ付き。
  **`max_input_tokens` は compaction 窓に一切流れない**(#623 当時の調査
  メモは現行版では誤り)。
- **`max_input_tokens` を消費する機構は存在するが無効**:
  `~/.claude/cache/model-capabilities.json`(`models.list()` 由来、
  `{id, max_input_tokens?, max_tokens?}` スキーマ)がバイナリ内にあるが、
  ゲート関数がコンパイル時に `return false` 固定。現状の唯一の配線先も
  max **output** tokens の上限調整のみ。このゲートが有効化されたら
  waired のルート対応 /v1/models 広告が窓にも効き始める。
- **リアクティブ compaction の契約**: 400 応答本文が
  `/prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)/i` にマッチすると
  compact → リトライが発火し、キャプチャした数値差分で要約入力を削る。
  waired の合成 400(`internal/gateway/anthropic.go`)はこの文言に合わせて
  あり、`TestAnthropicMessages_OverflowMessageMatchesClaudeCodeParser` が
  Go 側からピン留めしている。
- **managed-settings の `env` ブロックはプロセス起動時のみ適用**。
  ファイル監視・再読込はない。セッション途中に効かせたい値の置き場には
  ならない。
- 抽出手法メモ: バイナリは Bun 製 ELF で `strings` だけではコード領域を
  取りこぼす。Python の `re.finditer` で対象文字列のオフセットを求め、
  前後数 KB をデコードして読むのが確実。

## Refs

- https://github.com/waired-ai/waired-agent/pull/11
- waired#771 / waired#623(調査経緯・リアクティブ層の分析は issue コメント)
- internal/integration/claudemanaged/managedsettings.go
- internal/gateway/anthropic.go / anthropic_context_window_test.go
- scripts/ci/claude-code-canary.sh
