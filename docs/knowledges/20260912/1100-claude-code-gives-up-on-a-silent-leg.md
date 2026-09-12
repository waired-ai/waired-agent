# Claude Code は無音の脚をいつ諦めるか (20260912 11:00)

## Issue

waired-agent#1304 の後半は「長い TTFB(35B-A3B で 34〜84 秒、5 秒ごとの
`: waired keepalive` あり)で Claude Code が再試行に入った」だった。
どの条件で諦めたのかは実機検証では採れておらず、
`docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md` が
keepalive を SSE コメント行にしたときの前提
(「クライアント側はバイト無着信のウォッチドッグと全体タイムアウトを持つ」)も、
具体的な数字を持っていなかった。

クライアント側の性質なので実機は要らない。
`docs/knowledges/20260904/0210-claude-code-status-codes-for-gateway-errors.md`
と同じ手口 —— `ANTHROPIC_BASE_URL` を小さな Python スタブに向けて `claude -p` を
1 回ずつ走らせる —— で測った。Claude Code 2.1.269、`--debug --debug-file`、
隔離した `CLAUDE_CONFIG_DIR`、ケースごとに別ポート。

スタブは `tools` を持つ最初のターンだけ遅らせ、`claude -p` が先に投げる
タイトル生成(`tools` が空)には即答する。混ぜると計測が汚れる。

## Learnings

### 諦めるまでの時間

| スタブが返すもの | 諦めるまで | その後 |
|---|---|---|
| **応答ヘッダを一度も出さない**(ストリーム要求) | **366.7 s**(2 回目 367.7 s) | 同じ形で再試行を繰り返す |
| 200 + SSE ヘッダ + `: waired keepalive` を 5 s ごと | **609.9 s** | ストリームで再試行 |
| 200 + SSE ヘッダ + `event: ping` を 5 s ごと | **611.7 s** | 同上 |
| **非ストリーム**要求に何も返さない | **305.3 s** | 非ストリームで再試行 |

`--debug` が理由を書く:

```
[WARN]  Slow first byte: no stream chunk 30.0s after request sent (attempt 1)
[WARN]  Streaming idle warning: no chunks received for 150s
[ERROR] Streaming idle timeout: no chunks received for 300s, aborting stream
[WARN]  Stream idle timeout before first event — retrying streaming (1/1)
```

**読み取れること 3 つ:**

1. **keepalive は効く。** 無音 366.7 s に対し、keepalive を流すと 609.9 s まで
   待つ。#837 が直した側の数字が 366.7 s で、直した後が 609.9 s。
2. **ただし無制限ではない。** 5 秒ごとにフレームが届いていても約 10 分で切れる。
   したがって**10 分を超えるコールドロードは keepalive でも救えない**。
   rc6 のフリートでの実測(mbp14 のローカル first-byte 128〜542 s、
   evox2 の初回 prefill 148.5 s)は中に入るが、542 s は余裕 68 秒しかない。
3. **フレームの種類は関係ない。** SSE コメント行(609.9 s)と
   `event: ping`(611.7 s)が同じ。`2142` がコメント行を選んだ理由
   (「ping を message_start より前に置いてよいかは他人のクライアントの性質」)は
   そのまま生きており、**ping に変えても得るものが無い**ことが実測で確かめられた。

### commit 済みストリームの `event: error` は、非ストリームの再試行になる

これが #1304 の後半の**真の説明**である。オーナーが見た再試行は
長い TTFB が原因ではない —— 84 秒はどの上限にも遠く届かない。

スタブが keepalive を 30 秒流してから `event: error`(`upstream_error`)を
1 本出すと:

```
[ERROR] Error streaming, falling back to non-streaming mode: {"type":"error","error":{...}}
```

そして **5 ミリ秒後に同じターンを `stream:false` で再 POST する**。

- その再試行が答えられれば、ターンは成功し、**ユーザーは失敗を一切見ない**
  (実測: 31.5 秒で `STUBOK`、rc=0)。
- その再試行が**無音**だと、305.3 秒で諦めて次の再試行に入る。ここで
  `Waiting for API response · will retry in …` が出る。

つまり rc6 で観測された連鎖は:

1. 切替が `ollama serve` を落とし、実行中のストリームが切れる
   → gateway が commit 済みストリームに `event: error` を書く。
2. Claude Code が**非ストリームに落として**即座に再試行する。
3. 新しいモデルをロード中のエンジンに当たる。**非ストリームの脚には
   keepalive が無い**(`proxyAnthropicStream` にしか無い)ので完全な無音。
4. 305 秒で諦めてバックオフ表示。

`event: error` を出す設計(`writeAnthropicErrorOrEvent`、#837)そのものは
正しく働いている —— 再試行が答えられれば透過的に回復する。問題は
**その再試行が落ちる先が無音の脚であること**。

## 再現

`~/verify-20260912-l106/tools/{stub.py,run_case.sh}`(スクラッチ、リポジトリ外)。
形は 150 行程度の Python:

- `GET` → `{"data":[]}`。
- `POST /v1/messages`: ボディの `tools` が空なら即答(タイトル生成)。
  空でなければモードに従って遅らせる。`stream` が false のときに SSE を
  返してはいけない —— 返すと Claude Code は
  「the non-streaming request was answered with a stream」と言って
  そこで終わり、計測にならない(最初の版で踏んだ)。
- 罠は `docs/knowledges/20260904/0210` と同じ: `ANTHROPIC_API_KEY` ではなく
  `ANTHROPIC_AUTH_TOKEN` を使う、隔離した `CLAUDE_CONFIG_DIR` には
  `hasCompletedOnboarding` 等を仕込む、親の `CLAUDECODE` を `env -u` で落とす。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1304
- https://github.com/waired-ai/waired-agent/issues/952
- `docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md`
- `docs/knowledges/20260904/0210-claude-code-status-codes-for-gateway-errors.md`
