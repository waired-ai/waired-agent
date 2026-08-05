---
status: accepted
---

# quality_tier は「載せると決めた世代の並び順」である (20260805 14:27)

## Status
Accepted

## Context

`quality_tier` の composite は 3 項からなる（`internal/catalog/scoring/tier.go:35`）:

```
composite = 10·log10(total_params) + 0.3·swe_bench_verified − 5·log10(footprint_mb)
```

`internal/catalog/benchmarks.json` は **21 のバンドルマニフェストに対して 7 行**しか持たず、
`swe_bench_verified` が非ゼロなのは **4 件**（deepseek-v4-flash 79.0 / qwen3.6-27b 77.2 /
qwen3-coder-next-80b-a3b 70.6 / gpt-oss-120b 62.4）。うち 2 件は `tier_override` で手ピン
されているので、ベンチ項が実際に位置を決めている variant は 2 つ。

さらに `internal/catalog/tier.go:128` は生の map index で、

```go
mb, hasBench := bench.Models[m.ModelID]
```

ミスは `swe = 0` になる。これは「未測定」ではなく **composite 上の 30 点ペナルティ**として
効く。行が無いことと実測ゼロが同じ入力になっている。実際、store の 3 行は
`swe_bench_verified: 0` を持ちながら実在する `secondary` を併記している
（glm-5.2 は swe_bench_pro 62.1 / terminal_bench 81.0、qwen2.5-coder 7b/14b は
humaneval_plus 88.0 / 89.0）。3 件とも意味は「見つからなかった」で、スキーマがそれを
言えない。偽ゼロの発生源はパイプラインにある — `scripts/catalog-radar/prompt.md:73-74`
が「SWE-bench Verified が見つからなければ `swe_bench_verified: 0` を書け」と指示している。

**結果として、21 件中 17 件で ladder は既にパラメータ数とフットプリントだけで並んでいる。
式はベンチ駆動を名乗りながら、それを使っていない。**

これは表記の問題ではない。`docs/decisions/20260802/1505-agentgrade-remeasured-retirement-basis-withdrawn.md`
が agentgrade を退役根拠から取り下げ、判断は #200 の quality tier 論に戻ると記録している。
その #200 の主張（「14b は strictly superseded、gpt-oss-20b が同メモリ級で上位」）は
tier 60/62 対 55/58 であり、**両モデルとも benchmarks.json に行が無い**。
退役プログラム全体が、どの測定も生んでいない順序に乗っている。

### 外部ベンチへの載せ替えを検討し、実測で全滅した

「能動的に更新されていること」を第一条件に据えて調査した。凍結したリーダーボードは、
凍結後に出たモデルを順位付けできない。

| ソース | 判定 |
|---|---|
| LiveCodeBench 公式 | **凍結**。`performances_generation.json` を実 parse: 29,540 行 / 28 モデル / 問題は 2025-04-07 まで / 最終データコミット 2025-05-29。**うちの 21 件は 0 件**。約 13 か月経っても v6 が最新 |
| LiveBench | 生きている（repo `pushed_at` 2026-08-04）が、**問題を入れ替えるたびに roster がリセットされ、落ちるのはオープンウェイト**。現行表 38 件中うちは 3 件 |
| Artificial Analysis | **ライセンスで失格**。ToU §2.2(d) がサイト内容の再公開を禁止し、再配布は Commercial ティアのみ（Free は "internal use only, no redistribution"、Pro $417/月/席でも "restricted external use"）。加えて `tiny` の 75% / `small` の 77% が `isEstimated: true`（未実行の推定値を測定値と同じ index バージョンで提示）。Coding Index は qwen3.5-0.8b = 0、2b = 2.92、4b/27b/35b-a3b/qwen3-coder-30b/480b/glm-4.5-air/qwen2.5-coder-7b/granite = null |
| SWE-rebench | 月次・実 GitHub issue・独立。ただし下限 27B、実スコアは 5 件 |
| Terminal-Bench | ベンダー自己提出（チームは trajectory を検証するのみ） |
| Aider Polyglot | 2025-11-20 で停止 |
| EvalPlus | 公開 `results.json`、sub-7B 56 件と厚い。ただし roster が旧世代で**うちは 0 件** |
| llm-stats / ZeroEval | 集約サイト。自身の API が `verified_count: 0`、全行が自己申告でベンダーブログ出典 |

2 つの事実で決着した:

1. **7B 未満のオープンウェイトを継続的に独立実測しているソースは存在しない。**
   無料エンドポイントを持つソースはすべて下限約 27B。0.9B まで届く唯一のソースは
   再配布不可であり、しかもその帯の 4 分の 3 を実際には走らせていない。
   qwen2.5-coder 3b/7b/14b と granite4-350m は**どの能動的独立ソースにも存在しない**
2. **ベンダー自己申告は互いに交換可能ではない。** gpt-oss-120b は LiveCodeBench で
   **82.7 / 81.9 / 42.68 / 83.2** に読める（実行者と reasoning effort による）。
   これらをまたいで集約したものは順位ではない

参考として、**このカテゴリの製品はどこも測定された品質指標を使っていない**。
LM Studio はダウンロード数・スター・サイズ・更新日での並べ替えと、基準非公開の
"Staff Picks"。Ollama は pulls と新しさ。Jan / GPT4All / llamafile は指標を持たない。
これは怠慢ではなく、同じカバレッジ問題の帰結。

## Decision

`quality_tier` を **「載せると決めた世代のパラメータ順」** と定義し直す。規則は 1 本:

> 世代・系列をまたぐ配置だけが maintainer 判断であり、その判断は
> **能動的に更新される独立ランナー**の出典を要求する。

| 帯 | 方針 |
|---|---|
| 低〜中 | **qwen3.5 に決め打ち**。またぐ比較が発生しないので出典は不要 |
| 高 | **複数系列を維持**。同時に収まる選択肢が実在し、系列ごとに性格が違う。またぐ配置は `tier_override` を通し、理由と出典を必須にする |
| 出典 | **能動更新の独立ランナーのみ** — 現時点では LiveBench 現行表と SWE-rebench。どちらも下限 27B |
| gpt-oss | **載せるが推薦しない**。カタログに出てユーザーが自分で選べるが、自動選定はしない。人気があるので落とさないが、頼まれてもいない人の前に出すものではない。何とも並べないのでベンチも不要 |
| 床 | `InstallQualityFloorTier` は tier 数値のまま、出荷が続くモデルに再アンカー |

### なぜこれで統一感が取れるか

調査が確定させた事実による: **外部データの存在と、系列をまたぐ比較の必要性が同じ向きに
相関している。** 小型ホストは 1 サイズしか動かせないのでハードウェアが選択を強制し、
比較が発生しない — そしてデータも無い。大型ホストは複数が収まるので選択が実在する —
そしてデータも実在する。だから **出典要求は高帯でだけ発火し、低〜中帯では自動的に空**
になる。2 つの制度を作らずに済む。

### 係数 0.3 は置換ではなく削除する

`tierBenchWeight` を別のベンチに向け直すのではなく、項ごと消す。上表のとおり
向け直す先が無い。ベンチ項が消えた composite は
`10·log10(total_params) − 5·log10(footprint_mb)` になる。

### `benchmarks.json` は証拠置き場として残す

composite の入力ではなく、`tier_override` が引用する外部スコアの置き場にする。
**行ごとに違うベンチを持ってよい** — 順位付けに使わないので、集約も較正も発生しない。
override は理由を必須にし、高帯の override は許可ランナー由来の出典を必須にする。

### rerank は計画から落とす

新定義では**コミット済みの ladder 自体がキュレーション結果**であり、
params/footprint から引き直す理由がない。`AssignTiers` は freeze のまま、新エントリは
composite で slot され、maintainer が override で調整する。
`catalog-tool tier --rerank` は診断ツールとして残すが、出荷カタログには当てない。

### 自前でハーネスを回す案は今回採らない

BigCodeBench と EvalPlus は Apache / MIT なので、自分で走らせた結果は自分のライセンスで
公開できる。**完全で比較可能な ladder を得る唯一の道**だが、GPU 時間の継続コミットメント
であり、この決定の付随物ではなく独立した判断に値する。将来必要になったら別 issue。

## Consequences

* **#200 がベンチ抜きで解ける。** qwen2.5-coder 3b/7b/14b は比較に負けたのではなく、
  **載せる世代ではないから**外れる。16GB ホストの受け皿は qwen3.5-9b。
  gpt-oss が推薦対象外になるので、後継として gpt-oss-20b を持ち出す必要も消える
* **tier の絶対値は動かない。** freeze が composite を読むのは新規（`quality_tier == 0`）
  variant に対してだけで、バンドル 34 variant はすべて非ゼロ（最小は granite4-350m の 11）。
  freeze でバンドル tier を動かせる唯一のベンチ由来入力は `tier_override` の 3 件で、
  いずれもコミット済み値と一致する。**将来 tier-0 variant が `bundled/` に入ると
  この不変性は失効する**
* **`manual_only` という新しい proto フィールドが要る。** 今の `internal_only` は
  `BundledManifests()` からエントリごと落とすので、「一覧には出るが自動選定されない」を
  表現できない。additive、`omitempty`、`internal_only` が優先
* **`InstallQualityFloorTier` の定義文が指すモデルが消える。** 再アンカー先は
  qwen3.5-2b（tier 27）を推奨する。根拠は自前の agentgrade — 退場する現アンカー
  qwen2.5-coder-3b は **12/72 失敗**（search-then-edit 9/24、Wilson 95% 下限 0.233）、
  qwen3.5-2b は **4/72 失敗**。つまり新アンカーは旧アンカーより実測で良く、数値が
  下がっても床の実質は下がらない。qwen3.5-4b（42）に上げると、**今日ローカル推論が
  動いている 4GB 級ホストで無効化される** — 再アンカーの範囲を超える別の判断
* `internal/catalog/scoring/tier.go:6` の裸 `#133` は公開リポで無関係な issue に解決する
  （正しくは `waired-ai/waired#133`）。`:15-16` が参照する
  `docs/knowledges/.../catalog-scoring-formula.md` は存在しない。本 PR でノートを書き、
  引用の修正は composite の PR で行う

## Refs
- https://github.com/waired-ai/waired-agent/issues/518（tracker）
- https://github.com/waired-ai/waired-agent/issues/519 / #520 / #521 / #522 / #523
- https://github.com/waired-ai/waired-agent/issues/200
- waired-ai/waired#133（private、SWE-bench Verified を主信号に選んだ CLOSED issue）
- docs/decisions/20260802/1505-agentgrade-remeasured-retirement-basis-withdrawn.md
- docs/decisions/20260801/1900-ci-fixture-model-withheld.md
- docs/knowledges/20260805/1427-catalog-scoring-formula.md
