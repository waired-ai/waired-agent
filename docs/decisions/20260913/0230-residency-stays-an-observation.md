---
status: accepted
---

# residency は観測のままにする —— それでターンを断らない (20260913 02:30)

## Status

Accepted（オーナー裁定 2026-09-13）。waired-agent#1314 が「設計は未決」として
並べた 4 案のうち **案 3（`LocalResidency` が absent のとき、非ストリームの要求を
待たずに断る）を採らない**ことを決める。`internal/gateway/server.go` の
`Deps.LocalResidency` に書かれている「Observation only」は、この記録を出典として
そのまま残る。

## Context

`LocalResidency` は、このデバイスのエンジンが (V)RAM に何を持っているかの
**最新の観測**である（waired-agent#879）。ローカルの probe ループが
`state.HeartbeatInterval`（5 秒）ごとに `/api/ps` から取り込み、gateway は
リクエストの記録に `resident` / `absent` / `other` と書き残すためだけに読む。

waired-agent#1314 は、非ストリームの脚がコールドロードの間ずっと無音で、
Claude Code がそれを 300 秒で諦めて無限に再試行することを記録した issue である。
当時この脚には「待つ」という実効的な選択肢が無かった —— 待っても客が先に帰る ——
ので、本文は案 1（何もしない）と案 3（早く正直に失敗する）を並べ、
「1 と 3 のどちらを採るかはオーナー判断」と書いた。

その後 waired-agent#1331 が脚を塞いだ。
`docs/decisions/20260912/2200-nonstream-leg-commits-late-and-aborts.md`。

## Decision

**採らない。** 理由は 4 つあり、最後の 1 つが決定的である。

### 1. 「丁寧に断る」ための手段が存在しない

こちらの文面を**逐語で、リトライ無しで**画面に出せるステータスは **400 だけ**
（`docs/knowledges/20260904/0210-claude-code-status-codes-for-gateway-errors.md` の実測）。
503 は 3 回リトライされた上に「サーバ側の一時的な問題」という**こちらの意図と違う
診断**を足され、404 は文面ごと捨てられ、429 は「レート制限」に化ける。
そして **400 はターンを殺す** —— 30 秒待てば答えられたはずのロードでも。

### 2. 述語が、必要な意味を持っていない

「常駐していない」は「待っても無駄」を意味しない。両方向に外す:

- keep_alive が切れて降りただけの小さいモデルは**数秒で戻る**。これを断るのは退行。
- 観測は最大 1 ハートビート古い。waired-agent#1307 は **`engine_ready` の時点で
  `ollama ps` が空**を実測しており、「載っている」と言いながら実際は載っていない
  こともある。

ターンを終わらせる判断を、この精度の読みに載せられない。

### 3. 本当に望みの無いケースは、既に別の根拠で即座に失敗している

- エンジンが park されている → `EnsureRunning` が落ちて数ミリ秒で 503
  （2026-09-12 の sv-macmini で実測: 9 ms）。
- エンジンが入っていない / 選択が解決しない → `runtime_unavailable` /
  `runtime_unhealthy` で dispatch の前に終わる。

**residency が識別できる「望みの無いケース」は残っていない。** residency が
`absent` と言うとき、それはたいてい「これから数秒で載る」状態である。

### 4. 動機そのものが消えた

案 3 の根拠は「どうせ待っても無駄だから」の一点だった。#1331 の後、非ストリームの
脚は 4 分黙ってからコミットして本文を埋め、**実測で 885 秒保持してもクライアントは
諦めない**（上限が見つかっていない）。実機でも 95 秒かかったターンが完走した
（sv-macmini、`docs/knowledges/20260912/2130-nonstream-leg-held-only-by-committing.md`）。
待つことが無駄でなくなった以上、早く断る理由が無い。

## Consequences

- `Deps.LocalResidency` の「Observation only — nothing in the gateway decides on
  it」はそのまま。**この記録がその出典になる**ので、次に同じ案を思いついた人は
  検討を最初からやり直さずに済む。
- 長いコールドロードに対して gateway が返すのは、今後も**待ちと、埋められた本文**
  であって、拒否ではない。
- 再検討の条件を 1 つだけ挙げておく: クライアント側の締切が変わり、
  **どう埋めても待ちきれない**ことが実測で示された場合。そのときも問いは
  「residency で断るか」ではなく「何を根拠に断るか」から始まる —— 上の 2 は
  そのとき再び当たる。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1314
- https://github.com/waired-ai/waired-agent/pull/1331
- `docs/decisions/20260912/2200-nonstream-leg-commits-late-and-aborts.md`
- `docs/knowledges/20260912/2130-nonstream-leg-held-only-by-committing.md`
- `docs/knowledges/20260904/0210-claude-code-status-codes-for-gateway-errors.md`
