# 非ストリームの脚は、コミットしない限り黙らせるしかない (20260912 21:30)

## Issue

`docs/knowledges/20260912/1100-claude-code-gives-up-on-a-silent-leg.md` が、
「非ストリーム要求に何も返さないと 305.3 s で諦める」を測って waired-agent#1314 に
切り出した。あの issue が「設計は未決」として残した 4 案のうち、**案 2（HTTP レベルで
最初のバイトを出す）だけが測れば決まる** —— 本文が「Claude Code の JSON パーサが
どう扱うかは未測定」と書いていた一点である。

クライアント側の性質なので実機は要らない。1100 と同じ手口（`ANTHROPIC_BASE_URL` を
Python スタブに向けて `claude -p` を 1 回ずつ）で、スタブに非ストリーム用のモードを
足して測った。Claude Code 2.1.269、`--debug --debug-file`、隔離した `CLAUDE_CONFIG_DIR`、
ケースごとに別ポート。

非ストリームの脚に要求を落とすには、**まずストリームで失敗させる**必要がある
(1100 が測った連鎖: commit 済みストリームの `event: error` → 5 ms 後に `stream:false`
で再 POST)。したがって全ケースが「turn 1 = ストリーム、10 s で `event: error`」→
「turn 2 = 非ストリーム、これを測る」の形になる。

## Learnings

### 1xx は数えられない —— 切れるのはヘッダの締切だから

| turn 2 に返したもの | 結果 |
|---|---|
| 何も返さない（今日の脚） | **300.0 s** で諦め、以後およそ 301 s ごとに**無限に再試行** |
| `HTTP/1.1 100 Continue` を 5 s ごと | **300.02 s** で諦め（2 回とも同値）、同じく再試行 |
| `200 + application/json + chunked` をコミットし、`" "` を 5 s ごと | **400 s で答えて成功**(rc=0, 410.6 s) |

1100 の 305.3 s とここの 300.0 s は同じ締切である。あちらは `claude -p` の
経過時間（先行するストリームのターンを含む）、こちらはスタブが要求を受けてからの
保持時間で、差はその前段の分。以降この文書では**保持時間**で書く。

1xx は RFC 9110 §15.2 が「最終応答の前に 1 つ以上来てもクライアントは解析できねば
ならない」と定めており、実際 Claude Code は壊れない（4 本流した後の応答を正常に
受理した）。**ただし待ち時間を延ばさない。** 切れているのは
**応答ヘッダが来るまでの締切**で、1xx は最終応答のヘッダではないからである。

**本物のヘッダを出すと、待ちはフレームごとの締切に移る。** コミット後は
1 バイト届くたびに更新され、**885 s 保持した時点でも諦めていない**（こちらの打ち切り上限。
無音 300.0 s、ストリームの keepalive 609.9 s に対して、この形では
上限が見つからなかった）。

### JSON の先頭空白は通る

`" "` を本文の先頭に何十個も置いてから本体の JSON を書いても、クライアントは
値として読む（JSON の文法上、値の前の空白は無意味 —— RFC 8259 §2）。
79 個流した本文が正常に解釈された。

SSE のコメント行は**使えない**。`stream:false` の要求に SSE を返すと Claude Code は
「the non-streaming request was answered with a stream」と言ってそこで終わる
（1100 の脚注と同じ罠）。この脚に置けるのは「1 個の JSON 値の中で無意味なもの」だけ。

### コミット後に失敗したとき、言えることが 2 つしかない

コミットするとステータスは使い切られ、この方言の非ストリーム応答には
エラーを入れる場所が無い（ストリームの `event: error`、OpenAI ストリームの
`data: {"error":…}` に相当するものが無い）。取り得る形を両方測った:

| コミット後の失敗の出し方 | 画面に出るもの |
|---|---|
| エラーエンベロープを 200 の本文として書く | `API Error: API returned an empty or malformed response (HTTP 200) — check for a proxy or gateway intercepting the request. … body is JSON but not a Message …` |
| **終端チャンクを書かずに接続を切る** | `API Error: Connection to the API was lost (ECONNRESET). This is usually temporary — try again.` |

前者は**読む人を自分のネットワークの方へ送る** —— 原因はこのコンピュータのエンジン
なのに。後者は起きたことそのもので、助言も正しい。どちらも rc=1 で、
自動再試行はしない（非ストリームは「ストリーム要求の代替」として 1 回しか許されない）。

Go には後者を出す手段がある: `panic(http.ErrAbortHandler)` は終端チャンクを書かずに
接続を閉じ、スタックトレースも出さない。

### したがって「遅れてコミットする」

早く喋るほど良い、ではない。**コミットは 300 s の締切に対してだけ必要**で、
それより前に喋ると「エンジンの失敗を本当のステータスで報告できる」性質を捨てる。
出荷する形は「HoldAfter まで黙り、そこで初めてコミットして padding する」。
実測（240 s で コミット、500 s で応答）: **rc=0、511.0 s**。

### 実機での通し（sv-macmini、Apple Silicon、macOS 26.5.1、ollama 0.33.3）

スタブはクライアントの締切を測るためのもので、**実エンジンが本当にヘッダを
withhold するか**と**実クライアントが padding 付きの本文を読めるか**は別の問い。
`qwen3.5:4b-q4_K_M` をメモリから降ろし、117 KB（約 29k トークン）のプロンプトで
`stream:false` を 1 本ずつ撃った。

| ビルド | TTFB | 合計 | 本文 |
|---|---|---|---|
| 出荷する定数（4 分） | 93.5 s | 93.5 s | `Content-Length: 206`、**padding 0 バイト**、`type=message` |
| 検証用（5 秒） | **5.01 s** | 95.3 s | `Transfer-Encoding: chunked`、**空白 19 バイト + Message**、1 個の JSON 値として読めた |

- **93 秒待つターンでも、出荷する設定では今日と同形で返る。** コミットのログ行 0 本。
- 5 秒版のログは `waited_ms=5002 / hold_after_ms=5000` でコミットが**ちょうど 1 本**、
  `stream hold ended reason=first_byte shape=json-pad waited_ms=95131 frames=19`。
  同じホストで今日のコードなら **95 秒の完全な無音**。
- 偶然だが有用な 3 本目: unload が効かずウォームに当たったターンは 2.05 s で
  `Content-Length: 205`・chunked 無し・padding 無し —— HoldAfter 以内のターンが
  変わらないことの実機側の証拠。

**踏んだ罠 2 つ**（どちらも計測側の欠陥で、製品ではない）:

1. **`waired inference engine stop` は「コールドにする」手段にならない。** park された
   エンジンでは `EnsureRunning` が落ちるので、gateway は待たずに **9 ms で 503** を返す。
   待ちそのものを消してしまい、最初の実行は両ケースとも 503 で「padding 無し」に見えた。
   ロード待ちを作るなら `ollama serve` は動かしたまま、`/api/generate` に
   `keep_alive: 0` を投げて**モデルだけ**降ろす。
2. **`curl --raw` は転送デコードを切る。** 保存された本文がチャンク枠
   （`1\r\n \r\n…`）のままになり、「1 個の JSON 値として読める」を測れない。
   ワイヤの枠を見るには `--raw`、クライアントが読むものを見るには**付けない**で、
   両方要る。

## 再現

`~/verify-20260912-l1314/tools/{stub.py,run_case.sh}`（スクラッチ、リポジトリ外）。
実機側は同ディレクトリの `verify1314.sh` / `verify1314b.sh`（クロスビルドした daemon を
2 本入れ替えて撃ち、`trap ... EXIT` で元に戻す。Apple Silicon では**アドホック署名が要る** ——
`codesign -f -s -` を通さないと未署名の Mach-O は起動しない）。
1100 のスタブに非ストリーム用のモード 6 本（`n-silent` / `n-1xx` / `n-103` /
`n-pad` / `n-pad-err` / `n-pad-abort`）と `--answer-after` / `--commit-after` を
足したもの。`BaseHTTPRequestHandler` は 1xx を書けないので、`self.wfile` に
ステータス行を直書きする。

罠は 1100 と同じ: `ANTHROPIC_API_KEY` ではなく `ANTHROPIC_AUTH_TOKEN`、
隔離した `CLAUDE_CONFIG_DIR` に `hasCompletedOnboarding` 等、
親の `CLAUDECODE` を `env -u` で落とす。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1314
- `docs/knowledges/20260912/1100-claude-code-gives-up-on-a-silent-leg.md`
- `docs/knowledges/20260904/0210-claude-code-status-codes-for-gateway-errors.md`
- `docs/decisions/20260912/2200-nonstream-leg-commits-late-and-aborts.md`
