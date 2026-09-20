---
status: accepted
supersedes:
  - docs/decisions/20260820/1130-step-down-walks-the-selection-ladder.md
superseded_by:
  - docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md
  - docs/decisions/20260920/0300-catalog-admits-what-was-run-on-the-reference-host.md
  - docs/decisions/20260920/1800-windows-budget-stops-at-a-measured-load.md
---

# qwen3.8 を 35B-A3B の上に置き、段下げの行き先は参照機での実測で選び、カタログは参照機に載るものに限る (20260916 03:40)

## Status
Accepted。オーナー判断 2026-09-16（waired-ai/waired#1357 の L107、comment 5686058325）。`docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md` の裁定 13（「tier の順序は dense と MoE のどちらを上にするか、L107 が決める」）への答え。実装は waired-ai/waired-agent#1400。同日（2026-09-16）、#1400 の実装計画を確認する場でオーナーが 4 点に答え、決定 2・3 を詰め、決定 4・5 を足した。

`docs/decisions/20260920/0300-catalog-admits-what-was-run-on-the-reference-host.md`(オーナー判断 2026-09-19〜20、waired-ai/waired#1427)が、決定 3 を狭め、例外を 1 つ足す。入れるのは参照機で実際に動かした variant で、量子化していない variant は参照機に載らなくても推測の秒数で入れる。

`docs/decisions/20260920/1800-windows-budget-stops-at-a-measured-load.md`(waired-ai/waired-agent#1443 の計測、2026-09-20)が、決定 3 の参照機のアクセラレータメモリの予算を改める。`OllamaVRAMBudgetMB` は 98,304 MB ではなく 81,920 MB(参照機で読み込めた最大の負荷、80 GiB)。

次の記録を**部分的に狭める（覆さない）**。記録の `## Status` に鏡の一文を置いた。

`docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md`(オーナー決定 2026-09-16、waired-ai/waired-agent#1434)が、決定 4 の「残すのは」のうち「vLLM の `--max-model-len` による切り詰め」を置き換える。vLLM は 200,704 か 1,048,576 で配信し、KV プールが 200,704 を保てないビルドはコンテキストウィンドウを宣言しない。同じ箇所のほかの 2 つ(まだ宣言していないエンジン、granite4-350m)は残る。

- `docs/decisions/20260820/1130-step-down-walks-the-selection-ladder.md`: 決定 1 の受け入れ判定「baseline より厳密に軽い」（`footprintCmp`）を、「参照機での 1 リクエストの秒数が active の variant より小さい」に置き換える。ランク順で最初の候補を返すこと、active と同じモデルを除くこと（#754）、決定 2（tier 差を出さない）はそのまま有効。

## Context

### 出荷カタログでは、35B-A3B が品質の根拠なく 27B の上にいた

`qwen3.6-35b-a3b/mtp-q4-gguf` は `quality_tier` 90、`qwen3.8-27b/mtp-q4-gguf` は 71。`scoring.CompositeScore` で計算すると 82.86 対 82.36 のほぼ同点で、90 対 71 の差は手書きの順序だった（`tier_override` は無い）。その結果、KV `q4_0` 既定（#1388）の main `156b28b9` の `PickModel` で、NVIDIA 16〜78 GB と unified 22〜100 GB のホストはすべて 35B-A3B を渡され、`qwen3.8-27b/mtp-q4-gguf` はどのホストでも選ばれていなかった。

品質の出典は、どれも dense 27B を上に置いている:

| 出典 | dense 27B | MoE 35B-A3B |
|---|---|---|
| Qwen3.6 公式カードの同じ表: SWE-bench Verified / Pro / Terminal-Bench 2.0 | 77.2 / 53.5 / 59.3 | 73.4 / 49.5 / 51.5 |
| Qwen3.5 公式カードの同じ表: SWE-bench Verified | 72.4 | 69.2 |
| L107 の対応のある比較（`qwen3.8-27b` 対 `qwen3.6-35b-a3b`、MTP-Q4 同士、290 問、24 GB NVIDIA カード） | 74.1% | 70.0%（McNemar p=0.0501、差 +4.14 pt、95% CI 0.31〜7.96） |
| 公開の Terminal-Bench Core-19（Strix Halo） | 94.7% | 57.9% |

Qwen3.8-27B の公式カードは、Qwen3.6-27B をさらに上回ると言っている（SWE-bench Pro 61.7 対 53.5）。

### 速さは逆を向いている

Apple M5 Pro 48 GB の実測（`docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md`）で、`TurnSeconds` は 27B 228 秒・35B-A3B 70 秒。27B は線（190 秒）を超える。24 GB NVIDIA カードでは 27B も線の内側に収まる（L107 の実測 66〜81 秒）。

### 検討して採らなかった案

- **active パラメータ数で速さを並べる。** M5 Pro で active が 8.2 倍違うのに、速度差は decode 3.05 倍・prefill 3.56 倍だった。MoE の速さは active に比例せず、35B-A3B は dense の 8〜11B 相当になる。active の順で並べると 4B / 9B との順序を誤る。
- **35B-A3B を manual にする。** 27B が線を超えるホストで移り先が消える。また `qwen3.6-35b-a3b` だけを manual にすると、旧世代の `qwen3.5-35b-a3b`（tier 73）が 25〜78 GB を取る。
- **tier の入れ替えだけ。** `router.LighterCandidate` は重みが厳密に小さい候補しか受け入れない。27B Q4（17.7 GB）が線を超えたとき、35B-A3B の Q4（22.6 GB）と Q3（18.13 GB）は候補にならず、UD-Q2_K_XL（13.48 GB）が勧められる（main `156b28b9` で確認）。
- **active と総パラメータの両方から遅さを推測する。** 規則として閉じるが、推測より実測を採った（オーナー判断）。

## Decision

1. **`quality_tier` で、qwen3.8 の全 variant を `qwen3.6-35b-a3b` の全 variant より上に置く。** 同じモデルの中は精度の順（`TestBundledManifests_QualityTierFollowsPrecisionWithinAModel`）のまま。tier の一意性のために 35B-A3B 側を振り直す。理由の記載は `docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md` のとおり必須で、上の表を理由とする。
2. **段下げの行き先は、参照機での実測で選ぶ。**
   - 参照機は Strix Halo（128 GB のユニファイドメモリ機）。variant ごとに、製品の速度計測（`2245` の 1 リクエスト）を、製品がそのホストで使うエンジンと backend で測り、`TurnSeconds` をエンジン版・variant の digest・日付と一緒に残す。
   - 線を超えたと測られたら、「このホストで推奨の条件を満たし、参照機での `TurnSeconds` が active の variant より **5% 以上小さい**（候補 ≤ active × 0.95）」候補のうち、ランク順で最初のものを勧める。5% はオーナー判断 2026-09-16（「5%ぐらいにする。」）。
   - 1 歩ごとに参照機での秒数が 5% 以上下がるので、鎖は止まる。
   - 参照機での実測が無い variant の値は、次の順で引く（オーナー判断 2026-09-16: 「いまは基本は ollama の値を流用、なければ推測。vLLM でも同様の仕組みを用意する。」— 誤字と空白だけ整えた）。
     - (a) その variant 自身の、参照機での実測。
     - (b) 同じモデルの ollama の既定 variant（manifest の `default_variant`）の実測。
     - (c) オフラインで計算した推測値。同じデータファイルに、推測と分かる印を付けて置く。
     - データファイルは vLLM の記録も持つので、vLLM の値は参照機であとから埋められる。
   - **最初の選択には使わない。** 速度の予測で除外しない規則（`docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md` 決定 4）はそのまま。
3. **カタログに入れる条件は、参照機で 200k のコンテキストウィンドウ込みで完全常駐できること。**「完全常駐」の意味は `2355` 決定 10 のとおり。
   - ollama の variant は、参照機で推奨の判定（`hostfit.OllamaRecommendModel`）を得ること。
   - vLLM の variant は、製品の経路では参照機で動かせない（NVIDIA + Linux だけ）ので、200,704 トークンのコンテキストウィンドウで見積もった `min_vram_mb` が、参照機のアクセラレータメモリの予算（`OllamaVRAMBudgetMB`、98,304 MB）を超えないこと。
   - ネイティブのコンテキストウィンドウが 200,704 トークンに満たないモデルは、この条件を満たさない。
   - vLLM の variant は参照機で速度を測れないので、決定 2 の値は (b) か (c) で埋める。ollama の variant は参照機で測れる。
4. **gpt-oss-20b と gpt-oss-120b は、後継なしで退役する。** ネイティブのコンテキストウィンドウが 131,072 トークンで、決定 3 を満たさない。オーナー判断 2026-09-16: 「gpt-ossを退役させ後継なしでいい。またこれで不要になる、200kにコンテクストサイズが満たないモデルをclaude codeなどで使ったときの注意を促す機構など関連する機構も撤廃したい」。
   - 撤廃するのは、ネイティブのコンテキストウィンドウが 200k に満たないモデルを（Claude Code などから）使ったときに、注意を出す・扱いを変える機構。
   - 残すのは、別の理由で 200k 未満のコンテキストウィンドウで**配信している**コンピュータの扱い: vLLM の `--max-model-len` による切り詰め、まだコンテキストウィンドウを宣言していないエンジン、CI 専用の内部モデル granite4-350m（ネイティブ 32k）。
   - 実装はカタログの別の変更で行う。退役の表はいま後継（`SuccessorModelID`）を必須にしているので、その変更で後継を省略できるようにし、記録もその変更と一緒に残す。
5. **vLLM の variant の `min_vram_mb` と参照速度は、いまは推測で置き、あとで参照機で計測する。** オーナー判断 2026-09-16。原文は非公開のホスト名を含むので引用せず、趣旨は「参照機であとで計測するので、まずは推測で」。

## Consequences

- **決定 1 で適用した tier**:
  - `qwen3.8-27b`: fp8 90、mtp-q4-gguf 89、q3-gguf 87、q2-gguf 86（旧 72 / 71 / 66 / 65）。
  - `qwen3.6-35b-a3b`: mtp-q4-gguf 82、q4-gguf 81、mtp-q3-gguf 80、mtp-q2-gguf 79（旧 90 / 89 / 87 / 86）。
  - `qwen3.8-flash-next` は 91 のまま。
  - `qwen3.5-122b-a10b`（manual）は 83 → 78。これまでどおり `qwen3.6-35b-a3b` の全 variant の下に置くため。オーナーが決めたのは qwen3.8 対 35B-A3B の順序だけで、ほかの相対順序は保った。
  - gpt-oss-120b（88 / 85）は、退役するまで 27B の variant の間に挟まる。
- **既定の選択が変わる**（main `156b28b9`、KV `q4_0`。新しい tier で main `b3262756` でも確認した）:
  - NVIDIA 16〜18 GB → `qwen3.8-27b` UD-Q2_K_XL、19〜24 GB → UD-Q3_K_XL、25〜78 GB → Q4_K_M。
  - unified 22〜25 GB → UD-Q2_K_XL、26〜33 GB → UD-Q3_K_XL、34〜100 GB → Q4_K_M。
  - flash-next の帯（NVIDIA ≥79 GB / unified ≥101 GB）とそれ以下の帯は変わらない。
  - 旧世代の `qwen3.5-27b` Q4 が `qwen3.8-27b` Q3 より上に来る並び（tier 67 対 66）も消える。
  - エンジン版 0.32.13 未満は `qwen3.6-35b-a3b` のまま。qwen3.8 の全 variant が `min_engine_version` を持つため。
- **低ビットの帯では品質の差を確かめていない。** 同じメモリで比べた 27B UD-Q3 と 35B-A3B UD-Q2 は引き分けだった（コードは dense が +4.7 pt、推論は MoE が +5.0 pt）。`qwen3.8-27b` の UD-Q3_K_XL は attention に 2bit のテンソルを持つ（`docs/knowledges/20260914/0200-gguf-quant-tags-are-size-targets.md`）。
- **27B が線を超えるホストは、最初に 27B を 1 回ダウンロードして測ってから移る。** 予測で除外しない以上、この 1 回は払う。
- **文言を変える。** 段下げの提案は重いモデルを勧めることがあるので、「lighter model」（`cmd/waired/init_benchmark.go` と docs-site）を変える。文言は案を示してオーナーが確認する。
- **vLLM の参照機は別に要る。** 製品の vLLM の経路は NVIDIA + Linux だけで、参照機では測れない。値が揃うまでの vLLM の扱いは決定 2 と決定 5 で決めた: ollama の値を流用するか推測で置き、あとで参照機で計測する。
- **決定 3 を満たさない出荷中の variant は 7 つ**: `glm-5.2`（FP8 / NVFP4）、`deepseek-v4-flash`（FP8）、`gpt-oss-20b` と `gpt-oss-120b`（それぞれ ollama の `mxfp4-gguf` と vLLM の `mxfp4-safetensors`）。gpt-oss は決定 4 で退役する。`glm-5.2` と `deepseek-v4-flash` の扱いは waired-ai/waired#1427。それまではガードのテスト（`internal/hardware/catalog_admission_test.go` の `TestBundledCatalog_EveryBuildFitsTheReferenceHost`）で 7 つとも除外し、除外の理由はその行に書く。
- `qwen3.8-flash-next` の 4bit（111.33 GB、`2355` 決定 11 で「足してよい」）は、足す前に決定 3 で判定する。

## Refs

- waired-ai/waired#1357（L107、comment 5686058325）/ waired-ai/waired-agent#1400
- `docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md`（裁定 13）
- `docs/decisions/20260820/1130-step-down-walks-the-selection-ladder.md` / `docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md` / `docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md` / `docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md`
- https://huggingface.co/Qwen/Qwen3.6-27B / https://huggingface.co/Qwen/Qwen3.5-27B / https://huggingface.co/Qwen/Qwen3.8-27B
- https://github.com/hogeheer499-commits/strix-halo-guide（Strix Halo の Ollama で 27B Q4_K_M が prompt 292 tok/s・生成 20.4 tok/s。M5 Pro と同じ形）
