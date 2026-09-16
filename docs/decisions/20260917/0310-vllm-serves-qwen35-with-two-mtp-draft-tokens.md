---
status: accepted
---

# vLLM は Qwen3.5 を MTP の draft 2 トークンで提供する (20260917 03:10)

## Status
Accepted。オーナー裁定 2026-09-17(実装セッションでの判断; waired-ai/waired#1432)。

## Context

Qwen3.5 0.8B / 2B / 4B bf16 の checkpoint は MTP (multi-token prediction) 層を 1 つ持ち、vLLM 0.29.0 は `{"method":"mtp","num_speculative_tokens":N}` でそれを draft に使う投機的デコーディングを提供する。前回の pin 移動 (docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md §11) はこれをエンジンの事実として測っただけで、製品は draft 無しで提供していた。

実測 (docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md §1〜§5、RTX PRO 4000 Blackwell 24 GB、製品の argv):

- デコード (novel / edit、tok/s): 4B は N0 66.4 / 65.9 → N2 116.0 / 140.3 → N3 128.2 / 165.5 → N4 128.4 / 184.1。2B は 144.4 / 142.6 → N2 218.1 / 257.3。0.8B は 321.5 / 318.0 → N2 515.2 / 535.7。受理率は N を伸ばすほど下がる (4B: 0.95 / 0.85 / 0.82 / 0.77)。2 ストリーム同時でも合計は上がる (4B: 124 → 217 at N2)。
- メモリ: 4B のプールは N0 567,464 → N2 469,207 トークン。予約を掛けない見積りは 16 GB 級の予算で 4B の N2 に、プール 82,106 に対して 84,992 のコンテキストウィンドウを約束していた。
- 最初のトークンまでの時間 (TTFT): cold で +8〜15% (4B +8% / +10%、2B +12% / +13%、0.8B +13% / +15%)、再送は送るたびにブロック 1 つ分 (約 1,056 トークン) が余計に再計算される。エージェント形の再生では次のターンが +22%、3 会話を交互に育てる再生では再計算が +67%、TTFT p50 が +9%。
- tool call は全構成で返り、Model Runner V2 は維持される。

事前に置いた採用の基準は「TTFT を悪化させない」だった。この基準は満たされていない。オーナーはそれを知ったうえで既定 on を選んだ。

## Decision

1. **Qwen3.5 0.8B / 2B / 4B bf16 は MTP を既定 on、`num_speculative_tokens` 2 で提供する。** カタログの build が `catalog.Variant.MTPLayers` と `MTPDraftTokens` を持つとき、`router.VLLMSpeculative` が `{"method":"mtp","num_speculative_tokens":2}` を出す。2 は 3 つの build すべてで測った最大の draft 長。
2. **切るのは `vllm_disable_mtp`** (環境変数 `VLLM_DISABLE_MTP`、フラグ `--inference-vllm-disable-mtp`)。`vllm_speculative_ngram` が on のときは ngram が勝ち、MTP は出ない。
3. **pin された venv でだけ出す** (serve-flag gate)。converge 前の venv には出さない。
4. **メモリは予約して価格付けする。** `hostfit.VLLMMaxModelLenFor` は draft の長さを取り、256 MiB + draft 1 トークンあたり 128 MiB に、MTP 層の KV キャッシュのトークンあたりの価格 (`catalog.Variant.MTPKVBytesPerTokenFP16`: 4B 4096、0.8B / 2B 2048) をコンテキストウィンドウの全トークンに掛けたものを足す。選択は既定 on で価格付けし、提供は実際に出す draft で価格付けする (opt-out は提供だけを変える)。
5. **Qwen3.6-27B / Qwen3.8-27B の FP8 build は `MTPLayers` / `MTPKVBytesPerTokenFP16` の事実を持つが、draft は持たない。** それらの MTP 層は予約を較正した 0.25 GiB より大きい。9.32 GB (Qwen3.5-4B bf16) より重い build に draft が付くとテストが落ちる。
6. **ホスト速度の probe は draft を持たない。** その 45 s の線は draft 無しのエンジンで較正されており、ollama のホストも同じ尺度で分類するため。

## Consequences

- コーディングエージェントのデコードは 4B で 1.7〜2.1 倍、2B / 0.8B で 1.5〜1.8 倍になる。代わりに cold の TTFT は 8〜15% 遅く、長い会話の再送は毎回約 1,056 トークンを余計に計算する。
- 24 GB のホストで提供するコンテキストウィンドウは変わらない (262,144 の上限に掛かる)。16 GB 級の予算では 4B のコンテキストウィンドウが 84,992 から 46,080 に縮む — 予約が無ければその draft は起動できない構成だった。
- MTP ではハイブリッドモデルのプレフィックスキャッシュの retention が vLLM の既定で dense に変わる。これに flag を足さない理由は docs/decisions/20260917/0330-no-prefix-cache-retention-flag.md。
- 速度の記録は draft の長さをキーに持つ (draft が無いときのハッシュは不変)。既存の draft 無しの計測はそのまま残り、MTP の計測は別のキーに入る。
- `vllm_disable_mtp` を立てたホストは draft の分のメモリをコンテキストウィンドウに戻す。選択は既定 on の価格で行われるので、opt-out したホストの提供コンテキストウィンドウは価格付けより広い側に外れる (fp8 の opt-out と同じ非対称)。
- 27B の FP8 build に draft を付けるには、その MTP 層の大きさで予約を測り直す別の変更が要る。

## Refs
- https://github.com/waired-ai/waired/issues/1432
- docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md
- docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md
- docs/decisions/20260917/0330-no-prefix-cache-retention-flag.md
