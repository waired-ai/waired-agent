# エンジン自身の既定窓はどこにも届かない — ollama 0.33.3 の VRAM 依存既定を追った記録 (20260906 16:00)

## Issue

0.33.3 pin (waired-agent#1193 / PR #1235) の検証で、ollama **自身の**既定
コンテキスト窓が 1 つの数ではなくなったことが分かった。エンジンは起動時に
`vram-based default context` とログし、見つけた VRAM から導く。0.33.3 を
走らせた 3 台で 2026-09-06 に測った値:

| ホスト | エンジンが見た VRAM | `default_num_ctx` |
|---|---|---|
| macOS / Apple M5 Pro / 48 GB | 37.4 GiB | 32768 |
| Linux / RTX 5080 + RTX 5070 Laptop | 23.8 GiB | 32768 |
| Windows / Ryzen AI Max+ 395 (Strix Halo) / 127.15 GB UMA | 102.2 GiB | **262144** |

0.33.2 まではどこでも一律 32768 だった。オーナーの問いは正しかった:
**これは実害があるのか、それとも古くなったコメントがあるだけか。** 答えは
後者で、その理由は 1 つのファイルを読んでも見えない。このノートは、次に
`ollamaContextFloor` を読む人、あるいは #624 の起動順の隙間を踏んで
「チューニング無しのエンジンは何を失うのか」と考える人が、同じ呼び出し元
の追跡をやり直さずに済むように、「コードは触らない」と決めた根拠を残す。

## Learnings

### 1. チューニング経路では、エンジンの既定は一度も参照されない

serve チューニング (#621) は manifest とホストのメモリから窓を計算し、
`OLLAMA_CONTEXT_LENGTH` を export する。この経路ではエンジンの既定が
何になろうと参照されない。

`ollamaContextFloor` (cmd/waired-agent/inference_ollama_tuning.go) = 32768
は、waired が**自分の要求**に当てる床であってエンジンの読み値ではない
(`planOllamaKV` の `if want <= 0 || want < ollamaContextFloor { want = ollamaContextFloor }`)。
値は影響を受けない。変える必要があったのは、その doc の中で「エンジンの
既定」を説明していた 1 文だけ。

### 2. エンジンの既定が支配する経路は 1 本あり、狭い

`ollamaTuning.Env()` は出すものがあるときだけ変数を出す:

```go
func (t ollamaTuning) Env() []string {
	env := make([]string, 0, 4)
	if t.ContextLength > 0 {
		env = append(env, fmt.Sprintf("OLLAMA_CONTEXT_LENGTH=%d", t.ContextLength))
	}
	...
```

`ModelTuning.ContextLength` (internal/runtime/ollama.go) の doc は「0 means
the var was not set (unknown sizing inputs) and the engine runs at its own
default」。実地でこれを作るのは #624 の起動順の隙間: fresh install では
エンジンのバイナリが bootstrap の途中で届き (`no engine viable: ollama needs
binary`)、その後の spawn にチューニング env が乗らない。
`modelEnvProvider` が spawn ごとに再計算してこの隙間を閉じており、
`ok=false` がその残りかす。

### 3. 読み手は全員 `ContextLength > 0` でゲートしている — だから既定はどこにも届かない

これが「無害」の根拠で、5 か所に散らばっている。5 つのコメントではなく
1 本のノートにする理由もここにある。

| 読み手 | 未チューニング時のふるまい | 場所 |
|---|---|---|
| 窓の宣言 | `if !ok \|\| t.ContextLength <= 0 { return 0 }` — 何も名乗らない。さらに `if win < hostfit.ServingWindow200k { return 0 }` で、2 段のどちらにも届かない窓も宣言しない | `agentInferenceProvider.DeclaredContextWindow`, cmd/waired-agent/inference.go |
| 適用済みチューニングの解決 | `appliedTuningFor` は `t.ContextLength > 0 && t.ModelID == m.ModelID` の両方を要求する | 同上 |
| verify の「適用されなかった」判定 | `if t.ContextLength > 0 && psm.ContextLength > 0 && ...` — 偽の `OLLAMA_CONTEXT_LENGTH did not apply` を出さない | cmd/waired-agent/inference_ollama_verify.go |
| runner argv の読み戻し | `if listProcs == nil \|\| t.ContextLength <= 0 { return ..., false }` (`observeRunnerFlags`) | 同上 |
| prefill 測定 | 「caller skips the run when ContextLength is 0 (untuned engine)」 | cmd/waired-agent/inference_prefill_state.go |
| warm slots | `if t.KVCapacityTokens <= 0 \|\| t.ContextLength <= 0` で抜ける | cmd/waired-agent/inference_warm_slots.go |

つまり、チューニング無しのエンジンが 262,144 トークンの窓で serve して
いても、何も宣言せず、測定値も生まず、verify の判定も起こさない。
「ノードは 2 つの窓のどちらか、または何も宣言しない」(`ServingWindow1M`
= 1048576 / `ServingWindow200k` = 200704、waired-ai/waired#1031) は、
エンジンが自分で何を選ぼうと成り立つ。未チューニングの場合は構造上
「何も宣言しない」側に落ちるから。

### 4. 残るのはメモリで、自己制限的。実測は 1 点

残るのは、チューニング無しの spawn が以前より大きな KV cache を確保する
こと。エンジンは見えた VRAM から窓を導くので、増分は余裕のある機体に
落ちる — 37.4 GiB の Mac と 23.8 GiB の Linux 機では 32768 のまま、
262144 になるのは 102.2 GiB のホストだけ。

紙の上で収まるだけでなく実際に動いたという 1 点:
Qwen3.8-Flash-Next の測定
(docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md) は
まさにこの未チューニング経路で始まった。`OLLAMA_CONTEXT_LENGTH` 無しで
エンジンは 262144 を取り、78.87 GB のモデルを `size_vram == size` で
serve した — spill 無し。この窓での KV は 6144 + 2304 MiB。

**確かめていないこと**: 大きくなった既定のせいで、32k なら載った
未チューニング spawn が**失敗する**ケースを、誰も探していない。手元で
最大のモデルが 1 回成功したのは調査ではない。

### 5. 再利用できる形

「upstream のこの変更はこちらに害があるか」は、値が変わったファイルを
読んでは答えられず、変わった値が**何に流れ込むか**を追って初めて答え
られた。3 パッケージの 4 つのコメントが古い事実を言っていたが、
封じ込めの実体はそのどこにも無かった。実体は読み手の全員が繰り返して
いる 1 つの述語で、どのコメント 1 つにも書けない形をしていた。
エンジンの挙動が動いたら、製品に欠陥があるのか古い 1 文があるだけかを
決める前に、消費側を追うこと。

## 変更したもの

コメント 4 つを「いつから旧記述が偽になったか」を言う形に狭めた:
`cmd/waired-agent/inference_ollama_tuning.go` (`ollamaContextFloor`) と
`internal/runtime/ollama.go` / `cmd/waired-agent/inference.go` (どちらも
#624 の隙間を「its 32k default」のコストとして書いていた)、加えて
cached-token に関する 2 つ (別件、0230 のノート §4)。`ollamaContextFloor`
は PR #1235 で、#624 の 2 か所はコミット 6a00ef03 で入った。挙動は変えて
おらず、変更の提案もしていない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1193
- https://github.com/waired-ai/waired-agent/pull/1235
- https://github.com/waired-ai/waired-agent/issues/624
- https://github.com/waired-ai/waired-agent/issues/621
- https://github.com/waired-ai/waired/issues/1031
- https://github.com/waired-ai/waired/issues/1312
- docs/knowledges/20260906/0230-ollama-pin-0333.md
- docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md
