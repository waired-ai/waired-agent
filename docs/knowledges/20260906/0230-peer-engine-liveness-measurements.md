# ピアの engine の生死を、どこで・どれだけ速く観測できるか (20260906 02:30)

## Issue

waired-agent#1220(凍った engine が 30 分ターンを握る)と waired-agent#1171
(再起動中のピアが素の 502 になる)の修正は、どちらも「ピアの engine が今
答えるか」をどこから読むかに賭けている。賭ける前に実機で測った。

構成: macOS の要求側 → Linux のピア(vLLM 0.28.0 / gpt-oss-20b / RTX PRO 4000、
文脈 124928)。ピアの engine は `127.0.0.1:9479`(`:8000` は別プロセス、
`:9475` は ollama、`:9473` は waired の OpenAI 面、`:9472` が Claude 面)。

## Learnings

### 1. 負荷は 1 秒のプローブを揺らさない

`runLocalInferenceProbe` は engine の `/health` を **1 秒**タイムアウトで 5 秒ごとに
撃つ。これが正当な prefill 中に外れるなら、`Reachable` は日常的に嘘になる。

110k トークン(約 440 KB、実測 4 バイト/トークン)を engine に直接投げ、その間
0.5 秒ごとに `/health` を引いた結果: **41 サンプル全部 200、最大 0.92 ms、失敗ゼロ**。
プローブの余裕は 3 桁ある。

### 2. `SIGSTOP` された vLLM の `/health` は「拒否」ではなく「無反応」

プロセス木ごと止めると、接続は確立するが応答が来ない —— curl は 3 秒の
`--max-time` を使い切って `000` を返す(reset ではない)。1 秒のプローブから見ると
確実な失敗になる。**ただしこれはプロセス木ごと止めた場合の話で**、api_server を
残して EngineCore だけ止めれば frontend が 200 を返し続ける可能性はある(未測定)。

### 3. 伝播は速い —— 凍結から要求側の snapshot まで 4〜6 秒

要求側の `waired peers list --json` の `inference_state.reachable` を 3 秒間隔で
見た。凍結 → `false` が **約 4〜6 秒**、解凍 → `true` が **約 5 秒**。
`waired peers list` は非特権で読めるので、伝播の実測はこれだけで足りる。

**罠**: 同じ出力の `last_check` は **15 秒刻み**でしか進まない。プローブの 5 秒
刻みではない —— 制御プレーンは内容が変わったときにだけ map を再署名するので、
要求側が持っているのはフレームの刻みである。`last_check` を観測点にすると、
伝播が実際より遅く見える。

### 4. デーモン再起動のあと、ピン付きターンは約 43 秒断られ続ける

`systemctl restart waired-agent` が返ってから、ピン付きの小さいターンが 200 を
返すまで **43 秒**(2 回とも同じ)。その間の答えは名指しの
`400 pinned_peer_unreachable` で、無言ではない。engine の生死(4〜6 秒)より
一桁遅いのは、デーモンの再起動が WireGuard のハンドシェイクと登録をやり直すため。

### 5. プレフィクスを変えないと窓が消える

同じプロンプトを 2 回投げると 2 回目は engine のプレフィクスキャッシュに当たる。
実測 TTFB は **cold 17〜21 秒 / warm 1.95 秒**。dispatch 後に何かを起こす検証は、
毎回ランダムな接頭辞を付けないと 2 秒の窓しか無くなる。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1220
- https://github.com/waired-ai/waired-agent/issues/1171
- `docs/decisions/20260906/0200-the-wait-reads-the-observer-the-mesh-already-has.md`
