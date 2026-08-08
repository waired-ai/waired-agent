---
status: accepted
---

# エンジンが引いた分は、使えるターンにならなくても計上する (20260807 16:48)

## Status
Accepted

## Context

ゲートウェイの記録は、応答が始まった後に失敗したターンを 2 通りに書いていた。

- `/v1/chat/completions`（openai）— `rr.fail(http.StatusOK, "mid_stream_truncate")`。
  `emitUsage` は status だけで門を切るので、サンプルは `Deps.OnUsage` に届く。
- `/anthropic/v1/messages`（ストリーム）— `rr.fail(http.StatusBadGateway,
  "engine_truncated_stream")`。502 なのでサンプルは落ちる。

同じ物理事象（提供先のエンジンが、クライアントが読み始めた後に死ぬ）が、ゲストの
叩いた API の違いだけで逆に計上されていた。Anthropic 側は `scanner.Err() != nil` を
`truncated` として拾うので、「中身が使えない」だけでなく「回線が切れた」もこの分岐に
落ちる。

より重いのは、**計上されない側が最も働いた側**だったこと。`proxyAnthropicStream` は
捨てた再試行のトークンを意図的に畳み込んで `setUsage` している。waired-agent#458
（`bcc9701`）のコミットメッセージがその理由を書いている:

> Usage is metered with the abandoned attempts folded in — the engine really did
> that work, and leaving it out would make a model that needs three tries look as
> cheap as one that needs none.

同じコミットが選んだ 502 が、その合計を `emitUsage` で捨てていた。宣言された意図に
実装が届いていない状態で、方針の対立ではない。

さらに `:832` は、waired-agent#538 が全経路に広げた「記録はクライアントが受け取った
status を述べる」から取り残された唯一の箇所でもあった。`w.WriteHeader(http.StatusOK)`
は同じ関数の手前にあり、その先で status を取り消す手段は HTTP に無い。手前の失敗
（TTFB 超過 / transport / 非 2xx）は全て pre-commit で、クライアントに書いた status を
そのまま記録している。

## Decision

`engine_truncated_stream` を `rr.fail(http.StatusOK, reason)` として記録する。
reason の値は据え置き。

- **記録する status はクライアントが受け取ったもの**（#538）。失敗であることは
  error reason が担う — `observability.Recorder` の error 判定は
  `Status >= 400 || ErrorReason != ""`、WARN は `ErrorReason != ""` 起点なので、
  Prometheus の `result=error` ラベルも event ring の `error_reason` も WARN も
  変わらない。#458 が 502 の根拠に挙げた「success として記録されると誰も調べない」は
  reason だけで担保される。
- **計上の線は「エンジンが仕事をしたか」であって「クライアントが答えを使えたか」では
  ない**。post-commit の 2 つの結果はどちらも仕事をした側にある。
- reason 値は 2 つのまま（`mid_stream_truncate` は「バイトが止まった」、
  `engine_truncated_stream` は「中身が使えない」という別の観測）。揃えるのは status と
  計上の可否だけ。

## Consequences

- Public Share の使用量レポート（spec §12）が、エンジンが実際に引いた分を報告する。
  再試行を畳み込んだトークンがサンプルまで届くのはこの変更が初めてで、
  `TestGateway_AnthropicUnusableTurnIsMeteredWithRetriesFolded` が試行ごとに異なる
  トークン数を使って end-to-end で pin している。
- ストリーム途中でゲストが切断したターンも計上対象に入る。openai レグでは以前から
  そうなっている挙動に揃うだけ。`peerVerdict` の Ctrl-C ガード（waired-agent#281）は
  `rr.ctx.Err()` を見ているので不変。
- `peerVerdict` の結果は変わらない（200+reason / 502+reason のどちらも
  `ok=false, charge=true`）。ルーティングへの影響は無い。
- event ring の `status` が 502 → 200 に変わる。クライアントに送ったものと一致する。
- `emitUsage` の門を「error reason があれば落とす」に広げる案は、これで**両レグ**の
  テストが落ちるようになった。以前は openai 側しか守られていなかった。

## Refs
- https://github.com/waired-ai/waired-agent/issues/554
- https://github.com/waired-ai/waired-agent/pull/458 (`bcc9701`, #442 修正)
- https://github.com/waired-ai/waired-agent/pull/553 (#538 — 記録はクライアントが受け取った status を述べる)
- https://github.com/waired-ai/waired-agent/pull/112 (waired#829 — `emitUsage` の門)
- docs/decisions/20260727/1755-engine-liveness-from-error-replies.md
