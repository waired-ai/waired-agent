# `ollama pull` のクライアントを殺すとサーバ側の転送はどうなるか (20260805 12:02)

## Issue

waired-agent#379 の**ブロッキング前提条件**。`internal/download/ollama.go` が
書いているとおり `ollama pull` は `ollama serve` の **CLIENT** で、
`DefaultRunner.Run` は `exec.CommandContext` で直の子だけを殺す。
サーバがクライアント切断時に転送を止めるかは、このリポジトリのどこでも表明も
テストもされていない。

止まらないなら、in-flight pull のキャンセルは**何もしないより悪い**:
まだ全速でダウンロードしている最中にモデルを failed と記録し、2 本目が並走し、
1 本目の存在を示す唯一の観測点を消すことになる。

ピン版 `internal/runtime.OllamaPinnedVersion = "0.31.1"` の upstream ソースを読んだ。

## Learnings

### 1. 止まる。参照カウントが 0 になった瞬間にダウンロード自身の ctx が切れる

`server/download.go`:

```go
// downloadBlob
go download.Run(context.Background(), requestURL, opts.regOpts)  //nolint:contextcheck
return false, download.Wait(ctx, opts.fn)

// blobDownload
type blobDownload struct {
    ...
    context.CancelFunc          // run() が context.WithCancel で埋める
    references atomic.Int32
}
func (b *blobDownload) release() { if b.references.Add(-1) == 0 { b.CancelFunc() } }
func (b *blobDownload) Wait(ctx context.Context, fn ...) error {
    b.acquire(); defer b.release()
    ...
}
```

`go Run(context.Background(), ...)` の 1 行だけを見ると「リクエスト ctx から
切り離されているので落ちない」に見えるが、それは**待ち手が誰も居なくなるまで**の話。
`PullHandler` は `c.Request.Context()` 由来の ctx で回すので:

CLI kill → 接続断 → リクエスト ctx キャンセル → `Wait` が `ctx.Done()` で戻る →
`release()` → 参照 0 → `CancelFunc()` → **転送停止**。

`<blob>-partial` とパートの状態ファイルは残るので、後続の pull は途中から再開する。

### 2. それでも「必ず止まる」ではない — 角が 3 つ

- **進捗送信でブロックしうる。** `Wait` は 60ms tick ごとに `fn` を呼び、
  `PullHandler` の `fn` は `ch <- r`（**バッファなし・ctx select なし**）。
  クライアントが消えると `streamResponse` の `c.Stream` が抜けて誰も読まなくなる。
  `ctx.Done()` は切断時点で ready になり、select は基本そちらで起きるが、
  tick と切断が重なれば `fn` で永久ブロックし `release()` に到達しない。
- **`CancelFunc` は `run()` の中で代入される。** `go Run(...)` の直後に
  キャンセルが走ると nil を呼んで **`ollama serve` が panic** する。
- **参照カウントはブロブ単位で共有。** 同じブロブを 2 クライアントが待っていれば、
  片方を殺してもキャンセルされない。

### 3. これは契約ではない

`internal/runtime/ollama_version.go` には
`// renovate: datasource=github-releases depName=ollama/ollama`。
上記は upstream の内部実装で、ピンは自動 bump される。この挙動に依存する設計を
入れるなら、bump で静かに壊れないための手当てが要る。

### 4. #489 以前はこの読み方自体ができなかった

reuse モード撤去（#489）でエンジンが waired 管理のピン版だけになったので、
「配るバイナリのソースを読んだ」がそのまま現場の挙動になる。任意バージョンの
他人の ollama が動きうる間は、静的検証に意味がなかった。

## Refs
- https://github.com/waired-ai/waired-agent/issues/379
- https://github.com/waired-ai/waired-agent/issues/359
- docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md
- ollama v0.31.1 `server/download.go` / `server/routes.go`
