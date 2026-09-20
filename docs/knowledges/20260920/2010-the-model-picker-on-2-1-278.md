# `/model` picker を 2.1.278 で測る (20260920 20:10)

## Issue

`docs/knowledges/20260906/0340-the-model-picker-measured-again.md` §5 は、
Claude Code **2.1.261** で「settings は起動時に読まれ、その後に監視が張られる」
ことを測った。waired-ai/waired-agent#1454 の修正は、その監視に**同じ内容の
書き直しでも反応するか**に全面的に乗る。乗ってよいかを確かめる必要があった。
ついでに `CLAUDE_CONFIG_DIR` が settings.json をどこへ移すかも測った
（waired-ai/waired-agent#1457）。

## 方法

**waired も実機も要らない。** 開発機 1 台（Linux、WSL2）で足りる。

- ケースごとに使い捨ての `HOME`。中に `.claude.json`
  （`hasCompletedOnboarding` / `bypassPermissionsModeAccepted` /
  `projects.<cwd>.hasTrustDialogAccepted`）と `.claude/settings.json`
  （`modelPicker` に探触用の 1 行）。
- `ANTHROPIC_API_KEY` ではなく **`ANTHROPIC_AUTH_TOKEN`**、`ANTHROPIC_BASE_URL` は
  ローカルのスタブ（`GET` に `{"data":[]}`、`POST /v1/messages` に短い応答）。
  **スタブが無いと TUI は 15〜20 秒で落ちる** — 起動はするので、`/model` を
  20 秒後に開く形の測定は「画面が読めない」ではなく「シェルのプロンプトが
  返っている」という形で空振りする。
- 親の `CLAUDECODE` を `env -u` で落とす。`CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN=1`。
- tmux 200x50、**`bash --noprofile --norc`**。ログインシェルのままだと、起動処理
  （keychain など）が終わる前に `send-keys` した行が食われ、コマンドが走らない。
  プロンプトが出たことを目印で確かめてから打つ。
- 書き込みは temp+rename（`internal/platform/secrets` の `WriteFile` と同じ形）。
- 打鍵は「文字列 → 1 秒 → Enter を別送」。

## Learnings

### 1. 監視が張られるのは起動の 4〜5 秒の間（2.1.261 の 3〜6 s と一致）

内容を変える書き込みを 1 回だけ打ち、十分あとで `/model` を開く:

| 書き込みの時刻 | 同じセッションの `/model` | 同時刻のディスク |
|---|---|---|
| 1.1 s | 古いまま（18 s / 48 s とも） | 新しい |
| 3.1 s | 古いまま（28 s） | 新しい |
| 4.1 s | 古いまま（28 s） | 新しい |
| 5.1 s | 新しい（28 s） | 新しい |
| 6.1 s | 新しい（28 s） | 新しい |
| 8.1 s | 新しい（28 s / 48 s とも） | 新しい |

`/model` を開き直しても読み直さない。届くかどうかは**書いた時刻だけ**で決まる。

### 2. 同じバイト列の書き直しでも監視は発火する

1.3 秒に正しい内容を書き（SessionStart hook の代役。届かない）、そのあと
**まったく同じバイト列**をもう一度書く:

| 二度目の書き込み | 同じセッションの `/model` |
|---|---|
| 8.1 s | 新しい値（3 回とも） |
| 20.1 s（15 s に一度 `/model` を開いて古い行を見た後） | 新しい値 |

**内容の差ではなくファイルのイベントに反応している。** 旧 private cache のような
プロセス内メモ化も無く、一度描画したあとでも入れ替わる。waired-agent#1454 の
再公開はこの事実に乗っている。

### 3. `CLAUDE_CONFIG_DIR` は settings.json ごと移す（マージしない）

`CLAUDE_CONFIG_DIR` を別のディレクトリに向け、`modelPicker` の行を置く場所を変える:

| 行を置いた場所 | `/model` |
|---|---|
| `~/.claude/settings.json` だけ | **Waired の行が 1 つも出ない** |
| `$CLAUDE_CONFIG_DIR/settings.json` だけ | 出る |
| 両方 | `$CLAUDE_CONFIG_DIR` 側が出る |

`claudecode.SettingsPath()` は `~/.claude` 固定だったので、設定している利用者には
picker 行も status line も subagent の項目も一度も届いていなかった
（waired-ai/waired-agent#1457）。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1454
- https://github.com/waired-ai/waired-agent/issues/1457
- `docs/knowledges/20260906/0340-the-model-picker-measured-again.md` §5
- `docs/decisions/20260920/2000-the-rows-are-written-again-once-the-watch-is-armed.md`
