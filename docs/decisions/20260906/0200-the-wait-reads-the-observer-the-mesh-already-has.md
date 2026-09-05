---
status: accepted
---

# ピア脚の稼働判定は、選択が既に読んでいる観測を読む (20260906 02:00)

## Status
Accepted。`docs/decisions/20260828/0143-peer-leg-waits-while-the-peer-works.md`
の「時間でなく稼働で待ちを決める」裁定はそのまま。判定の入力が 1 つ増えるだけで、
猶予・上限・3 つの理由・即時中止の規則はいずれも変わらない。

## Context

`0143` はピア脚の pre-first-byte を「ピア自身に訊く」ことにした。訊く先は
`/waired/v1/inference/healthz` で、`engine_ready` かつ `capacity_used >= 1` を
「そのピアは今まさに推論の仕事をしている」と読む。

waired-agent#1220 は、その読みが成り立たない実機の形を測った。dispatch 後に
ピアの vLLM を `SIGSTOP` すると、**300 秒 0 バイト・ログ行ゼロ**でターンが宙吊りに
なる。打ち切ったのはクライアントで、ゲートウェイはまだ待っていた(上限 30 分)。

原因は、ピア側に engine の観測者が 2 人いて、要求側のウォッチが読んでいるのが
**再観測されない方**だったこと。

| 観測者 | 実装 | 凍った engine を |
|---|---|---|
| **live** | `runLocalInferenceProbe` の tick が `state.HeartbeatInterval`(5 s)ごとに `GET <engine>/health` を 1 s タイムアウトで撃つ(`cmd/waired-agent/inference_probe.go`)。2xx のときだけ `InferenceState.Reachable` | **1 tick で気づく** |
| **latched** | `/healthz` の `engine_ready` ← `servingAdapter().Health().State`。両アダプタの `superviseChild` は**プロセス終了しか待たない**ので、停止したプロセスはデーモンが生きている限り Ready のまま | **永久に気づかない** |

live な観測は既にメッシュに配られていて、**選択はそれを読んで新しいターンを断って
いた**(`buildMeshCandidates` / `pinReachableInSnapshot`)。#1220 の「選択の時点で
1.9 ms の名指し 400」がその証拠である。要求中のウォッチだけが、それを見ていなかった。

進捗(トークンが動いているか)はワイヤに無い。`capacity_used` は占有であって進捗では
なく、正当な prefill は数分無音になり得る(0143 の実測で 30k トークンに 9 分 10 秒)。
したがってバイト進捗は判別に使えず、使えるのは「engine が今答えるか」だけである。

## Decision

**`classifyPeerWork` に、要求側が既に持っている観測を渡す。**

`gateway.Deps.PeerFacts` を新設し、mesh snapshot からそのピアの `Name` と
`InferenceState.Reachable` を読む。`/healthz` が「働いている」と答えても、
**この端末がそのピアの engine を「答えていない」と見ているなら `peerIdle`** とする。
理由と文言は既存の `peer_stopped_serving`(「the peer X stopped working on this
request after N」)をそのまま使う。凍った engine について真であり、docs にも既にある。

`/healthz` の `engine_ready` は変えない。読み手が 12 か所あり、そのうち選択の
`IsReady` は既に map の live な bit で守られている。1 s のプローブが負荷で 1 回
外した瞬間に、それら全部が同時に誤るのは割に合わない。

**知らないことは証拠にしない。** `PeerFacts.Known` が false のとき(dep 未配線 /
ピアが snapshot に居ない / この端末が既に stale と見ているフレーム)、判定は
この引数が存在しなかったときと 1 バイトも変わらない。この端末が制御プレーンとの
接触を失ったことで、他人のターンが終わってはならない。

## Consequences

- **実測(sv-macmini → sv-mag、vLLM 0.28.0 / gpt-oss-20b)**
  - before: 300 s 0 バイト(クライアントが先に諦めた)
  - after: **60.4 s** で `400 waired_cannot_serve` / `X-Waired-Local-Error:
    peer_stopped_serving` /「The peer sv-mag stopped working on this request
    after 1m0s.」
  - 正当な cold prefill(110k トークン、TTFB 17.5 s)は**中断されない**
- **ヒステリシスは足さない。** 平滑化は既に aggregator の Policy にある
  (`internal/inferencemesh/aggregator.go`、waired-agent#323)。0143 の
  「『何もしていない』と答えたら即中止」はそのまま維持する
- **1 s のプローブは負荷では揺れない。** 110k トークンの実 prefill 中に engine の
  `/health` を 0.5 s 間隔で 41 回引いて、**全部 200・最大 0.92 ms・失敗ゼロ**
- **伝播は速い。** 凍結 → 要求側の snapshot が `reachable: false` を見るまで
  **約 4〜6 秒**、解凍 → 復帰も約 5 秒(実測)
- **この修正で捕まらない形が残る。** engine の HTTP 面が答え続けたまま何も進まない
  もの — GPU ハング、ollama の親が生きたまま runner が凍る形(waired-agent#29 が
  記録した形の停止版)。実在する: 並行レーンが同日、Metal の OOM
  (`kIOGPUCommandBufferCallbackErrorOutOfMemory`)中の ollama が `/api/chat` に
  **200 と零値の body** を返し、`/v1` の `usage` が全部 0 になるのを実測している。
  `/health` を引くこの修正も、ステータスだけ見るアサートも、どちらも沈黙で通る。
  判別には要求単位の識別子か進捗の指標が要り、0143 の Consequences が
  「区別する必要が出たら」と書いたのはこの軸である。30 分の上限はその場合の
  最後の囲いとして残す
- **フリートが混在する間は残る。** 更新前のピアは凍っても `engine_ready: true` を
  返し続けるが、`Reachable` は同じように配られるので、この修正は**要求側だけ**の
  更新で効く

## Refs
- waired-ai/waired-agent#1220
- `docs/decisions/20260828/0143-peer-leg-waits-while-the-peer-works.md`
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`
- `docs/knowledges/20260906/0230-peer-engine-liveness-measurements.md`
