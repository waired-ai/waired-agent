---
status: accepted
---

# 実測は推奨の入力に戻り、梯子は 1 本になる (20260822 19:35)

## Status
Accepted

## Context

`docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md`
決定 4 は `RankModels` から速度による除外を外し、こう書いて復帰経路を予約していた:

> 速度は実測が入ってから推奨の入力に戻す (waired-ai/waired-agent#466)

同じ文が control plane 側の `confirmedSlow` にも独立に書かれていた。予約されたまま
2 週間が経ち、その間 rc9 の 3-OS 実機検証が**予約が必要だった理由そのもの**を撮って
いた（waired-agent#784）:

- macmini: 4B を 26 tok/s と実測 → 87 秒後もそれを `recommended_pick`（しかも削除済み）
- xps15: 9B を 11〜12 tok/s と実測 → 段下げは正しく動いたのに、バッジは 9B のまま

#784 は当初これを「ダウンロード前に**予測値**で弾け」と申し立てていた。オーナー裁定
（2026-08-21）で却下 — 最軽量すら動かせない端末を救う意味はなく、多段ダウンロードは
予測が困難である以上仕方なく、段下げも軽いモデルの選択もユーザーの選択である。

**却下されたのは申立てであって、証拠ではなかった。** 証拠は予測ではなく実測に基づいて
おり、それは決定 4 が明示的に予約していた経路そのものだった。

## Decision

### 1. 実測したモデルは推奨から外れ、1 段下が受け取る

`RankModels` に 3 段目の `narrow` を足す。除外の根拠は**このホストが実際に出した値**
のみで、予測値（roofline）は従来どおり注記するだけで除外しない。

決定 4 が予測を禁じた理由 2 つは実測に届かない:

- 分母が無い。`BandwidthSystemRAMGBs` のような母集団定数が無いので、`ClassCPUOnly` が
  免除されて `ClassDiscrete-spilled` が免除されない、という順序の逆転が起きない。
- decode 専用でない。予測は decode しか価格を付けていなかったが、実測はそのマシンが
  実際に出した値である。

**`narrow` の段であって、フィルタではない。** 全部が遅いと実測されたホストでは素通り
してバッジが残る。ローカル AI を失わせるのは waired#1056 決定 1 が禁じる唯一の答え。

台帳は**変種ごとの map**（`State.MeasuredVariants`、鍵は `VariantSHA`）。1 件だけ
覚える設計では、9B を遅いと実測 → 段下げ → 4B も遅いと実測、の 2 段目で 9B の除外が
上書きされ、梯子が降りずに振動する。

### 2. 床未満は「軽いモデルがある」とは別の主張である

最軽量モデルを実測したホストには段下げ先が無いので `Recommendation` が nil になり、
CLI はその不在を「十分速い」と読んでいた。`BelowFloor` / `FloorTokps` を管理 API に
足して 2 つを分ける。判別は `isLightestOfferedModel`（順序判定）で、提案の不在では
ない — 提案はエンジン選択の失敗などでも消えるので、それに「推論を切るか」を訊くのは
誰も尋ねていない質問に答えることになる。

### 3. 梯子は `proto/modelrank` に 1 本だけ置く

CP は同じ問いに自前の実装で答えていた（自前の候補ループ、自前の `narrow`、自前の
tie-break）。`proto/hostfit` が作られた原因（waired#942）と同じ形で、waired#986 は
まさにこの関数の drift — ネイティブ窓の床がこちらでは散文、あちらでは実装だったため、
16 GB カードに 22.6 GB の MoE が既定になった。

`internal/router` は `hardware.Profile` 型の扉に徹し、**fit / sizing の算術を一切
持たない**。持たなければ drift しようがない。

### 4. エンジン床の規則は 1 つ、`""` の意味だけが呼び出し元のもの

agent は fail closed、CP は fail open で、2 つの規則に見えていた。実際は 1 つの規則で、
**空文字列の意味**が違う — agent は「自分のマシンを見て取れなかった」、CP は「端末が
まだ申告していない」。後者はベンチ未実施の全端末を覆う（waired#1225）。

沈黙の意味を入力（`UnknownEngineVersionPasses`）にして、規則を 1 本にする。さらに
`InferenceState.ServingEngineVersion` を単独で載せ、不明の母集団を「フリートの大半」
から「この版より古い agent」に縮める。

### 5. 誰も回さないつまみは公開しない

`RequireCapability` / `NoContextFloor` / `NoRecommendGate` は本番の書き手がゼロ
だった（#522 が stand-down の必要性を消し、`install_picker.go` 自身がそう書いている）。
proto に載せると永久に消せないので載せない — 決定 4 が速度ハッチを却下したときの
言葉「何も無効化しないフィールドは無いより悪い」がそのまま当てはまる。additive なので、
呼び出し元が現れた日に足せばよい。

## Consequences

- **`#466` は閉じない。** あちらは install 時の帯域プローブから**予測**でランクする話で、
  その §4 自身が「install 後の実モデルベンチが補正役として残る」と書いている。本決定は
  その補正役の半分を着地させたもの。
- **設定された床は wire に無いので、CP は既定値で判定する。** `interactive_floor_tokps`
  を動かしたホストは、ブラウザ面と端末で 1 段ずれ得る。設定が触られることは稀で、
  判定される実測値は同じなので受容するが、呼び出し元にその旨を記録した。
- **`isLightestOfferedModel` は畳まなかった。** ホスト非依存の順序判定（doc:
  *"An ORDERING, not a floor"*）で、`RankModels` はホストで絞り込むため、畳むと軽い
  モデルが載らないホストで答えが変わる。3 本目の実装に見えるが、別の問いに答えている。
- **不変条件 1 つを言い換えた。** decision 20260805/1620 決定 6 のガードは
  `NoRecommendGate` を使って「ゲートは判定を変えない」と述べていた。ハッチが無くなった
  ので「判定は**容量**にのみ追随する」と正の形で述べ直した。こちらの方が強い — 前者は
  ゲートが不活性だと言うだけだが、後者は `ok=false` に到達してよい唯一の拒否
  （確実な OOM）を名指しし、他を全部禁じる。切替前のコードで成立を実測してから書いた。
- **`protoconsumer` が移設を検出した。** 梯子が出ていった瞬間 `Pick.Manifest` などが
  producer を失って赤くなり、`producedInProto` に移った。逆に
  `UnknownEngineVersionPasses` は本リポジトリが書くようになったので exemption が外れた。

## Refs
- https://github.com/waired-ai/waired-agent/issues/784 / #794 / #970 / #971
- PR #969 / #972 / #974 / #976、waired-ai/waired#1257、`proto/v0.2.55`
- `docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md` 決定 4
- `docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md` 決定 6
- `docs/decisions/20260820/1130-step-down-walks-the-selection-ladder.md`
- waired-ai/waired#1056 決定 1 / #942 / #986 / #1225 / #1250
