---
status: accepted
---

# 非ストリームの脚は遅れてコミットし、コミット後の失敗では接続を切る (20260912 22:00)

## Status

Accepted。`docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md` と
`docs/decisions/20260912/1145-openai-leg-post-commit-error-frame.md` が
ストリームの 2 本について決めたことを、残る 2 本（非ストリーム）へ延ばすもの。
どちらも改定しない —— あの 2 本の「最初の tick でコミットする」は
**ストリームについて正しいまま**で、ここで違う答えを出すのは脚の性質が違うからである。

## Context

ゲートウェイのローカル脚は 4 本ある（Anthropic / OpenAI × ストリーム有無）。
ollama は重みが常駐するまで応答ヘッダを 1 バイトも出さず、`postToEngine` は
`Timeout: 0` の `client.Do` で待つので、放っておけば 4 本とも無音になる。

ストリームの 2 本は 5 秒ごとの SSE コメント行で塞がれている（#837、#952）。
非ストリームの 2 本は**同じ道具が使えない** —— この脚はクライアントに
「JSON オブジェクトを 1 個」と約束しており、SSE のフレームはその約束を破る。
`proxyAnthropicNonStream` のシグネチャに `waitPolicy` が無かったのは、
その事実がコードに現れていたということである。

waired-agent#1314 はこの穴を、**1 つの出来事が両方を生む**形として記録した:
commit 済みストリームが失敗すると Claude Code は 5 ms 後に同じターンを
`stream:false` で再 POST し、そのストリームを切った原因（切替による
`ollama serve` の再起動、クラッシュ回復、park）が、まさにエンジンをロード中に
している。rc6 で見えた `Waiting for API response · will retry in 2m 32s` はこの連鎖。

実測は `docs/knowledges/20260912/2130-nonstream-leg-held-only-by-committing.md`。
要点は 3 つ:

1. **1xx は待ちを延ばさない。** 切れているのは応答ヘッダまでの締切で、
   `100 Continue` を 5 秒ごとに流しても 300.02 s で切れる（無音と同値）。
   本物のヘッダを出すとフレームごとの締切に移り、885 s 保持しても諦めなかった
   （こちらの打ち切り上限で、上限は見つかっていない）。
2. **JSON の先頭空白は通る。** 値の前の空白は文法上無意味（RFC 8259 §2）。
3. **コミット後に失敗を言う手段が無い。** この方言の非ストリーム応答には
   エラーを入れる場所が無く、200 の下にエンベロープを置くと
   「プロキシかゲートウェイを疑え」と読まれる。

## Decision

### 1. 非ストリームの脚も待ちを埋める。フレームは無意味な空白

コミットするのは `200 + Content-Type: application/json`、`Content-Length` は
付けない（長さはコミット時点で分からず、付けないことが chunked にして
1 フレームずつ届かせる）。フレームは `" "` 1 バイト。

配線はストリームと同じ `waitPolicy` から取る —— **LOCAL 選択に限る**という
`2142` の制限はそのまま引き継ぐ。ピア脚の非 2xx には窓超過の 400 があり、
`relayPeerContextOverflow` がそれを**ステータスとして**中継して Claude Code の
自動コンパクションを起こす。コミットした本文はステータスを運べない。

### 2. 最初のフレームは「1 インターバル後」ではなく「4 分後」

ストリームが最初の tick でコミットしてよいのは、コミット後も `event: error` で
失敗を言えるからである。この脚にはそれが無い（上記 3）。したがって
**沈黙は、バイトより価値がある間は保つ**。

既定 4 分。実測の 300.0 s に対して 1 分の余裕を取る。この余裕は遊びではなく、
**その 1 分はエンジンの失敗を本当のステータスで報告できる 1 分**である。
代償を払うのは「4 分を超えて待った」ターンだけで、それは今日この脚が
取りこぼしているターンそのものである。

`nonStreamHoldAfter` はパッケージ定数にした。設定項目にしない理由は、
この値が**こちらの都合ではなくクライアントの締切から決まる**もので、
運用者が選ぶ量ではないため。締切が変わったら測り直して定数を動かす。

### 3. コミット後に失敗したら、接続を切る

`panic(http.ErrAbortHandler)`。終端チャンクを書かずに閉じる。

これは「何も言わない」ことの選択ではなく、**言える 2 つのうち正しい方**の
選択である（実測、上記 3）:

- エンベロープを 200 の下に置くと `check for a proxy or gateway intercepting
  the request` —— 原因はこのコンピュータのエンジンなのに、読む人を自分の
  ネットワークへ送る。
- 切ると `Connection to the API was lost (ECONNRESET). This is usually
  temporary — try again.` —— 起きたことそのもので、助言も正しい。

**本当のステータスは `rr.fail(実ステータス, 理由)` に残る。** クライアントが
受け取ったのは 200 で、失敗であることは reason が担う ——
`docs/decisions/20260807/1648-truncated-turn-is-metered.md` と #538 が置いた線の、
この脚での適用。`1145` が OpenAI ストリーム脚について書いたのと同じ形。

判定は形が持つ: `holdShape.inBand` が「コミット後も失敗を言えるか」を持ち、
SSE の 2 形は true、この形は false。呼び出し側は `canReportInBand()` を見るだけで、
どの脚かを知らなくてよい。

### 4. OpenAI 方言の非ストリーム脚も同じ PR で塞ぐ

#952 は `opts.Streaming` でゲートしたので、`proxyToEngine` の
`stream:false` 経路は同じ理由で無音のままだった。#1314 の本文は Anthropic 面だけを
名指しているが、機構は共通で、別々に直せば 2 つの答えが食い違う。

### 5. オーバーレイには配線しない

`Deps.StreamKeepalive` は `:9474` で 0 のまま。`2142` の理由がそのまま当たる ——
提供側ピアが**エンジンが出していない最初のバイト**を呼び出し側に渡すと、
呼び出し側の #757 予算が無効になる。空白でも 1xx でも同じことである。

## Consequences

- **4 分以内に答えたターンは 1 バイトも変わらない。** 失敗も今日と同じ
  ステータスで届く。代償を払うのは 4 分を超えたターンだけ。
- **4 分を超えたターンは、失敗したときステータスを失う。** 読む人は
  「接続が切れた、もう一度」を見る。今日そのターンが見るのは
  300 秒の沈黙と無限の再試行なので、失うものは無い。
- **`X-Waired-*` ヘッダはコミット後には乗らない。** `HeaderLocalError` の
  ステージングが無効になるが、`rr` が同じ reason を持っており、
  intercept のジャーナルが読むのはそちらである。
- ストリーム脚の keepalive は 609.9 s で切れる一方、こちらは 885 s 保持しても
  切れなかった（上限は見つかっていない）。**同じ待ちでも脚によって天井が違う**
  ことになるが、どちらも「クライアントが決めた締切」であり、こちらで
  揃えられるものではない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1314
- `docs/knowledges/20260912/2130-nonstream-leg-held-only-by-committing.md`
- `docs/knowledges/20260912/1100-claude-code-gives-up-on-a-silent-leg.md`
- `docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md`
- `docs/decisions/20260912/1145-openai-leg-post-commit-error-frame.md`
