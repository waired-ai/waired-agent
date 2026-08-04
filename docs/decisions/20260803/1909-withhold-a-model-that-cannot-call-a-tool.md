---
status: accepted
superseded_by:
  - docs/decisions/20260804/1943-retire-a-catalog-entry-with-a-successor-map.md
---

# ツールを呼べないモデルは提供から外す（退役までの暫定）(20260803 19:09)

## Status

Accepted。ただし**「削除ではなく withhold にする」半分は 20260804 に
superseded**（`docs/decisions/20260804/1943-retire-a-catalog-entry-with-a-successor-map.md`）。
下に書いた exit condition —「#200 の機構が入ったら 0.5b を消す」— はそのとおり
果たされ、qwen2.5-coder-0.5b はカタログから削除された。その名前は
`catalog.Retirements()` 経由で qwen3.5-0.8b に解決する。

`--require-pass` を ci.yml で有効にする判断と、その根拠（Wilson 95% 下限を
`RetireFailureRate` と比べ、24 試行なら 17/24 で初めて鳴る）は引き続き有効。

## Context

`catalog-tool agentgrade` は #322 の計測器で、`--check`（カバレッジ）と
`--require-pass`（退役ワークリストが空であること）の 2 つを持つ。ci.yml では
`--check` だけが有効で、`--require-pass` は「今日の既知状態を赤にしないため」に
切ってあった。結果として、**コーディングエージェントを駆動できない新規エントリが
カタログに入るのを止めるものが何も無い**——#322 が開かれた原因そのものが、
その門番を止めている理由になっていた。

「今日の既知状態」は測ってみると 1 件だけだった。
`qwen2.5-coder-0.5b-instruct/q4-gguf` は、ツールを要求する 2 ケース
（`read-file` / `search-then-edit`）の**両方で 24/24 失敗**、いずれも
`fail_unstructured_tool_call`。真の失敗率は 95% 信頼で **90% 以上**。次点は
qwen2.5-coder-3b の 23% で、ほぼ 4 倍の開きがある。

`--require-pass` が読む `Failures()` は #462 以降 Wilson 95% 下限を
`RetireFailureRate = 0.5` と比べる。24 試行では **17/24** で初めて越える。
つまりこの門番は「たまに落ちる」では鳴らず、「働くより多く失敗する」だけを
捕まえる。3 択で迷ったのは、その 1 件をどう外すかだった。

## Decision

**`internal_only` で提供から外す（withhold）。削除はしない。**

外して問題ない根拠は測定済み：

* RAM 2–24 GB のどこでも **自動選定されない**。32k ネイティブなので
  `hostfit.NativeContextFloorTokens = 200000`（#624）が除外する
* **フロア下フォールバックの選にも入らない**。2–3 GB は qwen3.5-0.8b
  （tier 12 > 10）
* `waired/tiny|small|medium` の **どれも指していない**
  （tiny=granite4-350m, small=3b, medium=14b）

`internal_only` は「人が見る／配られるもの全部から外すが、id・エイリアスでの
解決性は保つ」機構なので、**既存のピンは壊れない**（pull も serve もできる）。
前例は granite4-350m で、その理由文はすでに品質判断
（"At 352M it produces nothing worth giving anyone"）を根拠にしている。
`InternalOnly` の doc にある「withholding は quality と直交」は *フィールドが
品質ランキングではない* の意味であって、品質所見を理由にできないという意味では
ないと読む。

**削除しなかった理由**：#200 の退役→後継マップと、コントロールプレーン側の
カタログ所属検証がまだ無い。今エントリを消すと、ピン留めしている利用者に
移行ではなく `ErrModelNotFound` が返る。

### withhold が終着点にならないようにする

`internal_only` に置いたものが何年も居座るのが一番ありがちな失敗なので、
人が読む所と機械が見る所の両方に置いた：

1. **理由文**が「退役待ちの暫定」であることと追跡先（#475 / #200）を持つ。
   granite4-350m の「CI フィクスチャなので恒久」とは性質が違うことを明記
2. **レポートの `WITHHELD, PENDING RETIREMENT` 節**（`printWithheldPendingRetirement`）
   が、`internal_only` かつ退役ラインを越えているエントリを、レートと理由文つきで
   毎回列挙する。**報告専用でゲートではない** —— ゲートにすると誰も消せない赤に
   なり、恒久的な赤は無視される。それこそがこの節の防ごうとしている結末
3. **`TestFailuresOnTheShippedCatalog`** が「offered は空」と「withheld 側に
   0.5b が居る」を両方主張する。空という 1 つの事実が
   「線を越えるものが無い」と「線を越えたものは外してある」の 2 つを意味しうるので、
   後者であることをテストに書いた
4. `agentgrade.json` の `notes` が、レート表に居ないことを「綺麗な結果」と
   読まないよう警告する

## Consequences

* **ci.yml が `agentgrade --check --require-pass` になった。** 以後、offered な
  カタログに「働くより多く失敗する」と 95% 信頼で言えるエントリが 1 つでも入ると
  lint が落ちる。赤を消すために閾値を動かすのではなく、withhold するか退役させる
* `docs/reference/models.md` から 0.5b の 3 行が消え、bundled は 21 → 20 ファミリ
* **ホスト影響ゼロ・ピン影響ゼロ。** 上記の測定どおり
* **#200 は依然として必要。** withhold は削除の代替ではない。#200 の機構が入ったら
  0.5b を消し、この暫定状態を終える（exit condition）
* あわせて、#467 がデータだけ差し替えて置いていった散文 6 か所を実測に合わせた。
  同じ陳腐化が起きないよう、`RetireFailureRate` の根拠が引用している 3 つのレートを
  `TestRatesCitedByRetireFailureRate` が出荷ストアから読み直す

## Refs
- https://github.com/waired-ai/waired-agent/issues/475
- https://github.com/waired-ai/waired-agent/issues/322
- https://github.com/waired-ai/waired-agent/issues/200
- docs/decisions/20260801/1900-ci-fixture-model-withheld.md（granite4-350m の前例）
- docs/decisions/20260803/1454-agentgrade-retirement-threshold-is-a-rate-bound.md
- docs/decisions/20260803/1705-agentgrade-remeasured-after-stream-retry.md
