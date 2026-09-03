---
status: accepted
superseded_by:
  - docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
---

# auto mode の分類器は route に関わらず Anthropic が答える (20260828 02:21)

## Status
Accepted — ただし部分的に superseded: Consequences「オフラインで壊さない」（上流不達時に `passthroughWithLocalDegrade` がローカルへ降格する）は `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md` が置き換える（降格なし、不達は Claude Code に届くエラー）。分類器が route に関わらず Anthropic へ行く決定と、形による識別は有効。

## Context

Claude Code の auto permission mode は、ツール呼び出しごとに**別のモデル**を走らせて
「その操作を実行してよいか」を判定する。Anthropic のドキュメントは、そのモデルは
Claude Code が選び、利用者は設定できないと明記している(code.claude.com/docs/en/
auto-mode-config § Review denials)。

このリクエストは実 Anthropic の id(既定 `claude-sonnet-5`)を運ぶため directive では
なく、クラス `main` の route(既定 `auto`)に従って**ローカルへ配信されていた**。
waired-agent#1041 に測定を記録した。要点:

- 同一リクエストを 2 回投げて severity が **0 と 50**(Anthropic は 5)。Claude Code は
  この点数を**分類器モデルごとに固定したしきい値**(`claude-sonnet-5` なら `t1=25`)で
  読むので、同じ無害な操作が 1 回目は許可・2 回目はブロックになる。点を付けるモデル
  だけが入れ替わり、較正は Sonnet 5 のまま残る。
- 冷間 37.0 s(ローカルエンジンの数えで 26,611 トークンの prefill)。約 120 KB の
  プレフィクスは主会話 1 本(実測 181 KB)で追い出され、`OLLAMA_NUM_PARALLEL=1` の
  1 スロットに並ぶ。#1041 の実測 127 s はクライアントの上限 120 s を超え、**ツールが
  拒否される**。
- 分類器が答えられないことは中立ではない。未応答は fail-closed で拒否に落ちる。

id では識別できない。分類器リクエストが一度失敗すると、Claude Code はそのセッションの
残りを**セッション自身のモデル**で送り直す。Waired 上ではそれが directive id になり得る
(#1039 が `claude-waired-auto` を、#1041 が `claude-sonnet-5[1m]` を観測し、**どちらも
正しかった**)。

## Decision

**auto mode の分類器リクエストは、`waired claude route` の値に関わらず実 Anthropic API
が答える。`waired` も例外にしない**(オーナー裁定 2026-08-28、waired-agent#1041。
`waired` を例外にするか明示的に問うたうえでの裁定)。

- 識別は**形**で行う: `tools` キーが無く、かつ `stop_sequences` が非空。Claude Code が
  この面に送る他のどのリクエストもこの組み合わせにならない(主会話・タイトル生成・
  quota プローブ・モデル切替後の `Hi` のいずれも `stop_sequences` を送らない)。
  `metadata.user_id` の `session_id` は主会話と同一なので使えない。分類器のシステム
  プロンプト本文には**合わせない** — 版で変わる散文に経路を賭けることになる。
- 判定は **#52 の directive id より先**に行う。降格後の分類器は directive id を運び得る
  ため、id を先に見ると許可判断がこの機械に戻ってしまう。
- 読めない・オブジェクトでない本文は false(**fail-open**)。その場合はこの検査が無かった
  ときと同じ route を通る。
- この面(`Inference.ClaudeGatewayPort`)は Claude Code 専用。他のコーディング
  エージェントは一般ゲートウェイ(`LocalGatewayPort`)に来るので、この述語が他
  クライアントのローカル推論を横取りすることはない。

## Consequences

- **`waired` route の文言が変わる。**「never contacts Anthropic」は偽になるため、CLI
  ヘルプ・`claudeRouteHintText`・`/waired-route` スキルの説明・docs-site(en/ja)を
  同時に直した。例外が 1 種類のリクエストに限られること、Anthropic に到達できない
  ときはローカルに降格することを明記する。
- **オフラインで壊さない。** `routeAnthropic` は `passthroughWithLocalDegrade` に入り、
  上流に到達**できない**場合だけローカルへ降格する。全ツールが fail-closed で拒否される
  よりよい。
- 副次的に、分類器が `dispatchAuto` を通らなくなるので `observeMainModel` が
  `claude-sonnet-5` で汚れなくなる(#1036/#1037 と同根)。
- 費用は分類器 1 回あたり約 41,000 入力トークン(うち 40,415 はキャッシュ読み出し)。
  出力は 9 トークン。主会話が既に同じ会話を同じアカウントへ送っているので、**新たな
  情報開示は発生しない** — むしろローカル/ピア配信のほうが開示先を増やす側だった。

## Refs

- waired-agent#1041(裁定と測定)
- waired-agent#1039(分類器がツールを拒否する腕。`n_ubatch` の腕は別途)
- code.claude.com/docs/en/permission-modes, /auto-mode-config, /errors
