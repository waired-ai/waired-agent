---
status: accepted
---

# 失敗したピア脚は発生源で名乗り、代替の無い脚だけが今そこで終わる (20260906 02:10)

## Status
Accepted。

## Context

ピアのデーモンが再起動している間に脚が飛ぶと、`postToEngine` の transport error は
**ミリ秒で**返る。ウォッチはまだ猶予(`ClaudeTTFBBudgetMainMs`、60 秒)を眠っている
最中なので、ヘルスチェックは 1 回も行われない。実測(sv-macmini → sv-mag、
`0de54fca`、脚が 13.3 秒飛んだところで `systemctl restart waired-agent`):

```
HTTP/1.1 502 Bad Gateway
X-Waired-Local-Error: engine_request_failed
{"type":"error","error":{"type":"upstream_error",
 "message":"runtime \"remote:dev_d6e2…\": Post \"http://<overlay>:9474/v1/chat/completions\": EOF"}}
```

2 つ別のことが同時に間違っている。

- **名前**。ピアに届かなかった失敗に、`engine_request_failed` という engine の名前が
  付いている。engine は 1 バイトも関与していない。
- **status**。5xx は Claude Code が最大 10 回リトライする。ただしそのリトライは
  **新しい POST** なので、ゲートウェイでは `selectAndProbe` がやり直される。

## Decision

**名前は常に発生源で付ける。status は代替が無いときだけ動かす。**

1. ピア脚の pre-commit transport error は `peer_unreachable` を名乗る。この端末が
   そのピアのオーバーレイ終端に届かなかった、という直接の観測である。プローブは
   撃たない — 1 回のヘルスチェックは証拠にならず(waired-agent#624)、4 つの verdict の
   うち 2 つは今日より**悪い**文を出す(「stopped working on this request」は、
   一度も始まらなかった要求について偽)。前例:
   `docs/decisions/20260904/0215-a-hangup-is-not-the-engines-failure.md`
   —— 名前は発生源で付ける、status は変えない。
2. **代替のあるピア脚は 502 のまま。** リトライが再選択になるので、10 秒で戻ってくる
   ピアはそれで黙って復旧する。`ErrPeersDidNotAnswer` が fail-closed 化のあとも
   503 + `Retry-After` のまま残っているのと同じ読み方である。
3. **ピン脚は名指しの 400 で今そこで終わる。** pin は代替されない
   (waired-agent#325)ので、リトライは今失敗したその 1 台に訊き直すだけになる。
   これは waired-agent#1180 が dispatch **前**の双子に既に下した裁定で、
   `internal/gateway/anthropic.go` の `ErrPinnedPeerUnreachable` の枝にそう書いてある。
   **文言は同じものを再利用する** — dispatch の前と後で、人の状況は同じだから。
4. **名指しは人が使う名前で。** `peerDisplayID` は自ネットワークのピアでは
   DeviceID を返すので、この 2 つの文はどちらも
   `dev_d6e2…` と名乗っていた。#1180 が「行動できない
   唯一の文字列」と呼んだものである。`Deps.PeerFacts.Name` で mesh snapshot から
   引く(Public Share のピアは名前を持たず、その擬名にフォールバックする)。
5. **ローカル脚は変えない。** `engine_request_failed` は発生源の名前として既に正しく、
   status は同じ理由で 502 のまま(次の試行の `EnsureRunning` が engine を起こし直す)。

## Consequences

- **実測(after、同じ 2 台)**: ピン脚の再起動は
  `400 waired_pinned_peer_unreachable` /「The computer this turn is pinned to,
  sv-mag, is not answering.」。生の dial エラーもオーバーレイアドレスも本文に出ない
- 反転する既存テストは無い。`internal/e2e/integration/budget.go` の
  `engine_request_failed` = 502 = `driveRetry` も、
  `internal/gateway/public_peer_display_test.go` の 3 面 502 も、どちらも**非ピン**
  なのでそのまま
- `notPeerFault` は変えない。`peer_unreachable` はピアに課金する — そこに到達
  できなかったのだから正しい
- **`X-Waired-Local-Error` の読み手はもう居ない。** #1198 が intercept のミラーを
  消したので、この値が届く先は observability の event ring とログ行だけになった。
  それでもここで名乗るのは、`rr.fail` が記録する理由と同じ語彙で staged される
  必要があるからで、ring は `waired doctor` と管理 API が読む

## Refs
- waired-ai/waired-agent#1171 / waired-ai/waired-agent#1180 / waired-ai/waired-agent#325
- `docs/decisions/20260904/0215-a-hangup-is-not-the-engines-failure.md`
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`
- `docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md`
