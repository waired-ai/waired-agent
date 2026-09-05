# `/model` picker をもう一度実機で測る (20260906 03:40)

## Issue

`docs/knowledges/20260820/0300-model-picker-measured-on-device.md` が picker の
振る舞いを実機で測ったのは、行が Claude Code の private cache から読まれていた頃。
waired-agent#1185 で書き先を `modelPicker` 設定に移すにあたり、当時の実測のうち
どれがまだ有効かを確かめる必要があった。**2 つは古くなっていた。**

## Learnings

Claude Code **2.1.261**。隔離した `CLAUDE_CONFIG_DIR` と、`ANTHROPIC_BASE_URL` を
向けた小さな Anthropic 形スタブ。picker は pty で `/model` を開いて画面を採取。
ケースごとに config dir とポートを分離。

### 1. `modelPicker` の行は `description` を持ち、描画される（**旧実測の訂正**）

`replaceBuiltInOptions` を未設定にした画面:

```
❯ 1. Default (recommended) ✔  Use the default model (currently …) · $5/$25 per Mtok
  2. Opus (1M context)        Opus 5 with 1M context · …
  3. Sonnet                   Sonnet 5 · …
  4. Sonnet 5 (1M context)    …
  5. Haiku                    …
  6. ZZ-STATIC-CLAUDE         desc-static-claude
  7. ZZ-STATIC-SLASH          desc-static-slash
  8. ZZ-NO-DESC               Custom model (waired/nodesc-PROBE)
```

- 旧実測の「description フィールドは無い / どの行も "From gateway" になる」は
  **キャッシュ経由の行の話**で、`modelPicker` の行には当てはまらない。
  description 無しの行は `Custom model (<id>)`。
- したがってピア行は「ノードがラベル、モデルが description」に分けられる。
  以前は両方をラベルに詰め込む必要があった。
- カタログ外の id の行は Dropped にも Grayed out にもならず、そのまま残る。
- 60 行 × 200 桁では 8 行でも畳まれない（旧実測の「約 10 行で畳む」は端末サイズ次第）。

### 2. `CLAUDE_CODE_MAX_CONTEXT_TOKENS` は `claude-` で始まる id には効かない

バンドルの述語:

```js
function EU(e){ let t=St(e),r=er(t),o=Fe(r);
  if($me(o)||r!==t&&$me(Fe(t)))return!1;      // カタログにある → false
  let d=r.toLowerCase();
  return !d.startsWith("claude-")||d!==o }     // ← ここ
```

`claude -p --model <id>` を 4 条件で:

| id | `CLAUDE_CODE_MAX_CONTEXT_TOKENS` | カタログ外の注意書き |
|---|---|---|
| `claude-waired-local-PROBE` | 未設定 | 出る |
| `waired/local-PROBE` | 未設定 | 出る |
| `claude-waired-local-PROBE` | 131072 | **出る**（env が無視された） |
| `waired/local-PROBE` | 131072 | **出ない** |

注意書きは `--debug` のログではなく **TUI の画面**に出る。逐語:
`"<id>" isn't described by this version's model catalog; update Claude Code, or
map it with behavesAs on a modelPicker row (or modelOverrides, if it is a
provider id of a model this version knows). Until then auto-compact keeps this
session within 200k tokens (the context window it assumes); …
CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1 restores the previous
wait-for-the-API behavior.`

「未知モデルの窓の強制」は新しい挙動（その env の説明が "restores the previous …
behavior" と書いている）。つまり `claude-` 頭の Waired 行は今日の Claude Code で
この行を画面に出している。

### 3. 1M は `[1m]` サフィックスで、接頭辞とは無関係

`/context` で実際の窓を読み、スタブでワイヤを記録:

| 行の id | env | セッションの窓 | `anthropic-beta` | ワイヤ上の model |
|---|---|---|---|---|
| `waired/local-PROBE[1m]` | 未設定 | **1M**（999.5k free） | `context-1m-2025-08-07` | `waired/local-PROBE` |
| `claude-waired-peer-PROBE[1m]` | 未設定 | **1M** | `context-1m-2025-08-07` | `claude-waired-peer-PROBE` |
| `waired/local-PROBE` | 1000000 | **1M** | （1m beta なし） | `waired/local-PROBE` |
| `waired/local-PROBE[1m]` | 131072 | **1M**（`[1m]` が勝つ） | `context-1m-2025-08-07` | `waired/local-PROBE` |

`[1m]` は id から剥がされる（#1036 の再確認）。1M であることは
`anthropic-beta: context-1m-*` にしか残らない。

### 4. `behavesAs` は在るが、公開 docs には無い

`modelPicker` の行スキーマは `{ model, label?, description?, behavesAs? }`。
`behavesAs: "claude-sonnet-5"` を足すとカタログ外の注意書きが**消え**、
**送信される model id は変わらない**（実測）。ただし settings-reference /
managed-settings / sub-agents / llm-gateway-protocol / model-config のどれにも
記述が無い（取得して 0 件）。バイナリのスキーマ説明と、上のユーザー向け
メッセージ本文にだけ存在する。`modelOverrides` は文書化されているが
「既知モデル id → プロバイダ固有 id」の向きで、合成 id には合わない。

### 5. SessionStart フックの書込は同じセッションに効かない（**旧実測の反転**）

旧実測は「キャッシュはプロセスごとに 1 回読まれ、SessionStart はその読取より前に
走る」。settings は逆で、起動時に読まれ**その後**に監視が張られる:

| 書き手 / 時刻 | 同じセッションの `/model` に出るか |
|---|---|
| SessionStart フック（同期） | いいえ |
| フック（1 s / 2 s / 3 s 遅延） | いいえ |
| フック（6 s 遅延）/ 外部プロセス（15 s） | はい |

裏を返すと、**settings の変更はセッション途中でも拾われる**。キャッシュには無かった
性質で、将来メッシュ変化でデーモン側から書くなら即時に効く。

## Applied

`docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md`。
`scripts/ci/claude-code-canary.sh` の Part 2 は、キャッシュの schema を測るのを
やめて (a) waired が書く lineup が parse されること (b) 上の 2 の述語がまだ成立して
いること、の 2 レグになった。(b) は非対称 —— waired の綴りに注意書きが出たら FAIL、
`claude-` の対照に出なくなったら WARN（上流が強制をやめただけかもしれず、それは
waired にとって無害だが、述語を測り直す合図になる）。

## Refs

- `docs/knowledges/20260820/0300-model-picker-measured-on-device.md`（当時の実測）
- https://code.claude.com/docs/en/settings-reference#modelpicker
- https://code.claude.com/docs/en/managed-settings
- waired-ai/waired-agent#1185, #1177, #1036, #830
