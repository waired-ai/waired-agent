---
title: コーディングツールのコマンド
description: waired link、unlink、claudeについて、ステータス行とサブエージェントのサブコマンド、廃止されたコマンドを実行したときの表示を説明します。
meta:
  audience: ターミナルで作業する人、画面のないパソコンを使う人
  needs: インストール済みのWaired
  time: 必要なコマンドを読むだけ
---

## <a id="waired-link-and-unlink"></a>`waired link`と`unlink`

```sh
waired link                  # 見つかったすべてのコーディングツールを設定する
waired link claude-code
waired link opencode
waired link openclaw
waired link --force all      # 変更がなさそうな場所も含めて適用し直す
waired link --dry-run        # 変更内容を表示するだけで何も変えない
waired unlink <agent>
```

`link`は、各ツールのユーザーごとの連携を書き込みます。Claude Codeにはスキル、OpenCodeとOpenClawにはプラグインです。ファイルはツールをインストールした時点で有効になるので、まだインストールしていないツールを連携しても問題ありません。`unlink`は`link`が追加したものだけを元に戻します。`link`が既存の設定ファイルを変更する必要があったのはOpenClawだけで、先に取ったコピーは残り、`unlink`がその場所を表示します。[OpenCodeから使う](/ja/guides/opencode/)と[OpenClawから使う](/ja/guides/openclaw/)を参照してください。

## <a id="waired-claude"></a>`waired claude`

```sh
waired claude status
sudo waired claude enable            # Claude CodeをWairedに向ける（initも行う）
sudo waired claude enable --no-statusline
sudo waired claude disable
```

`enable`と`disable`には管理者権限が必要です。資格情報は書き込まないので、claude.aiのサブスクリプションには影響しません。[Claude Codeから使う](/ja/guides/claude-code/)を参照してください。

組織がすでにClaude Codeを管理しているパソコンでは、`enable`は何も書き込まず、そのことを表示します。先にパソコン全体の設定ファイルを読み、`forceLoginOrgUUID`、`forceLoginMethod`、`forceLoginGatewayUrl`、`availableModels`、`modelPicker`、またはWaired自身のループバックアドレスでない`ANTHROPIC_BASE_URL`のどれかがあれば、自分以外の誰かがClaude Codeを設定したとみなします。[WairedがClaude Codeは組織が管理していると言う](/ja/troubleshooting/claude-code/#waired-says-claude-code-is-managed-by-your-organization)を参照してください。

`enable`はパソコン全体の設定と、自分の`~/.claude/settings.json`の`modelPicker`に`/model`のWairedの行を書き込みます。そこにあるWaired以外の一覧には触れません。既定のモデルは設定しないので、まだ操作していないセッションはClaude Codeの既定で始まります。`disable`は行、ステータス行、サブエージェントの設定を削除します。

セッションのモデルにかかわらずAnthropic APIに送られるリクエストが1種類あります。Claude Codeのautoモードが実行する安全確認で、各ツール呼び出しを続行してよいか採点する分類器です。そのモデルはClaude Code自身が選ぶので、Wairedが権限の判断を肩代わりすることはできません。Anthropicに届かないと、その確認は失敗します。

### <a id="what-status-prints"></a>`status`の出力

```
managed settings:   /etc/claude-code/managed-settings.json (present)
ANTHROPIC_BASE_URL: http://127.0.0.1:9472
expected base URL:  http://127.0.0.1:9472
gateway listener:   127.0.0.1:9472 (listening)
local window:       200704  (managed settings: 200704)
/model rows:        6 rows
                    /home/you/.claude/settings.json
statusline:         waired segment installed
subagents:          follow their own model
default model:      not set — Claude Code uses its own, which is a real Anthropic model
last request:       waired → Waired   (2 minutes ago)
last served:        2026-09-04T01:52:11+09:00 — qwen3.5-9b (peer sv-mag)
waired node:        auto (this device or a mesh peer)   (change with `waired worker`)
```

| 行 | 意味 |
|---|---|
| `managed settings:` | パソコン全体の設定ファイルと、その有無。 |
| `ANTHROPIC_BASE_URL:` | ファイルが指す先。ルーティングがオフなら`(not set)`、読めなければ`UNREADABLE — this file is not JSON waired can parse.`。 |
| `local window:` | このパソコンの推論エンジンが保持できるコンテキストウィンドウと、Claude Codeに伝えた値。食い違っていればその旨が表示されます。 |
| `/model rows:` | 設定ファイルにあるWairedの行数。または`not written`、`LEFT ALONE`（ファイルに独自の行がある）、`UNREADABLE`。 |
| `statusline:` | `waired segment installed`、`wrapping your existing statusLine`、`not waired (custom: …)`、`not installed`、または`installed but shadowed here by <file> (<scope> scope)`。 |
| `subagents:` | `follow their own model`、`on Waired`、または`LEFT ALONE — CLAUDE_CODE_SUBAGENT_MODEL=<value> is not waired's`。 |
| `default model:` | 新しいセッションが始まるモデルと、その送り先。 |
| `last request:` | 直前のターンが持っていたモデルID、そのIDが送った側、時刻。 |
| `last served:` | 何が、どのパソコンで答えたか。 |
| `waired node:` | Waired宛のターンを自分のどのパソコンが受けるか。`waired worker`で変えます。 |

`installed, but not in the form this computer runs`と表示される行は、ステータス行か`/model`の更新フックが別のOSのシェル向けに書かれていることを意味します。`sudo waired claude enable`で書き直されます。

### <a id="waired-claude-statusline"></a>`waired claude statusline`

```sh
waired claude statusline                 # Claude Codeが呼ぶのと同じ表示を出力する
waired claude statusline install         # ~/.claude/settings.jsonに表示を追加する
waired claude statusline install --wrap  # 既存のstatusLineを飛ばさずに包む
waired claude statusline remove          # Wairedの表示を削除する（包んだものは戻す）
```

`enable`がこの表示をインストールします。`--wrap`は、既存のステータス行を置き換えるのではなく包みます。`disable`は自分のステータス行を戻して表示を削除します。[Wairedのステータス行](/ja/guides/claude-code/status-line/)を参照してください。

### <a id="waired-claude-subagents"></a>`waired claude subagents`

```sh
waired claude subagents            # 現在の設定を表示する
waired claude subagents follow     # 各サブエージェント自身のモデルが指す場所で動かす
waired claude subagents waired     # すべてのサブエージェントを自分のパソコンで動かす
```

このスイッチは自分の`~/.claude/settings.json`に書かれるので管理者権限は不要で、そのあとに起動した`claude`セッションに適用されます。

```
Subagents run on Waired (/home/you/.claude/settings.json).
Restart any running `claude` session to pick it up.
```

[サブエージェントの実行先を選ぶ](/ja/guides/claude-code/subagents/)を参照してください。

## <a id="retired-commands"></a>廃止されたコマンド

古いメモやスクリプトにはまだこれらの名前が残っていることがあります。それぞれ、何に置き換わったかを表示します。

| コマンド | 表示 |
|---|---|
| `waired claude route` | ``(removed) pick where a turn runs in Claude Code's /model.`` |
| `waired claude node` | ``(removed) use /model to choose a side and 'waired worker' to choose a node.`` |
| `waired claude fallback` | ``(removed) Waired never sends a turn to Anthropic on its own.`` |
| `waired proxy` | ``waired proxy was removed in favour of managed settings; use waired claude <enable|disable|status>`` |
