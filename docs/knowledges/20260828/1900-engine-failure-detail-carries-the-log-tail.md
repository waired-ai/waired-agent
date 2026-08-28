# 起動失敗の detail には既にログの tail が入っている (20260828 19:00)

## Issue

waired-agent#1069 の修正で、諦める直前に「原因を名指しする一文」を作る必要が
出た。素直に考えると `engine.log` を読み直す仕掛け（アダプタに
`EngineLogTail` を足す、ログの場所をハンドラに配る）が要るように見えるが、
実際には要らなかった。

## Learnings

**1. `OnStartFailed` / `OnUnhealthy` に渡る `detail` には engine.log の tail が
既に畳み込まれている。** その文字列を作る経路が 4 本あり、全部が畳んでいる:

- `startupExitError` → `engineExitError`（`--- <engine> stderr (tail, full log: …) ---`）
- `servingExitError` → 同上
- ollama の起動待ち deadline 枝（`ollama.go` の `not ready within …`）
- `markUnhealthy`（クラッシュ時、自分で tail を足してから `LastErr` に入れる）

だから診断関数は `detail` をそのまま読めばよく、ファイル I/O もアダプタ API の
追加も要らない。

**2. `LastEngineLogSpawn` は ollama では実質 no-op だが、掛けて損はない。**
banner（`===== waired: vllm spawn`）を書くのは vLLM だけ。ollama は
`openEngineLog` が spawn ごとに `engine.log` → `engine.log.1` に回して
truncate するので、**ファイル全体が常に 1 spawn 分**。関数の doc がその前提を
明記している。副作用として、ollama の 3 回の試行のうち `engine.log` に残るのは
3 回目だけ（2 回目は `.1`）。

**3. ollama がポート衝突で書く文字列（実測）。** sv-mag で python の listener に
:9475 を握らせ、同梱 ollama を `OLLAMA_HOST=127.0.0.1:9475` で起動:

```
Error: listen tcp 127.0.0.1:9475: bind: address already in use
```

exit code 1。**vLLM の Python OSError と違い、ollama 自身がアドレスを名乗る**
（vLLM 側は `[Errno 98] Address already in use` だけでアドレスが無いので、
#1026 は config から addr を渡す設計にした）。

なお **この経路に到達するのは非 ollama が握っている場合だけ**。別の ollama なら
`EnsureRunning` が `/api/version` を先に叩いて adopt するか、版を名指しして断る。

**3b. OS のエラー文言はテストの中で OS から採れる（#1085 の着地）。**
上の逐語は「一度どこかのホストで読んだ文字列」であり、書き写した時点で
古びはじめる。Windows 側の綴りに至っては、当初は**誰も観測していない
推定**として腕に入っていた（Go の `net` が OS の文言をそのまま運ぶ、という
根拠は正しいが、根拠は観測ではない）。

`ollama serve` は Go の `net` で bind するので、**同じアドレスに 2 回
`net.Listen` したときのエラーは、ollama が engine.log に書く文字列と
1 文字も違わない**。つまりテストの中で OS 自身にフィクスチャを書かせられる:

```go
ln, _ := net.Listen("tcp", "127.0.0.1:0")
_, err := net.Listen("tcp", ln.Addr().String())   // 必ず失敗する
ollamaStartupDiagnosis("Error: "+err.Error()+"\n", ln.Addr().String())
```

Go は Windows で `SO_REUSEADDR` を張らない（張ると横取りを許すため）ので
2 回目は `WSAEADDRINUSE`、Linux/darwin は `SO_REUSEADDR` があっても
2 本目のリスナは作れないので `EADDRINUSE`。**3 OS すべてで成立する。**

これで手書きフィクスチャが**毎 PR の実測**に変わる — CI の
`unit tests (windows)` は windows-latest で `go test ./...` を丸ごと走らせて
いるので、将来 Windows や Go が文言を変えれば赤くなる。実測値（2026-08-28）:

| OS | `err.Error()` |
|---|---|
| linux | `listen tcp 127.0.0.1:P: bind: address already in use` |
| windows | `listen tcp 127.0.0.1:P: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.` |

**採るのに実機は要らなかった**: `CGO_ENABLED=0 GOOS=windows go test -c` して
WSL の interop でそのまま実行すれば、本物の Windows PE として動く
（[[cross-os-verification-via-cross-built-test-binary]] の実践）。実機は
「ollama が実際にその行を engine.log に書き、`waired status` に出る」という
end-to-end の確認にだけ使えばよい。

**3c. vLLM 側の Windows の腕は死にコードだった。** `vllmStartupDiagnosis` に
`WinError 10048` の腕があったが、vLLM は linux 限定（`internal/runtime/vllm.go`
と `inference_vllm_linux.go` が `//go:build linux`、他 2 OS は `vllm_stub_*.go`）
なので **Windows の vLLM engine.log は原理的に存在しない**。#1085 で証拠を
探しに行って、読む対象のファイルが在り得ないことが分かった形。削除した。

**4. proto-additive-guard は const の「書かれたとおりの値」を比べる。**
`scripts/ci/protoguard/main.go` が `types.ExprString(vs.Values[i])` で値を
文字列化して突き合わせるので、`KVFactorF16 = 1.0` を
`KVFactorF16 = hostfit.VLLMKVFactorF16` に書き換えると、数値が同じでも
「const value changed」で落ちる。パッケージ間で定数を移すときは
**リテラルを両方に残してテストで結ぶ**しかない。func は署名（パラメータ名込み）
で比べるので、委譲ラッパは名前まで揃える必要がある。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1069
- https://github.com/waired-ai/waired-agent/issues/1085
- `internal/runtime/engine_log.go`, `internal/runtime/ollama.go`
- `docs/decisions/20260828/1830-the-give-up-message-carries-the-diagnosis.md`
