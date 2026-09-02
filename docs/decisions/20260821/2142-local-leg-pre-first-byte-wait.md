---
status: accepted
superseded_by:
  - docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
---

# ローカル脚の「最初の1バイトまで」をどう扱うか (20260821 21:42)

## Status
Accepted — ただし部分的に superseded: Decision の表の auto 行（`LocalTTFBBudget` で pre-commit 打ち切り → Anthropic へ再ルート）と Consequences「ローカルのターンが auto ルートで端末の外へ出うる」は `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md` が置き換える（auto ルート撤去。全ローカル脚が「逃げ場のない脚」になる）。keepalive を SSE コメント行で書く決定と、その LOCAL 限定は有効。

## Context

waired-agent#837 の方針4。ローカルのエンジンがモデルをロードしている間、
コーディングエージェントには**1バイトも返っていなかった**。

- `internal/gateway/handlers.go` のエンジン用 HTTP クライアントは `Timeout: 0`。
- `ttfbBudgetFor`（旧）は remote 脚以外に必ず 0 を返していたので、ローカル脚には
  上限が存在しない。
- SSE ヘッダは `postToEngine` が返った**後**に書かれる。ollama は重みが常駐する
  まで応答ヘッダを出さないので、ロード全体が無音になる。

これは見た目の問題ではない。クライアント側（Claude Code）はバイト無着信の
ウォッチドッグと全体タイムアウトを持ち、失敗すると自動リトライする。ロードが
それより長ければ**クライアントが先に諦め、リトライがロードを最初からやり直す**。
#837 が実測した「420 秒で 0 バイト → 180 秒のリトライも 0 バイト → `/api/ps` が空」
はこのループである。

## Decision

**脚ごとに、その脚で合法な方の手当てだけを行う。** 既に製品が下している裁定
（#786、`internal/gateway/probe.go` の `capacityQueueBudget`）がそのまま当たる:
逃げ場のある脚は早く返し、逃げ場のない脚は待つ（待つ価値があるから）。
intercept は `X-Waired-Fallback-Allowed` で両者を既に分離している。

| 脚 | 早期に1バイト書けるか | 待ちを打ち切れるか | 採った手当て |
|---|---|---|---|
| auto（ヘッダ `=1`） | **不可** — `fallbackRecorder` は最初の `Write` でも `Flush` でも commit し、以後 `eligibleForFallback()` は永久に false（#580） | 可 | `Deps.LocalTTFBBudget` で pre-commit 打ち切り → Anthropic へ再ルート＋会話内通知 |
| waired / pin / :9473 / :9479（ヘッダ無し） | 可 — `dispatchLocal` は素の `w` に書く | **不可** — 「pinned local/waired-only leg is never aborted」(#757) | `Deps.StreamKeepalive` で SSE keepalive を書き、無音をやめる |

同時には決して両方立たない。`waitPolicyFor` がその排他を担い、テストが固定する。

### keepalive は SSE の**コメント行**にする

`: waired keepalive\n\n`。理由:

- コメント行は SSE 仕様自身の「無視される行」なので、`ping` を `message_start`
  より前に置いてよいかという**他人のクライアントの性質**に依存しない。
- 画面には何も出ない。`message_start` 以降の順序も乱さない。
- リポジトリ内の SSE リーダ（`internal/agentgrade` の `readAnthropicStream`、
  gateway の変換ループ）はどちらも `data: ` の無い行を読み飛ばす。

**最初の tick で初めて書く（t=0 では書かない）。** 1 インターバル以内に応答した
ターンは今日と 1 バイトも変わらない。`docs/decisions/20260727/1500-vllm-install-progress-from-uv-lines.md`
の「壊れず、静かに元に戻る」を満たす形。

上限は設けない。逃げ場のない脚で keepalive が期限切れになるのは、
「一度喋ってからまた黙る」ことであり、最初から黙っているより悪い。

### keepalive は LOCAL 選択にも限定する（対称性ではなく必要）

ピア脚の非 2xx には、`HeaderLocalError=context_overflow` を載せた窓超過 400 が
あり得る。`relayPeerContextOverflow` はそれを**ステータスとして**中継し、
Claude Code はその status で自動コンパクションを起こす。早期 commit すると
それが in-band エラーに化け、セッションは二度とコンパクトできない。ローカルの
エンジンはこのヘッダを作れない（窓ガードは dispatch 前に走る）ので、
ローカル限定にすることで危険を**構造的に**消している。

### 既定値は 10 分（オーナー裁定 2026-08-21）

`claude_local_ttfb_budget_ms` を新設し、既定 600000、0 で無効。ピアの
60s/20s を流用しない理由:

- サブエージェント用の短い予算が存在するのは「止まったサブエージェントは
  再ルートが安い」から。それは**代わりのある**ピアについて真であって、
  この計算機については偽。
- ここでの冷ロードは正当な待ちで、再ルートはユーザーが選んだローカル実行を
  奪う。だからクライアントがもう待っていないような長さでのみ打ち切る。

打ち切ったときは `Deps.OnLocalEngineAbandoned` から既存のバックグラウンド
warm を叩く。single-flight で `/api/ps` を先に見るので、再ルートが連続しても
ロードは 1 回で、**次のターンはローカルに戻る**。

### 何を「言ってよいか」

観測した事実だけ。`RequestEvent` に載せるのは
`ModelResidency`（`resident`/`absent`/`other`/空=見ていない）と
`EngineInflight`（この要求が**並んだ相手の数**、自分の admission slot を取る
**前**に読む）。「cold」「slow」「loading」とは書かない — waired-agent#912 が
未決なのは閾値が存在しないからであり、#866/#883 は**常駐したまま** 35 秒の
prefill を実測している。

`engine.log` を tail してロードの様子を語る案は採らない。`cappedWriter` は
ログの**先頭** 8 MiB を残して以降を捨てるので、上限超過後の tail は
プロセス最初期のテキストになり、しかもエラーにならない（別 issue で起票）。

## Consequences

- **ローカルのターンが auto ルートで端末の外へ出うる。** 10 分の既定、会話内
  通知、`route waired`/pin は構造的に無関係、`0` で無効、で囲っている。
- **held な脚では commit 後のエンジン失敗がステータスを失う**（in-band の
  `event: error` になる）。ローカル＋逃げ場なしの脚に閉じており、そこでは
  製品挙動を持つ唯一のステータス（窓超過 400）が発生し得ない。記録側は
  `rr.fail(実ステータス, 理由)` のままなので event ring は正しい。
- **`localModelObserver` の発火が早まる**: held なストリームでは最初の
  keepalive が commit になるので、`OnServed` はエンジンの最初のバイトより前に
  走る。「once, at commit time」という規則は変わっておらず、commit が動いた。
- **overlay リスナには絶対に配線しない。** あの面の選択は構造上すべてローカル
  なので、gateway 側のローカル条件では守れない。守っているのは「dep を配線
  しない」ことだけである。配線すると、提供側ピアが**エンジンが出していない
  最初のバイト**を呼び出し側に渡し、呼び出し側の #757 予算を無効化する。
- **OpenAI 脚は今日のまま。** `proxyToEngine` はエンジンのヘッダをそのまま
  転送するバイトパイプで、早期 commit は `Content-Type` の捏造を強いる。
  別 issue で起票する。
- #837 の方針1〜3 はここで直っていない。keepalive は 10 分の待ちを
  **見えるようにする**だけで、受け入れ可能にはしない。

## Refs

- waired-ai/waired-agent#837
- `docs/decisions/20260727/1500-vllm-install-progress-from-uv-lines.md`
- `docs/decisions/20260820/0130-model-residency-is-a-setting.md`
- `docs/decisions/20260811/2340-one-model-resident-at-a-time.md`
