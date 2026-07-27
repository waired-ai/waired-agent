---
status: accepted
---

# エンジンの死活はエラー応答から検知する（新規ポーリングは足さない）(20260727 17:55)

## Status
Accepted

## Context

waired-agent#29 で、ollama の子プロセス `llama-server` が segfault しても
**誰も気づかない**ことが判明した。親の `ollama serve` は生き続けるため:

- ヘルスチェックは `GET /api/tags`。生きている**親**が 200 を返す → 「正常」
- 起動後の定期ヘルスループが**存在しない**。`Health()` はキャッシュ返却、
  `EnsureRunning` は `StateReady` で即 return → プロセス寿命の間 Ready がラッチ
- `waitReady` の子プロセス監視は readiness 待ちで終了 → Ready 後は誰も見ていない
- `EngineReady()` はディスク上の catalog state しか見ない
- gateway / router に engine 5xx ハンドラが 1 つも無く、`proxyToEngine` は 500 を
  そのまま転送して `nil` を返すため、呼び出し元が `rr.succeed()` を呼び
  **イベントリングに status=200 として記録**され、課金 usage にも計上される

結果、観測されたのは「6 分間・約 90 リクエストがすべて 500、その間
`waired status` は ready」という状態だった。ユーザー影響のある製品バグ。

## Decision

### 検知: エラー応答の分類（新規ポーリング無し）

`FailureReporter` という**任意インターフェース**を `internal/runtime` に追加し、
gateway が非 2xx をアダプタに渡す。アダプタだけが自エンジンのエラー語彙を知っている。

検討して却下した代替:

| 候補 | 却下理由 |
|---|---|
| `/api/tags` のポーリング強化 | 構造的に検知不能（親が答える）。しかも `inference_probe.go` が既に 5 秒毎に叩いており、増やしても何も増えない |
| `/api/ps` | 健全なアイドルエンジンでも空になるため偽陽性を作る（既存の 2 箇所はいずれも先にモデルをロードしてから見ている） |
| 定期的な `/api/generate` | 決定的だが、アイドル時に数 GB のモデルロードを誘発する |

**素の 5xx では降格しない。** `engineDeadMarkers`
（`process has terminated` / `model runner has unexpectedly stopped`）に一致した
場合のみ。マーカー無しの 5xx は `slog.Warn` でカナリアとして残す — 将来 ollama が
文言を変えたら検知が黙って旧挙動に戻るため、それを可視化する唯一の手段。

**補完（無料）**: Ready 到達後に `proc.Done()` を監視する `superviseChild`。
`procGen` 世代カウンタで意図的な Stop / Park / reconcile と区別する。

### 復旧: `reconcileEngineServe` を再利用

`reconcileEngineServe` が既に唯一の合流点（`engineReconcileInFlight` CAS）。
`engineRecoverPending` を足して 4 箇所を外科的に修正するだけで、モデルスワップ・
並列度再調整との相互排他が無料で手に入る。並行機構は作らない。

予算は **3 回**（0s / 15s / 60s）＋ `engineRecoveryStableFor = 5m` でリセット。
初回 0 秒は意図的（人が待っている）。使い切ったら `LatchFailed` で
`ErrEngineUnrecoverable` を返し、**正直に諦める**。

**`restartOnWedge`（デーモン再起動）は再利用しない。** systemd が
`Restart=always` なので無限ループになり、boot ベンチと 30,720 トークンの
depth sweep を毎周回する。子プロセスは SIGTERM→SIGKILL で確実に落とせるので、
その場で入れ替えるほうが常に良い。

### `EnsureRunning` を join 方式に

現状 `StateStarting` 中の同時呼び出しはハードエラー → `503 runtime_unhealthy`。
**復旧再起動はまさにその窓を作る**ので、これを直さないと「永久 500」を
「30 秒間の 503 の壁」に置き換えるだけになる。

### 証拠の保全

`openEngineLog` は spawn 毎に `O_TRUNC` していた。自動再起動が crash trace を
消してしまうため、1 世代だけ `engine.log.1` にローテートする（上限 16MiB）。
併せて降格時に `tailEngineLog` で `LastErr` に畳み込む（`waired status` /
tray / mgmt API は無変更で理由を表示できる）。

### 正直な報告

`EngineReady()` にアダプタ health を織り込む。この 1 箇所で peer `/healthz`・
observability ゲージ・`waired doctor`・setup ゲート・ベンチの ready ゲートが
すべて正直になる。`subsystem_state` に `engine_failed` を追加。
mesh probe は 2xx-4xx ルールを緩めず `EngineDead` を差し込む — **`StateFailed`
のみ**（起動中に落とすと `waired claude` と透過 proxy の degrade が連鎖する）。

## Consequences

- エンジンがクラッシュしても自動復帰する。決定論的にクラッシュするモデルでは
  3 回で諦め、`waired inference engine start` が明示的な復帰口になる（新 API 不要）。
- borrowed（reuse）/ adopted / parked エンジンは復旧対象外 — waired が所有して
  いないものを再起動するのは運用者の判断を上書きすることになる。
  borrowed では `Ready → Failed → Ready` のフラつきが起きうるが、
  「永久に嘘をつく」より厳密に良い。
- gateway が非 2xx を `rr.fail(status, "engine_error")` で記録するようになり、
  イベントリングと課金 usage が正直になった（副次的だが独立した修正）。
- 残存リスク: ちょうど `engineRecoveryStableFor` + 1 秒毎にクラッシュすると
  永久に許容される（約 288 回/日）。必要なら通算上限を追加する。
- `EnsureRunning` の同一ラッチ問題は `vllm.go` と `openaicompat/adapter.go` にも
  ある。1 PR 1 アダプタとし、パリティ債務として記録する。

## Refs
- waired-ai/waired-agent#29
- `internal/runtime/ollama.go`（`markUnhealthy` / `superviseChild` / `LatchFailed`）
- `cmd/waired-agent/inference.go`（`onEngineUnhealthy`）
- `docs/decisions/20260727/1715-ollama-kv-quant-only-when-it-buys-ctx.md`
