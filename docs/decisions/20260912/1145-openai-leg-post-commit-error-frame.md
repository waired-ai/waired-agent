---
status: accepted
---

# OpenAI 面で commit 後に失敗したら、同じエンベロープを 1 本の data フレームで出す (20260912 11:45)

## Status

Accepted。`docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md` が
「OpenAI 脚は今日のまま。別 issue で起票する」と残した穴(waired-agent#952)を塞ぐ
にあたって、あの決定が「決めないと入れられない」と挙げた 4 点のうち
**4 点目**を決めるもの。1〜3 点目は実装とコメントで閉じている。

## Context

`proxyToEngine` はバイトパイプで、エンジンのヘッダをそのまま転送し、非 2xx の
ステータスと本文も逐語で中継する。keepalive を入れると最初のフレームで
**ステータスが使われてしまう**ので、その後にエンジンが非 2xx を返しても、
あるいはトランスポートが落ちても、今までの「逐語で中継する」が成り立たない。

Anthropic 面は `writeAnthropicErrorOrEvent` で答えを持っている
(commit 後は `event: error` 1 本 + 本当のステータスを `rr` に記録)。
OpenAI 面は方言が違うので、同じ答えをそのまま持ってこられない。

## Decision

**commit 前に書いたはずのエンベロープを、そのまま 1 本の `data:` フレームにする。**

```
data: {"error":{"message":"…","type":"upstream_error","code":"engine_error"}}
```

- **形**: `writeOpenAIError` が書く `openAIErrorEnvelope` と同一。この脚の
  クライアントは元々この本文を読めなければならないので、新しい約束が要らない。
  OpenAI 自身のストリーミング面も mid-stream の失敗をこの形で出す。
- **`[DONE]` は続けない。** あの番兵は「ストリームは終わった」と言う語で、
  ここで終わったのはストリームではない。
- **本当のステータスは `rr` に記録する。** クライアントが受け取ったのは 200 で、
  失敗であることは reason が担う —— `docs/decisions/20260807/1648` と
  waired-agent#538 が既に置いた線の、この脚での適用。
- **commit していなければ何も変わらない。** ステータスも本文もエンジンのものを
  そのまま中継する。`stream:false` の要求では keepalive を張らないので、
  非ストリームの経路はこの決定の範囲外。

### 併せて決めたこと: commit 後の失敗は `responseStarted=true` を返す

`proxyToEngine` の戻り値は「エンジンの応答がクライアントに届き始めていたか」で、
呼び出し元はそれで失敗の持ち主を決める(#538)。keepalive が commit した後は
届き始めているので、トランスポートが落ちても `true` を返す。`false` のままだと
呼び出し元が「エンジンが答える前に失敗」と記録し、既にストリームを読んでいる
クライアントについて嘘を書くことになる。

## Consequences

- **1 インターバル以内に答えたターンは 1 バイトも変わらない。** 代償を払うのは
  「ストリームを要求し、かつ keepalive のインターバルを 1 回待った」要求だけ。
- その要求については、エンジンのヘッダ(`Content-Type` 以外の付随ヘッダを含む)が
  中継されなくなる。エンジンが `text/event-stream` を返す場面なので、
  実務上の差はほぼ無い。
- オーバーレイ(`:9474`)は `StreamKeepalive` が 0 のままなので、この決定に
  一度も到達しない。ピアに配信する側が最初の 1 バイトを捏造してはいけない理由は
  `2142` と `cmd/waired-agent/inference.go` のコメントが持っている。

## Refs

- https://github.com/waired-ai/waired-agent/issues/952
- `docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md`
- `docs/decisions/20260807/1648-truncated-turn-is-metered.md`
