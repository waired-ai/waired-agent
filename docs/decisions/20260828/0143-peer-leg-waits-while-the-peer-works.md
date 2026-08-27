---
status: accepted
---

# ピア脚の「最初の1バイトまで」は時間でなく稼働で決める (20260828 01:43)

## Status
Accepted

## Context

`#757` はピア脚の pre-first-byte を**固定の締切**で囲った
（`ClaudeTTFBBudgetMainMs` 既定 60 秒 / sub 20 秒）。0.0.3-rc4 の実機レビュー
（waired-agent#1040）で、その数値が**測る対象を取り違えている**ことが分かった。

呼び出し側がピアの最初の1バイトまでに待っているのは、そのピアが
**プロンプトを prefill している時間**である。それはピアの計算速度と、
クライアントが送った文脈量の関数であって、到達性の関数ではない。
Claude Code の初回ターンは約 3 万トークンあり、レビュー時のフリートでは
4 台中 3 台がその 1 ターンに 60 秒を超えた（Strix Halo 164 秒 / M4 16GB 264 秒 /
24GB ディスクリートは OOM）。

結果、`auto` の帰結は**プローブがどのピアに当たったか**で変わった。正常に
働いているピアが prefill 3 分目で見捨てられ、メッシュが空いているのに
ターンが本来の Anthropic API へ行った。同じホストの `waired infer --explain`
はそのピアを問題なく並べている。

### オーナー裁定 (2026-08-28、waired-agent#1040)

> こういうクライアント側でのタイムアウトではなくて、peer 先からヘルス情報
> みたいなものを受け取って判断してはどうか。そもそも推論をリクエストしている
> ものを遅いからというだけで切るのは間違いではないだろうか。

## Decision

**遅さで切るのをやめ、ピア自身に訊く。**

ピアの `/waired/v1/inference/healthz` は**署名済み NetworkMap ではなく
オーバーレイの直接 HTTP** であり、proto を経由しない。そして、この端末が
投げたリクエストは、**そのピアの admission スロットを 1 つ保持し続ける** —
エンジン起動とモデルロードを含む（`internal/inference/server.go`、spec §8.2）。
つまり `engine_ready` かつ `capacity_used >= 1` は「そのピアは今まさに推論の
仕事をしている」というピア自身の申告になる。しかもこれは選択前のプローブが
既に叩いている口である。

`ClaudeTTFBBudgetMainMs` は**猶予期間**になる。

| 猶予の間 | ピアには何も訊かない。猶予内に答えるターンは今日と1バイトも変わらない |
|---|---|
| 猶予を過ぎたら | `peerLivenessInterval`（15 秒）ごとに `/healthz` を引く |
| 働いている | 待ち続ける |
| 「何もしていない」と答えた | **即 pre-commit 中止**、`peer_stopped_serving` |
| 2 回連続で答えない | **即 pre-commit 中止**、`peer_unreachable` |
| `/healthz` を持たない古いピア | **今日の挙動へフェイルオープン** — 猶予をそのまま締切として使い、理由は既存の `peer_ttfb_timeout` |
| `ClaudePeerWaitCeilingMs` に達した | 中止、`peer_ttfb_timeout` |

### 上限は 10 分（オーナー裁定 2026-08-28）

`claude_peer_wait_ceiling_ms` を新設、既定 600000、0 または猶予以下で無効
（＝固定締切のまま）。10 分はローカル脚が既に使っている数字
（`docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md`）なので、
**Waired のノードは、このパソコンであれ別のパソコンであれ同じ長さ待たれる**。

稼働の申告はどこまで行ってもピアの主張であり、主張は誤り得る。上限は
その一点のために在る。

### sub クラスは固定締切のまま（この PR で唯一の独断）

`ClaudeTTFBBudgetSubMs`（20 秒）は据え置き、`PeerWaitCeiling("sub")` は 0 を返す。

- その予算が短いのは「止まったサブエージェントは再ルートが安い」から
  （`internal/gateway/server.go` の #757 由来のコメント）であり、これは
  **代わりのある**ピアについて真である。
- Claude Code の補助リクエストは自前の 120 秒期限を持つ（waired-agent#1041）。
  稼働ウォッチで 10 分待たせると、#1041 の「ツールが拒否される」を悪化させ得る。

裁定されたものではない。覆すのは config 1 行である。

### 何を「言ってよいか」

観測した事実だけ。中止の理由は 3 つに分かれ、会話内通知もそれぞれ別の文を出す。

- `peer_ttfb_timeout` — 「応答が無かった」。時間しか観測していないときだけ。
- `peer_stopped_serving` — 「止まったとピアが言った」。
- `peer_unreachable` — 「答えなくなった」。

`peer_stopped_serving` を「応答が無かった」と書くのは、ヘルスチェックに
答えたマシンについての**誤った説明**である。だから値を分けた。

## Consequences

- **auto のターンが最大 10 分ピアを待ちうる。** 猶予内に答えるターンは不変、
  上限で囲い、通知は理由を名指し、`ClaudePeerWaitCeilingMs=0` で無効化できる。
- **猶予を過ぎたターンはオーバーレイ往復を 15 秒ごとに 1 回払う。** 3 分の
  prefill で十数回。相手はキャッシュ済み状態を返すハンドラである。
- **`capacity_used >= 1` は「このリクエストが」ではなく「何かが」動いている
  ことしか言わない。** 別のリクエストが入れ替わりに入っていれば区別できない。
  区別する必要が出たらリクエスト単位の識別子が要るが、今回は要らない —
  どちらにせよ、そのピアは推論の仕事をしている。
- **fallback 可の脚に SSE keepalive は armed できない**（`20260821/2142` の
  排他規則は不変）。したがって 10 分の待ちは**依然として無音**である。
  Claude Code 側の忍耐がそれより短ければ、上限 10 分は事実上届かない —
  実機で測り、waired-ai/waired#1280 に追記する。
- **`peerLivenessMisses = 2`。** 返ってこないプローブ 1 回はピアについての
  証拠ではない（waired-agent#624 のオーナー裁定と同じ理由）。
- **OpenAI 脚は今日のまま。** `#952` と同じ線引き。

## Refs

- waired-ai/waired-agent#1040
- waired-ai/waired#1280
- `docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md`
- `docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md`
