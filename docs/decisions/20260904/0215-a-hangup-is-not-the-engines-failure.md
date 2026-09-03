---
status: accepted
---

# クライアントが去ったことは、エンジンの失敗ではない (20260904 02:15)

## Status

Accepted。`docs/decisions/20260807/1648-truncated-turn-is-metered.md` が
「`mid_stream_truncate` は『バイトが止まった』、`engine_truncated_stream` は
『中身が使えない』という別の観測」と置いた線を、後者の内側へ延ばす。
元の決定(status と計上の可否)はそのまま生きている。

## Context

0.0.3-rc5 の実機検証で 2 本の issue が立った。waired-agent#1168
(「ollama 0.33.2 が散発的に 500 を返し、Anthropic へフォールバックする」)と
waired-agent#1179(「thinking だけの返答が `engine_truncated_stream` になる」)。
どちらも**同じ 1 つの事象**だった —— クライアントが待つのをやめたこと。

実機(16 GB M4、qwen3.5-9b)で確認した:

- `local_status_502` はエンジンの 5xx ではない。エンジンが本当に 5xx を返せば
  その status がそのまま通り、intercept は `local_status_500` と書く。502 は
  `postToEngine` の `client.Do` が**トランスポートエラー**を返した経路だけ。
- 50k トークンのプロンプトを投げた curl を 45 秒で切るだけで、#1168 が報告する
  ログの全行が再現した。ollama 側の `| 500 |` 行と `srv stop: cancel task` は、
  **こちらが切ったことの ollama 側の記録**だった。
- #1179 の一次証跡は rc5 全期間で 1 件だけで、その 1 件は `truncated=true` かつ
  `scan_err="context canceled"`。commit 前に切れたものが #1168、commit 後に
  切れたものが #1179。

コードベースはこれを既に知っていた。`requestRec.peerVerdict` は
`rr.ctx.Err()` が `context.Canceled` ならピア判定を捨てており、その
コメントが「切断は `engine_truncated_stream` / `engine_request_failed` /
`mid_stream_truncate` に化ける」と書いている。**回避策はピア選択の信号に
だけ**入っていて、メトリクス・イベントリング・WARN・利用者に見えるノート・
intercept のフォールバック記録はそのまま誤帰属していた。

## Decision

**inbound の要求がもう居ないなら、それはエンジンの失敗でもモデルの失敗でもない。**
発生源で `client_disconnected` と名前を付ける。

- `postToEngine` がエラーを返したとき、`ctx.Err() != nil` なら
  `engine_request_failed` ではなく `client_disconnected` を記録し、
  `X-Waired-Local-Error` にも載せる。openai レグの `mid_stream_truncate` も同じ。
  ADR 1648 が正した「同じ物理事象を、叩いた API の違いで別々に書く」を作り直さない。
- 切れた要求は**再送しない**。実測で、リトライの POST は同じ死んだ context の下で
  0.5 ms 後に失敗していた。
- 切れたターンに**見えるノートを差し込まない**。読む相手が居ないうえ、内容
  (「別のモデルに替えろ」)が事実に反する。
- ひとつの文字列に潰れていた「使えないターン」を分ける:
  `engine_truncated_stream`(本当にバイトが早く止まった場合だけ)/
  `engine_thinking_only` / `engine_markup_only` / `engine_no_usable_turn`。
  判定直後の WARN は `thinking_only` と `engine_markup_only` を属性として
  既に持っていた —— 区別は在って、記録される理由だけが持っていなかった。
- **status は変えない**(ADR 1648)。記録する status はクライアントが受け取った
  もので、失敗であることは reason が担う。`emitUsage` の門も `reason != ""` の
  ままなので、捨てた再試行のトークンを畳み込む #458 の計上は不変。

## Consequences

- `waired claude status` の `last fallback` と tray の履歴が、誰も答えなかった
  ターンを「Anthropic が答えた」と言わなくなる(理由が
  `local_client_disconnected` と読める)。**フォールバックとして計上すること自体**を
  やめるのは intercept 側の仕事で、waired-agent#1184 が該当機構ごと撤去する。
- `engine_truncated_stream` を grep する人が、truncation だけを見るようになる。
- `notPeerFault` に `client_disconnected` が入る。`peerVerdict` の
  `rr.ctx.Err()` ガードは**残す** —— まだ他の理由(openai レグの
  `mid_stream_truncate` など、この変更が名前を付けていない経路)を覆っている。
- #1168 の本体、すなわち「auto ルートのローカル脚が keepalive を張らず、
  prefill の 4〜5 分間ワイヤが無音になる」ことは**この決定では閉じない**。
  `waitPolicyFor` はフォールバックできる脚に keepalive を張らない —— keepalive の
  1 バイトがストリームを commit してしまい、以後フォールバックできなくなるため。
  waired-agent#1184 が auto を撤去すると分岐が keepalive 側へ落ちるので、そこで
  実機の after を採り直して判定する。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1168
- https://github.com/waired-ai/waired-agent/issues/1179
- https://github.com/waired-ai/waired-agent/issues/1184
- docs/decisions/20260807/1648-truncated-turn-is-metered.md
