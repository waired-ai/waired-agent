---
status: accepted
---

# ollama の MTP の draft はホストごとに書く (20260917 03:20)

## Status
Accepted。オーナー裁定 2026-09-17(実装セッションでの判断; waired-ai/waired#1433)。

## Context

カタログの unsloth の 4 タグ (`hf.co/unsloth/Qwen3.6-35B-A3B-MTP-GGUF:UD-Q3_K_XL` / `UD-Q2_K_XL`、`hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL` / `UD-Q2_K_XL`) は nextn ブロックを持つが `draft_num_predict` を公開していない。ollama 0.34.0 が MTP の draft を走らせるのはタグか要求がこれを設定したときだけなので (`server/routes.go`)、製品はこれまで draft 無しで提供していた (docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md §12)。

実測 (docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md §6〜§8):

- 24 GB の CUDA (全層 GPU): draft 2 でデコードは 35B-A3B が 141〜154 → 224〜240 tok/s (novel)、27B が 36〜41 → 66〜67 tok/s。draft 4 は 35B-A3B の edit で最速 (UD-Q2 292.4) だが、novel では draft 2 より遅い (UD-Q2 220.0 対 239.5)。
- 48 GB の Metal (全層 GPU): 35B-A3B は draft 2 で 66〜75 → 89〜97 tok/s、draft 4 で 97〜99。27B は draft 2 で novel が横ばい (20.7 → 20.4、17.5 → 19.3)、draft 4 で novel が下がる (20.7 → 17.2)。
- **あふれるホストでは draft が速度を壊す。** 16 GB の M4、27B UD-Q2 を 200,704 で: draft 無しは 66 層のうち 38 層が GPU で 4.0 tok/s、draft 2 は 24 層で 0.11 tok/s、400 トークンの生成が 30 分のタイムアウトに掛かった。48 GB の Mac で 24 GB の予算を模しても、27B q2 は draft 無し 66/66 層 20.7 tok/s に対し draft 2 は 50/66 層 9.7 tok/s。
- ollama はタグの draft を落として場所を空けることはしない。llama.cpp の fit が層を CPU に移す。
- ollama は qwen35 と qwen35moe を `OLLAMA_NUM_PARALLEL` に関わらず 1 スロットで起動する (`server/sched.go`; waired-ai/waired-agent#1423)。unsloth の 2 ファミリはどちらもこれに当たる。
- `draft_num_predict` は runner のオプションなので、タグのパラメータを変えると次の要求で runner がロードし直される (`server/sched.go` needsReload)。存在するタグへの `ollama pull` は書き込んだ draft を消し、`ollama create` で PARAMETER を省いても既存の draft は消えない。

## Decision

1. **4 つの unsloth のタグの `MTPDraftTokens` は 2。** draft 4 は 35B-A3B の edit で速いが、Metal の 27B の novel では遅い。
2. **draft はホストごとに書く。** `hostfit.OllamaDraftTokens` が、1 スロットの負荷に draft を足したものがデバイスの予算に入るホストでだけ 2 を返し、入らないホストでは 0 を返す。カタログの値だけで書かない。
3. **draft は 2 つ目のスロットより先。** 2 つ目のスロットは draft の隣に入るときだけ与える (オーナー: 「draft 優先」; ollama 自身もこれらのアーキテクチャを 1 スロットで起動する)。
4. **コンテキストウィンドウは draft より先。** 容量は draft 無しで決め、draft はその後に入るかどうかを見る。draft でコンテキストウィンドウは縮まない。
5. **書くのは pull のとき** (`download.Rendering.DraftNumPredict`)。ロード後、verify が runner のコマンドライン (`proclist.RunnerFlags.SpecType` / `SpecDraftTokens`) から実際の draft を読み、適用した tuning での規則と比べる (`cmd/waired-agent` の `draftToRewrite`)。食い違えば `restampDraft` がそのタグを pull し直して書き直す。(tag, draft) ごとにプロセスで 1 回だけ。提供は止めない — ollama が次の要求で runner をロードし直す。
6. **自分で draft を公開しているタグには触れない** (`GGUF.DraftMaxTokens > 0`)。製品が書いた draft だけが製品の変えるもの。
7. **速度の記録は draft をキーに持つ** (draft が無いときのハッシュは不変)。

## Consequences

- 同じタグでも、ホストによって draft が走ったり走らなかったりする。24 GB の CUDA と 48 GB の Metal では 4 タグとも draft 2 で走る。16 GB の M4 の 27B のように draft の分が入らないホストは、draft 無しで動いたままになる — 0.11 tok/s になる構成は生成されない。
- 価格付けと実際に走る構成が揃う。`hostfit.OllamaDraftTokens` が価格付けした draft を、pull と verify が同じ規則で書く。
- カタログが `MTPDraftTokens` を持つ前に pull されたタグは、次のロード後の verify で 1 回 pull し直される。その pull は書き込んだ RENDERER / PARSER も同時に書き直す (製品の書き込みは pull の後に create を掛ける手順)。
- 既存の draft 無しの速度計測はキーが変わらないので残る。draft 2 の計測は別のキーに入り、初回は測り直す。
- verify が同じ (tag, draft) で 2 回目に食い違いを見ても pull し直さない。書き直しても runner が別の draft を見せるなら、もう 1 回 pull しても変わらないため。
- Metal の見積りは 35B-A3B q2 で fit の増分を約 490 MiB 下回る (ollama が ubatch 2048 を選ぶため)。draft 無しの見積りが fit の予測より 400〜1,100 MiB 上にあるので、合計は安全側に留まる。ubatch を見積りに入れるのは別の変更。

## Refs
- https://github.com/waired-ai/waired/issues/1433
- https://github.com/waired-ai/waired-agent/issues/1423
- docs/knowledges/20260917/0300-mtp-speculative-decoding-vllm-and-ollama.md
- docs/knowledges/20260916/0500-engine-pins-0340-and-0290.md
- docs/decisions/20260828/1900-retire-the-forced-generation-batch.md
