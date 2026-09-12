# go:embed のテンプレートは Windows ビルドで CRLF になる (20260912 12:30)

## Issue

waired-agent#1306 で OpenClaw プラグインに行の一覧を焼き込み、それを読み返す
正規表現を足した。手元の Linux では全部緑、CI の **Windows の unit tests レグ
だけ**が落ちた。

```
--- FAIL: TestRenderedPluginIsWhatTheReadersRead
    declaredRefs = [] — the rows line and its reader moved apart
```

落ちたのは行末を `$` で固定していた側だけ:

```go
// 落ちる
var declaredModelsRe = regexp.MustCompile(`(?m)^const MODELS = (\[.*\]);$`)
// 隣にあった、落ちない側（`$` が無い）
var declaredWindowRe = regexp.MustCompile(`(?m)^const CONTEXT_WINDOW = (\d+);`)
```

## Learnings

- **`go:embed` はビルド時のチェックアウトのバイトをそのまま取り込む。**
  このリポジトリに `.gitattributes` は無く、Git for Windows の既定は
  `core.autocrlf=true` なので、Windows のランナーでは
  `templates/index.mjs.tmpl` が **CRLF で checkout され、その CRLF が
  埋め込まれ、書き出されたプラグインの行も `];\r\n` で終わる**。
  `(?m)` の `$` は `\n` の直前に一致するので、間の `\r` で外れる。
  Linux と macOS のランナーでは LF のまま checkout されるため、
  **手元でも 2 つの OS の CI でも永久に緑**になる。

- 対処は 2 つある。行末の固定を外す（`\s*$` にしても `\s` が `\r` を食うので
  可）か、`.gitattributes` で `*.tmpl text eol=lf` を宣言する。今回は前者を
  採った — 埋め込むテンプレートは今後も増えるが、リポジトリ全体の改行方針を
  この PR で決めるのは筋が違う。

- **再現は Linux で書ける。** テストの中で `bytes.ReplaceAll(body,
  []byte("\n"), []byte("\r\n"))` すれば、ランナーの checkout に依存せず
  同じ失敗が出る。「OS がやることをテストの中で OS にやらせる」の変種で、
  ここでは *Git* がやることを自分でやる。

- 同型の走査: `regexp.MustCompile(`(?m)` を全体に当てると、生成物を読む
  正規表現は 3 本しかなかった。`internal/integration/detect/jetbrains.go` の
  1 本は `\s*$` なので無事（`\s` が `\r` に一致する）。残る 1 本が上の
  `declaredWindowRe` で、もともと固定していなかった。

## Refs

- https://github.com/waired-ai/waired-agent/pull/1324
- `internal/integration/openclaw/topup.go`
- `internal/integration/openclaw/detect_adjacency_test.go`
