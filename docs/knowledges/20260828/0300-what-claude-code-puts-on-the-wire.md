# Claude Code がワイヤに何を乗せるか — model id・1M・既定・補助リクエスト (20260828 03:00)

## Issue

waired-agent#1036 / #1037（親 waired-ai/waired#1283 レーン L81）の設計判断を、推測ではなく
実測で決めるために採った。計測環境:

- ホスト sv-evox2（Windows、edge `0.0.3-edge.20260827153153+4117821`、ollama qwen3.5-122b-a10b）
- Claude Code **2.1.245**。`waired claude route` は `main: anthropic`（既存設定、変えていない）
- ワイヤ捕捉は `waired` リポの `scripts/dev/coding-agent-verify/capture_proxy.py` を
  `127.0.0.1:9482` に置き、managed settings の `ANTHROPIC_BASE_URL` を一時的にそこへ向けた。
  終了後にバックアップからバイト一致で復元済み
- 対話 TUI は `@microsoft/tui-test`（ConPTY）。§4 の分類器の 2 件だけは L82 が
  Claude Code 2.1.247 で採ったもの

## Learnings

### 1. `[1m]` はワイヤで剥がれる。1M はヘッダにしか残らない

| `/model` または `--model` | body の `model` | `anthropic-beta` に `context-1m` |
|---|---|---|
| `claude-fable-5[1m]`（settings 由来） | `claude-fable-5` | あり |
| `claude-waired-cloud[1m]` | `claude-waired-cloud` | あり |
| `claude-waired-auto[1m]` | `claude-waired-auto` | あり |

`claude-waired-cloud` は directive 表（`[1m]` 付きの綴りで完全一致）に当たらず、
**404 を現行 edge でそのまま再現した**（`--model 'claude-waired-cloud[1m]'` → 上流 404 ×2）。
2026-08-22 の窓ノート（private 側 `20260822/1507`）は窓だけ測ってワイヤの id を見ていない。

→ 表の照合は裸形で。1M は `anthropic-beta` から導出する。

### 2. Claude Code の既定は `claude-opus-5`

ユーザー設定から `model` を外して `claude -p` を実行すると、body は `claude-opus-5`
（+ 1M ベータ）。「既定のまま = 実 Anthropic の id が来る」ことの確認。

### 3. `/model` の選択はユーザー設定に即時書き戻される。managed settings は毎起動で引き戻す

対話 TUI で `/model claude-fable-5` を実行すると、画面はこう答えた:

```
⎿  Set model to Fable 5 and saved as your default for new sessions
      Managed settings pins claude-waired-auto — that applies on restart
```

- `~/.claude/settings.json` に `"model": "claude-fable-5"` が**その場で**書かれた
- managed settings の `model` は効く（そのセッションの最初のリクエストは
  `claude-waired-auto` で出た）が、**セッション中は `/model` が勝ち、再起動で戻る**

→ 既定を書くなら**ユーザー設定**。managed に書くと、別のモデルを選んだ操作者の選択が
毎セッション取り消される。

### 4. 主会話以外のリクエストが 3 種類あり、いずれもセッションのモデル id を運ぶ

1 セッション（同一 `session_id`）で捕捉した非主会話リクエスト:

| 形 | 中身 |
|---|---|
| `max_tokens:1`・tools 無し・system 無し・本文 `"quota"` | 起動時の枠確認 |
| `max_tokens:1`・system 127B・本文 `"Hi"` | モデル切替直後 |
| `max_tokens:64000`・tools 無し・system 3186B・本文 `<session>…` | セッションタイトル生成 |

**「tools が無い」だけでは補助リクエストを識別できない。**

### 5. Claude Code は自分でモデルを変える

Fable 5 のターンが向こう側の安全性判定で弾かれ、画面に

```
Fable 5's safeguards flagged this message. … Switched to Opus 4.8.
```

と出て、**`claude-opus-4-8` で再送された**（tools=62、system 15692B — 主会話と同じ形）。

→ ワイヤの id は「そのリクエストが何を要求したか」であって「ユーザーが何を選んだか」
ではない。id から永続設定を書くと、ユーザーが何もしていないのに設定が動く。

### 6. `metadata.user_id` は JSON 文字列で、中に `session_id` がある

```
{"device_id":"…","account_uuid":"…","session_id":"…"}
```

同一セッションのターンは共有する。**主会話と補助リクエストの区別には使えない**
（L82 の実測でも分類器は主会話と同じ `session_id` を運ぶ）。

### 7. 印字モードに auto 権限の分類器は存在しない

`claude -p "Use the Bash tool to run: echo hello …" --permission-mode auto` は Bash ツールを
実際に実行して成功したが、捕捉されたのは主会話の 2 リクエストだけだった。分類器は
対話セッションにしか無い（`scripts/dev/coding-agent-verify/README.md` の記述どおり）。

### 8. 分類器（L82 実測、Claude Code 2.1.247）

- 既定の id は **`claude-sonnet-5`**。セッションのモデルには追随しない
- **降格経路がある**: 最初の分類器リクエストが 401 以外で失敗すると
  `externalSonnet5Probe="demoted"` がセッション内でラッチし、以後は**セッションの
  メインモデル id** を運ぶ。waired-agent#1039 が `claude-waired-auto` を、#1041 が
  `claude-sonnet-5[1m]` を観測した矛盾はこれで説明できる
- 形の決め手は **`tools` 不在 + `stop_sequences` あり**（`["</severity>"]` 等）。
  §4 の 3 種はいずれも `stop_sequences` を送らない

### 9. 踏んだ罠

- **PowerShell の `function H` は `Get-History` のエイリアスを踏む。** 別名にする
- **`Set-Content -Encoding utf8`（5.1）は BOM を付ける。** managed settings に付くと
  waired だけが読めなくなる（waired-agent#1067、本ラウンドで発見・修正）。書き戻しは
  `[System.IO.File]::WriteAllText($p, $txt, (New-Object System.Text.UTF8Encoding($false)))`
- **`npm.ps1` は実行ポリシーで止まる。** `cmd /c npm …` を使う
- **tui-test の待ち条件に `>` を入れると cmd のプロンプトに当たる。** 11 秒で「1 passed」に
  なり、Claude Code は起動すらしていなかった。`windows/README.md` の「画面テキストで
  ターン完了を判定するな」の隣にある穴

## Refs

- waired-ai/waired-agent#1036 / #1037 / #1067
- `docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md`
- `docs/knowledges/20260820/0300-model-picker-measured-on-device.md`（同じ面の先行実測）
- waired の private 側 `docs/knowledges/20260822/1507-claude-code-window-for-directive-model-ids.md`
- 検証ハーネス: waired の private 側 `scripts/dev/coding-agent-verify/`
