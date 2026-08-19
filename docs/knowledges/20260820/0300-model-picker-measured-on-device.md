# `/model` ピッカーの実測ファクト (20260820 03:00)

## Issue

waired-agent#830 の設計にあたり、Claude Code `2.1.233` を**実ホスト・実サブスク
リプション・実 waired ゲートウェイ**で走らせ、`/model` を tmux に描画して採った
測定。コードを読むだけでは確定できない点ばかりで、再測定にはそれなりの手間が
かかるので残す。

**サンドボックスは代表性がない。** 隔離した `CLAUDE_CONFIG_DIR` + ダミー API
キーで同じことをすると **Waired の項目が 1 つも描画されない**。実アカウントとは
Claude Code 内部の分岐が違う。この種の確認は実機でしか成立しない。

メジャーアップグレード時は再検証すること（週次の `claude-code-canary` が文字列
レベルの不変条件だけは監視している）。

## Learnings

### 1. 説明欄は使えない

キャッシュに `description` を書いても**剥落する**。ピッカーは全ての
gateway 由来の行に `From gateway` を固定表示する。**情報は `display_name`
1 本に収めるしかない。**

### 2. 同一プロセスで `/model` を開き直しても読み直さない

走行中に `~/.claude/cache/gateway-models.json` を書き換えて `/model` を再
オープンしても、内容は変わらない。ピッカーはプロセス内でメモ化している。
`#830` が要望していた「`/model` が叩かれるたびに最新」は**クライアント側の
制約で達成不能**。到達可能な鮮度は「起動ごと」。

### 3. ただし SessionStart フックはその起動に間に合う

ユーザー scope の `~/.claude/settings.json` に SessionStart フックを置き、
そのフックからキャッシュに歩哨エントリを追記したところ、**同じセッションの
`/model` にその行が出た**（`11. HOOKPROBE written by SessionStart`）。

つまり 2 と 3 は矛盾しない。読み込みはプロセスあたり 1 回で、SessionStart
フックはその 1 回**より前**に走る。「Claude Code を起動するたびに最新」は
フック経由なら成立する（次回起動に持ち越されない）。

### 4. `^(claude|anthropic)` フィルタはフェッチ経路だけ

- **ネットワーク discovery**（`GET /v1/models`）: 効く。接頭辞を持たない id は
  キャッシュに書かれる前に落ちる。`description` もここで剥がされる。
- **オンディスクのキャッシュ読み出し**: **効かない**。`waired-peer-noprefix`
  のような id もそのまま描画される。

waired はキャッシュを自分で書くので、綴りの制約は「フィルタを通るため」では
なく別の理由（下記 5）で選ぶことになる。

### 5. 接頭辞が決めるのは「セッションの窓を誰が決めるか」

- `claude-` 始まり: Claude Code が id 文字列だけで窓を決める（既定 200k、
  `[1m]` 接尾辞で 1M）。
- 非 `claude-` 始まり: `CLAUDE_CODE_MAX_CONTEXT_TOKENS` を使う。これは
  **単一のグローバル値**で、この端末の窓を持つ。

したがって「他のノードに流す」項目に非 `claude-` を使うと、**この端末の窓が
peer のセッション長として使われる**。常に誤り。

### 6. 10 行で折り畳まれる

組み込みモデルが 6 行あり、その下に gateway 由来の行が続く。合計が 10 行を
超えると `… +N models` に畳まれ、↑↓ でスクロールする。つまり **Waired の行は
実質 4 行しかスクロールなしで見えない**。項目を増やす設計では並び順が実際の
可視性を決める。

### 7. ルーティングを伴わない項目は黙った嘘になる

ピッカーに出ていない未知の id（`anthropic-waired-peer-<node>` 形）を
ゲートウェイに POST すると、**200 でローカル実行され、応答の `model` は要求した
id をそのまま返す**。UI 上は「その peer が答えた」ように見える。ピッカーに行を
足す変更は、ルーティング側を同じ PR で入れないと成立しない。

### 8. ライブの `/v1/models` は 37 件返す

ゲートウェイの `/v1/models` は 4 つの directive + `waired/default` + カタログの
全 model_id とそのエイリアスを返す。4 と併せると、**ライブ応答をそのまま
キャッシュに書くと生のカタログ id が 33 行ピッカーに並ぶ**。動的化するなら
書き手側で絞る必要がある。

## 測定手順

```sh
ssh <host> tmux new-session -d -s probe -x 200 -y 60 -c /tmp/probe
ssh <host> tmux send-keys -t probe "claude" Enter          # 信頼プロンプトに Enter
ssh <host> tmux send-keys -t probe "/model"; ... Enter
ssh <host> tmux capture-pane -t probe:0.0 -p
```

キャッシュや設定を差し替えるときは `cp -a` でバックアップを取り、終了時に
md5 一致まで確認して復元すること。

## Refs

- waired-ai/waired-agent#830 / waired-ai/waired#1223 / waired-ai/waired#1227 レーン L64
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md`
- `docs/knowledges/20260714/0241-claude-code-context-window-internals.md`
