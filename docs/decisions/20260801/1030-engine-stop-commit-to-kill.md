---
status: accepted
---

# エンジン停止は commit-to-kill、シグナル不能プラットフォームでは即 Kill (20260801 10:30)

## Status
Accepted

## Context

rc7 レビュー（waired-ai/waired#986、waired-agent#316）で、Windows 11 + RTX 5080
の実機のトレイから「Stop inference engine」が**必ず**失敗し、しかも**何も
kill されない**ことが判明した。`llama-server.exe` が 15GB の VRAM を掴んだまま
残り、Windows サービスを止めるまで解放されなかった。

3 つの欠陥が重なっていた。

1. **決定論的なタイムアウト** — トレイの書き込みクライアントは 3s 固定。一方
   stop は完全同期で、Windows では必ず 5s 以上かかる。`spawner_windows.go` の
   `Signal` が「黙って nil を返すだけの no-op」だったため、アダプタは死なない
   子プロセスを `StopTimeout`（既定 5s）丸ごと待っていた。3s < 5s なので、
   トレイ経由の stop は構造上一度も成功しえなかった（CLI の 10s 予算だけが
   たまたま間に合っていた）。
2. **中断が何も殺さない** — クライアントが切れると `r.Context()` が cancel され、
   `stopProcess` は Kill に到達する前に `case <-ctx.Done(): return ctx.Err()` で
   戻っていた。`Park` は既に `parked=true` をラッチ済みなので、status は
   `engine_power=stopped` と嘘をつき、トレイは "Start" を表示し、parked ラッチが
   `EnsureRunning` を塞いでローカル推論も peer 推論も死んだままになる。
   Linux/macOS では SIGTERM が実際に配送されるため被害が表面化しなかっただけ。
3. **無制限の刈り取り待ち** — Kill 後の `<-proc.Done()` に上限がなく、OS が
   回収しない子がいると管理ハンドラごと永久に固まる。

Windows の Job Object 配管（`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`）自体は正しく、
`taskkill /T` や新しい job 配管は不要だった。

## Decision

**一度始めた停止は、必ずプロセスの死で終わる（commit-to-kill）。**

- `stopProcess` は `ctx.Done()` でも Kill へエスカレーションする。呼び出し側の
  ctx は「呼び出し側があとどれだけ待つか」だけを縛り、「kill がどれだけ走るか」
  は縛らない。kill が成功したなら目的（メモリ解放）は達成なので nil を返し、
  graceful を待てなかったことは warn ログに残す。
- `RunningProcess.Signal` は、シグナルを配送できないプラットフォームで
  `ErrSignalUnsupported`（`internal/runtime/adapter.go`）を返す。`stopProcess` は
  これを見たら猶予を挟まず即 Kill する。Windows の stop は 1s 未満で完了する。
- Kill 後の刈り取り待ちは `StopTimeout` で有界化する。
- `Park` は停止が失敗したらラッチを外す。`engine_power` が「stopped」と言い
  ながらプロセスが生きている状態を構造的に作れなくする。
- `engineController.StopEngine` は `context.WithoutCancel` + 15s 予算で Park を
  走らせる。3s で諦めたトレイが Unix 側の SIGTERM 猶予を切り詰めてはならない。
- トレイはエンジン電源操作専用の書き込みクライアント（20s）を持つ。
  `http.Client.Timeout` は ctx より強い実時間上限なので、呼び出し側から予算を
  伸ばすことはできない（→ docs/knowledges/20260801/1045-http-client-timeout-beats-ctx.md）。

Windows 固有の挙動は `runtime.GOOS` 分岐ではなく **RunningProcess シームの値**
（`ErrSignalUnsupported`）として表現する。これにより untagged な `stopProcess` の
ロジックが Linux を含む全 CI レグで実行される。

同じ commit-to-kill 規則を vLLM アダプタにも反映した（multiprocessing の worker が
GPU メモリを掴むため理由は同じ）。

## Consequences

- 「レスポンスが返った時点でメモリ解放が確定している」という同期契約は維持される
  （202 async 化はしない）。トレイは実際の結果を見られる。
- 停止が失敗した場合、`engine_power` は `stopped` ではなく元の状態を報告し、
  エラーが呼び出し側に伝わる。status が嘘をつくよりよい。
- Windows のサブプロセス管理方針（Unix = pgid + SIGTERM / Windows = Job Object）
  自体は変わらない。変わったのは「シグナルを配送できないことを黙るか、値として
  返すか」だけである。
- ollama の graceful HTTP shutdown（`POST /api/shutdown` 相当）は引き続き未実装。
  Windows では猶予なしで Job Object を閉じることになるが、`ollama serve` と
  `llama-server` に対しては安全と判断した。必要になれば別途アダプタ側で足す。

## Refs
- https://github.com/waired-ai/waired-agent/issues/316
- docs/decisions/20260727/1755-engine-liveness-from-error-replies.md（procGen 世代管理。
  意図的な停止をクラッシュと誤認させないため、Kill のエスカレーションは
  `procGen++` の後に走る必要がある）
- docs/decisions/20260801/1035-mesh-share-suspension-is-live-only.md
