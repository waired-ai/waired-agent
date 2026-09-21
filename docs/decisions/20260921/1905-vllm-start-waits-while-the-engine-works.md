---
status: accepted
---

# vLLM の起動待ちは、エンジンが働いている間は続ける (20260921 19:05)

## Status

Accepted。waired-agent#1508。

## Context

`VLLMAdapter.waitReady` は `/health` が**連続 60 回**失敗したら起動を失敗とみなしていた
(2 秒間隔で約 120 秒、TP>1 は 90 回)。根拠は「起動は重みの読み込みが支配的で 10〜60 秒」
だった。

新しい venv からの最初の起動は、それより長くかかる。0.28 以降は flashinfer-cubin を入れない
ので、flashinfer のカーネルを nvcc でコンパイルする時間が加わる。

- RTX PRO 4000 Blackwell で、作ったばかりの 0.29.0 の venv から Qwen3.5-4B(bf16)を初めて
  起動したとき、133 秒を過ぎた時点で失敗と判定された。`waired runtimes status` はその間
  `engine_failed` を返し、次の試行で約 100 秒で起動した(2026-09-21)。
- GPU e2e のレーンは L4 の初回起動に約 4 分かかるのを見て、レーン自身の予算を 5 分に広げていた。
- 35B 級は重みの読み込みだけで 120 秒に近い。

vLLM の版が動くたびに新しい venv を作るので、更新後の最初の起動は毎回この形になる。
プロセスは生きて仕事をしているのに決まった回数で打ち切るので、利用者には一時的な
`engine_failed` が見え、再試行が 1 回ぶん無駄になる。

## Decision

起動の待ちは、**子プロセスが生きていて働いている間は続ける**。ピア脚の待ちを稼働で切った決定
(`docs/decisions/20260828/0143-peer-leg-waits-while-the-peer-works.md`)と、ダウンロードの停止検知(#189)と同じ形にする。

1. **働いている**とは次のどれかが起きたこと: 子の出力のバイト数が増えた(engine.log の上限の
   手前で数えるので、上限を超えた出力も数える)/プロセスグループの CPU 時間が、前回の読み取りから
   の経過時間の半分以上増えた(ログを出さないコンパイルの間用。Linux の `/proc/<pid>/stat` の
   utime+stime+cutime+cstime をグループで合計)/`/health` が 200 を返した。
2. **5 分**どれも起きなければ失敗(`DefaultVLLMStartStallTimeout`)。観測した最も長い初回起動
   (L4 の約 4 分)を超え、無言で止まった起動を 5 分で見切る。
3. **30 分**で、働いていても打ち切る(`DefaultVLLMStartTimeout`)。NCCL の spin-wait やログの
   ループのように、働き続けて準備が整わない起動を終わらせる上限。
4. 子の終了は今どおり即失敗。待っている間の状態は `starting` のまま。2 分とその後 5 分ごとに
   INFO を 1 行出し、旧予算を超えた待ちをログで説明する。
5. `HealthMaxFails` は廃止。TP>1 の特別扱いも不要になった(NCCL の初期化もワーカーの
   コンパイルも「働いている」に入る)。

ollama は対象外。`ollama serve` はモデルを読む前に HTTP を開くので、起動時間がモデルの大きさにも
コンパイルにも依存しない(`internal/runtime/ollama.go` の `waitReady` は `StartupReadyTimeout`
で抑えている)。

## Consequences

- 冷えた初回起動が `engine_failed` を経由しなくなる。
- 本当に固まった起動は、旧来の約 2 分ではなく 5 分で失敗になる。働き続ける起動は最大 30 分
  `starting` のまま残りうる。人の `waired inference engine stop` は今どおり起動を切る。
- 読み取り: CPU の信号は Linux だけ(vLLM は Linux だけで動く)。USER_HZ は 100 と仮定する。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1508
- `internal/runtime/vllm_start_progress_linux.go`、`internal/runtime/proc_cpu_linux.go`、
  `internal/runtime/vllm.go` の `waitReady`
- `docs/decisions/20260828/0143-peer-leg-waits-while-the-peer-works.md`(ピア脚の待ち)
