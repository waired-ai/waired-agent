# サブエージェントのヘッダと `_FORCE` を実機で測る (20260906 03:50)

## Issue

waired-agent#1186 は、サブエージェント判別を waired 自作のモデル id
(`waired/subagent`)から Claude Code 自身のヘッダに移す。issue 本文は
`x-claude-code-agent-id` と `x-claude-code-parent-agent-id` を挙げ、
「attribution にだけ使い、ルーティングには使わない」と書いている。
**どちらのヘッダが実際に来るのか**、そして
`CLAUDE_CODE_SUBAGENT_MODEL_FORCE` が本当に要るのかは測っていなかった。

## Learnings

Claude Code **2.1.261**。隔離した `CLAUDE_CONFIG_DIR` と、`ANTHROPIC_BASE_URL`
を向けた Anthropic 形スタブ。スタブに `Agent` ツールの `tool_use` を返させて
本物のサブエージェントを起こし、届いたリクエストのヘッダを記録した。

**スタブでサブエージェントを起こす手口**: 最初に `tools` を提示してくるターン
(タイトル生成は `tools` が空なので自然に除外される)に対して、
`{"type":"tool_use","name":"Agent","input":{"description":…,"prompt":…,
"subagent_type":…}}` を返す。ツール名は 2.1.261 では **`Agent`**(`Task` ではない)。

### 1. 来るのは `x-claude-code-agent-id` だけ

| # | リクエスト | `tools` | agent-id ヘッダ |
|---|---|---|---|
| 1 | タイトル生成 | 0 | なし |
| 2 | メインのターン | 22 | なし |
| 3 | **サブエージェント** | 13 | `x-claude-code-agent-id: a00c775f…` |
| 4,5 | ツール結果後のメイン | 22 | なし |

`x-claude-code-parent-agent-id` は**来ない**(トップレベルのサブエージェントでは)。
判別に使うのは `x-claude-code-agent-id` の非空。

### 2. `_FORCE` 無しでは、定義の `model:` が勝つ

`.claude/agents/pinned-probe.md` に `model: claude-opus-4-8` を書き、env は
`CLAUDE_CODE_SUBAGENT_MODEL=waired`:

| `CLAUDE_CODE_SUBAGENT_MODEL_FORCE` | サブエージェントのリクエストの `model` |
|---|---|
| 未設定 | **`claude-opus-4-8`**(定義が勝つ) |
| `1` | **`waired`** |

モデルを pin していないエージェント(`general-purpose`)では、`_FORCE` の有無に
かかわらず `waired` で届く。

→ 「サブエージェントを Waired に」で `_FORCE` を書かないと、**わざわざモデルを
指定されたエージェントにだけ静かに効かない**。上流の変更履歴も
「`CLAUDE_CODE_SUBAGENT_MODEL` を *override everything* から *default subagent
model* に変更 —— 定義の `model:` と明示的な per-spawn model が優先する」と
書いている。

## Applied

- `cmd/waired-agent/claude_selector.go` の `classifyClaudeClass(http.Header)`。
  `gateway.Deps.ClassifyRequest` / `intercept.Deps.ClassifyRequest` に改名して
  ヘッダを渡す。
- `internal/integration/claudecode/subagentplacement.go` は
  `waired` を選んだとき **2 つとも**書く。
- e2e に `claude-subagent-class` レグを新設(同じ id + ヘッダ)。ラベルが唯一の
  クラス経路だったので、ラベルを消すだけだとクラスの e2e が 0 になる。
- 決定: `docs/decisions/20260906/0343-subagents-are-placed-by-the-documented-knob.md`

## Refs

- https://code.claude.com/docs/en/llm-gateway-protocol#request-headers
- https://code.claude.com/docs/en/sub-agents
- `docs/knowledges/20260906/0340-the-model-picker-measured-again.md`(同じ手口の先行測定)
