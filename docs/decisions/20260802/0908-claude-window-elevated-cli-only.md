---
status: accepted
---

# Claude Code に伝える窓は昇格 CLI が書く。daemon には持たせない (20260802 09:08)

## Status
Accepted

## Context

managed settings の `CLAUDE_CODE_MAX_CONTEXT_TOKENS` は、`claude-` で始まらない
/model directive id（`anthropic-waired-local` / `anthropic-waired-auto`）の
コンテキスト窓を決める。#52 はここに静的な `250000` を書いていたが、実際に
サーブできる窓はホストによって遥かに小さい（実測例: 32768）。

#332 の調査で、Claude Code の gateway model discovery は資格情報がないと発火
せず、subscription OAuth のユーザーには `/model` に Waired のエントリが一度も
出ていなかったことが判明した。#407 でエージェント側が picker のキャッシュを
書くようになると、**ユーザーは初めて `anthropic-waired-local` を選べるように
なり、250000 という嘘を信じる**。#408 はこの値を実窓から導出する。

導出そのものに争点はない。争点は **refresh の所有者**で、#408 の初稿は
「daemon が serving model の変更時に managed settings を書き直す」としていた。

## Decision

**書き手は従来どおり昇格した CLI プロセスのみ。daemon には書かせない。**

`waired claude enable` / `waired init` の書き込み時に、gateway の `/v1/models`
が `anthropic-waired-local` に対して返す `max_input_tokens`
（= `Deps.ContextWindowFor` = min(manifest の native 窓, 実際に適用した tuning)）
を読み、その値を書く。解決できなかった場合（agent 停止中、active model なし）は
**既存の値に触れない** — 検証できない数字を別の検証できない数字で置き換えない。

理由:

1. **受理済みの決定に反する。** `docs/decisions/20260728/1444-init-daemon-path-owns-claude-routing.md`
   §4 と waired#935 が「daemon を特権ブリッジにしない」を決めている。書き込み系の
   management API は認証のない unix socket（mode 0666）/ ローカル対話ユーザーに
   開いた named pipe（`internal/platform/localipc/localipc.go` — "It performs NO
   peer authentication"）なので、daemon に root 所有の machine-wide ファイルを
   書かせると、任意のローカルユーザーがその書き込みを発火できることになる。
   このファイルは `hooks.Stop` にコマンド文字列も持つ。
2. **Linux では技術的に不可能。** systemd unit は `User=waired`
   （`internal/platform/service/service_linux.go`）で `/etc/claude-code/` に
   書けない。実装するには daemon を root に上げるか setuid/polkit helper を足す
   ことになり、`service_darwin.go` が記録した「Linux は sandbox が効くので専用の
   非特権ユーザーに価値がある」という判断を反転させる。macOS/Windows だけ実装する
   のは cross-OS parity 規則違反。
3. **買えるものが小さい。** managed settings の `env` は Claude Code の
   **プロセス起動時にのみ**適用され、監視も再読込もない
   （`docs/knowledges/20260714/0241-claude-code-context-window-internals.md` 実測）。
   daemon refresh でも走行中セッションは直らない。効くのは「モデル変更 →
   Claude Code 再起動」だけ。
4. **残るズレは waired#1031 が構造的に消す。** 窓を契約として固定し、モードを
   名乗るノードは実際にその窓でエンジンをロードしている、という設計になれば
   追随する値そのものが無くなる。期限付きの問題のために恒久的な権限姿勢の変更は
   払わない。

   **訂正（20260802、waired#1031 着地後）**: 理由 4 は**半分しか当たっていな
   かった**。#1031 は auto ディレクティブについては期待どおり env を不要にした
   — `claude-waired-auto` / `claude-waired-auto[1m]` は `claude-` 接頭辞と
   `[1m]` サフィックスで窓が決まり、`CLAUDE_CODE_MAX_CONTEXT_TOKENS` を一切
   参照しない（#438 / #445）。しかし `anthropic-waired-local` は**意図的に**
   非 `claude-` のまま残した。200k / 1M のどちらでもない窓を報告できる唯一の
   id であり、契約を名乗れないノードにピンする逃げ道だからである。つまりこの
   env は消えず、書き手が昇格 CLI である以上、下の Consequences が言う
   「次の enable まで古いまま」も消えない。`waired claude status` の
   `local window:` STALE 行は**恒久的に必要**であって、#1031 で削除できる
   ものとして数えてはならない。

## Consequences

- serving model や適用 tuning が変わると、次に `sudo waired claude enable` /
  `waired init` を実行するまで値は古いままになる。**これは黙って起きてはならない**
  ので、`waired claude status` に `local window:` 行を追加し、実窓と managed
  settings の値を並べ、食い違うときは STALE と復旧コマンドを表示する。
  docs-site の該当箇所（Claude Code ガイド / troubleshooting）にも同じ導線を書いた。
- 実窓超過は従来どおり gateway の合成 400（`internal/gateway/anthropic.go`）が
  毎リクエスト捕まえる。ここは変わらない。
- 値がホスト依存になったため、feature-off / disable 時に「waired が書いた値か」を
  定数一致では判定できなくなった。`wairedOwnedMaxContextTokens` は
  「pre-#408 の定数」か「今このホストで解決した窓」に一致するかで判定する。
  enable 時と別のモデルが動いている状態で off にすると、その 1 キーは残る。
  managed settings は operator/MDM も所有するファイルなので、所有印を書き込む
  よりは残す方を選んだ。base URL が消えている以上このキーは無害。
- `claudemanaged.Remove()` は `RemoveWithOptions(RemoveOptions{})` になった。
  `waired claude disable` は解決した窓を渡す（best-effort）。

## Refs
- https://github.com/waired-ai/waired-agent/issues/408
- https://github.com/waired-ai/waired-agent/issues/332
- https://github.com/waired-ai/waired-agent/issues/407
- docs/decisions/20260728/1444-init-daemon-path-owns-claude-routing.md（本決定が
  依拠する制約。supersede ではなく、その §4 を別の書き込み対象に適用したもの）
- docs/knowledges/20260714/0241-claude-code-context-window-internals.md
- waired-ai/waired#1031（窓の契約化。auto については refresh 問題を消したが、
  local ピンについては消えない — 上の訂正を参照）
- waired-ai/waired#935 / waired-ai/waired#1002
