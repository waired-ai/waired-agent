# カタログの品質スコア式と、外部ベンチマーク調査 (20260805 14:27)

`internal/catalog/scoring/tier.go` が参照しているノート。式の根拠と方向性チェック、
および「外部ベンチを主信号にできるか」の調査結果を残す。

## Issue

`scoring/tier.go` の doc コメントが
`docs/knowledges/.../catalog-scoring-formula.md` を引用しているが、
そのファイルはリポジトリのどこにも存在しなかった。読んだ人が探して見つからない状態が
続いていた。同じコメントが引用する `#133` も、公開リポでは無関係な issue に解決する
（正しくは `waired-ai/waired#133`、private）。

## Learnings

### 式の形と各項のスケール

```
composite = tierParamWeight·log10(total_params) − tierVRAMWeight·log10(footprint_mb)
            (10.0)                                (5.0)
```

`CompositeScore` の**絶対値には意味がない**。variant 間の順序だけが意味を持ち、
`catalog.AssignTiers` がそれを一意な整数 tier に写す。

| 変化 | composite 差 |
|---|---|
| パラメータ数 ×10 | +10.00 |
| パラメータ数 ×2 | +3.01 |
| パラメータ数 3B → 480B | +22.04 |
| パラメータ数 0.8B → 480B（提供レンジ） | +27.78 |
| フットプリント ×2 | −1.51 |

フットプリント項が緩い penalty（レンジ全体で約 11）なのは意図的で、同じ重みなら
小さく載るほうを選ぶが、サイズ差を覆すほどではない。`footprintMB` には 1024 の
下限がある（`log10(0)` 回避と、極小の申告フットプリントを過剰に報いないため）。

**パラメータ数は total を使う（active ではない）。** scoring report §5.2 の
"active_params" という表記はここで上書きされている: active を使うと 30B-A3B の MoE が
7B dense を下回り、キュレーション済みの ladder とも Phase 7 の router スコア
（`ParamCount × QuantizationTier`）とも矛盾する。

### かつて第 3 項にベンチマークがあった

2026-08-05 まで `+ 0.3·swe_bench_verified` があった。削除の経緯は
`docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md`。要約すると、
提供 20 モデル中スコアを持つのが 4 件で、`internal/catalog/tier.go` の map ミスが
`swe = 0`（＝ 30 点ペナルティ）になるため、**17 件では実質パラメータ数と
フットプリントだけで並んでいた**。

### 外部ベンチマーク調査（2026-08-05 実施）

「主信号を差し替える」ために能動更新の独立ソースを探した結果。
**カバレッジより先に「まだ更新されているか」で落ちるソースが多い。**

| ソース | 最終更新（実確認） | 独立性 | うち 21 件のカバー | 備考 |
|---|---|---|---|---|
| LiveCodeBench 公式 | **2025-05-29**（データコミット） | 独立 | **0** | `performances_generation.json` を実 parse: 29,540 行 / 28 モデル / 問題は 2025-04-07 まで。v6 が最新のまま約 13 か月 |
| LiveBench | repo 2026-08-04 / 問題セット 2026-06-25 | 独立 | 3 | **問題入替のたびに roster がリセット**され、落ちるのはオープンウェイト。現行表 38 件。過去表の数字は現行表と比較不能 |
| Artificial Analysis | index v4.1 2026-06-15、継続追加 | 独立（方針上） | ~13 | **ToU §2.2(d) が再公開を禁止**。再配布は Commercial のみ（Free = "internal use only"、Pro = "restricted external use"）。`tiny` の 75% / `small` の 77% が `isEstimated: true` |
| SWE-rebench | 2026-07-28（HF dataset） | 独立 | 5 実スコア | 月次、実 GitHub issue。**下限 27B** |
| Terminal-Bench 2.1 | 2026-07-11（提出） | **ベンダー自己提出** | — | チームは trajectory を検証するのみ |
| Aider Polyglot | **2025-11-20** | 独立 | 7 | 停止 |
| EvalPlus | 不明（HumanEval+ は v0.1.10 で凍結） | 検証のみ | **0** | 公開 `results.json`、sub-7B 56 件と厚いが roster が旧世代 |
| llm-stats / ZeroEval | 2026-08 | **集約サイト** | 8 | 自身の API が `verified_count: 0`、全 53 行 `self_reported` |

**機械可読エンドポイント**（将来再調査する人向け）:

* LiveBench: `https://livebench.ai/table_YYYY_MM_DD.csv`（無認証、現行は `table_2026_06_25.csv`）
* LiveCodeBench: `https://livecodebench.github.io/performances_generation.json`（生の問題別レコード。pass@1 は自分で計算する）
* llm-stats: `https://api.zeroeval.com/leaderboard/benchmarks/livecodebench-v6/details`（無認証だが自己申告データ）
* EvalPlus: `https://evalplus.github.io/results.json`
* Artificial Analysis: `https://artificialanalysis.ai/api/v2/openapi`（spec は無認証で読めるが、データは要 API key。転記は ToU 違反）

**結論**: 7B 未満のオープンウェイトを継続的に独立実測しているソースは存在しない。
qwen2.5-coder 3b/7b/14b と granite4-350m はどの能動的独立ソースにもいない。

### ベンダー自己申告は互いに比較できない

gpt-oss-120b の LiveCodeBench は、出典によって **82.7 / 81.9 / 42.68 / 83.2** に読める。
reasoning effort と実行者で動く。**版・窓・実行者・effort の 4 つが揃っていない数字は、
このファイル内の他のどの数字とも比較できない。**

LiveCodeBench はスライディング窓でもある（v1 は 400 問 / v6 は 1055 問、いずれも
2023-05 起点の累積）。窓が違えば同一モデルでも 20 点以上動く。

### 同カテゴリの製品は測定指標を使っていない

LM Studio（ダウンロード数・スター・サイズ・更新日 ＋ 基準非公開の "Staff Picks"）、
Ollama（pulls・新しさ）、Jan / GPT4All / llamafile（指標なし）。
前例が無いことの記録であって、真似すべきという意味ではない。

### 自前で回すなら

BigCodeBench と EvalPlus は Apache / MIT のハーネス。**自分で走らせた結果は自分の
ライセンスで公開でき、帰属制約もつかない** — roster を自分で決められるので 0.8B から
480B まで同じ物差しに載る。上のライセンス問題を回避する唯一の道でもある。
コストは GPU 時間の継続コミットメント。

## Refs
- internal/catalog/scoring/tier.go
- internal/catalog/tier.go（`AssignTiers`、freeze / rerank）
- docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md
- https://github.com/waired-ai/waired-agent/issues/518
- waired-ai/waired#133（private、旧主信号を決めた issue）
