---
status: accepted
---

# vLLM のサイズ計算は hostfit が持ち、推奨はホストを見る (20260828 17:30)

## Status

Accepted。waired-ai/waired-agent#1061 の設計判断。
`docs/decisions/20260821/…` 系ではなく、`proto/hostfit` と `proto/modelrank`
の層の向きについての記録。

## Context

waired-agent#1029 の症状1を閉じた PR#1044 は、vLLM の推奨判定に**1 節だけ**
入れた —— モデル自身の窓が ~200k に届くか。これはマニフェストについての事実で、
エンジンもハードウェアも動かさない。

入らなかったのは「**このホストなら実際にその窓で配信できるか**」で、
`OllamaRecommendModel` の第3節にあたる。vLLM にも等価物はある
（`VLLMMaxModelLen` = 起動時のプール確保から導く `--max-model-len`）が、
それが `proto/modelrank` にあり、`modelrank` は `proto/hostfit` を import して
いる。逆向きには書けない。

結果として、24GB のカードで**native 200k 超だが実際にはクランプされるモデル**が
ウィザードの vLLM タブに無印で並んだ。同じ機械のエージェントはそれを知っていて、
tuning warning で「context window clamped to 124928 tokens … below the ~200k
coding-agent context target」と言っていたのに、その事実が picker の判定に
届いていなかった。

## Decision

**1. vLLM の算術は `proto/hostfit` へ移す。**

`modelrank/vllm.go` の中身は modelrank の未公開定数 2 つ以外に依存が無く、
唯一の hostfit 呼び出しは既に公開されている `MaxContextTokens` だった。
`hostfit` は既に `proto/signer` を import している（`FromHardwareSummary`）ので、
`[]signer.HardwareGPUSummary` は**依存を 1 つも増やさずに**届く。

「サイズが理由」ではなく「問いが理由」で分けるという `modelrank` の
パッケージ doc の原則に沿う: hostfit は「このモデルは収まるか、このホストは
その窓を配信するか」を per-model で答える。vLLM のクランプはまさにその問いで、
最初から hostfit 側の問いだった。

**2. `modelrank` の公開名は委譲ラッパとして残す。**

`proto-additive-guard` は公開 func の削除と署名変更を禁じる。公開 const は
**書かれたとおりの値のテキスト**を比較するので、`= hostfit.X` への書き換えは
「値が変わった」と読まれる。よって `KVFactorF16` / `KVFactorFP8` /
`DefaultVLLMGPUMemoryUtilization` はリテラルのまま両方に置き、
`TestVLLMConstsMatchHostfit` で結び付ける。重複だが、guard の下で取れる形は
これしかない。

**3. `hostfit.Host` に GPU リストは足さない。**

`modelrank.PickInput.GPUs` の doc が「per-device の詳細は Host が**意図的に**
持たない」と明記している。Host は決定への**入力**であり小さな数値の集合で、
両側が一度だけ適応するもの。ここにスライスを足すと、その性質が変わり、
`FromHardwareSummary` を通る全消費者の意味が静かに動く。

代わりに **入力構造体 `hostfit.ModelProjection` + `ProjectModelFrom`** を足す。
`ProjectModel` の署名は公開済みで 6 つ目の引数を取れない。`PickInput` が
Host の隣に GPUs を置いているのと同じ形になる。旧エントリポイントは
`GPUs: nil` で委譲するので、**既存の全呼び出し元でバイト同一**。

**4. `VLLMRecommendModel` は据え置き、新しい節は
`VLLMRecommendModelOnHost` に置く。**

署名が凍結されているだけでなく、デバイス情報を持たない呼び出し元にとっては
マニフェストだけの判定が**正直な答え**である。

vLLM の第2節（重みが収まるか）は繰り返さない。それは VRAM 予算に対する
`VLLMFit` で、`ProjectModelFrom` が既に**容量**として訊いている。容量だけが
断ってよい規則（waired-agent#229）なので、推奨としてもう一度訊けば、容量が
既に断った行をさらに降格することになる。

**5. `NeedMB` / `HaveMB` は埋めない。**

ここで収まらないのは**トークン数**で、それをメガバイトに翻訳するのは校正の
無い二つ目の算術になる。コンソールはこの理由に対してサイズを取らない文面
（`setup_rec_window_memory`）を既に持っている。

## Consequences

- ウィザードの vLLM タブと、エージェント自身の family 行の両方が、クランプする
  ホストで降格を出す。CP は proto を bump して `hw.GPUs` を渡すだけで、
  **コンソールの文言変更は不要**。
- `RankModels` の vLLM 腕が `Pick.Recommendation` を埋めるようになった。
  **降格の結果は変わらない**（同じ事実が既に `gateOK` の
  `VLLMServesContextFloor` にある）が、常時 `{Fits:true}` ではなくなり、
  プールから外れた理由が Pick に載る。
- `modelrank.Pick.Manifest` の protoconsumer 免除が消えた。guard は
  フィールド**名**で突き合わせるので、`ModelProjection.Manifest` を
  internal/router が埋めた時点で「producer が居る」と見える。
  `Pick.Manifest` の書き手は今も `RankModels` だけで、動いたのは名前のほう。
  同ブロックの `Variant` / `Reasons` / `Recommendation` が最初から
  免除に載っていないのと同じ理由。
- テストは**1 本も反転していない**。既存の vLLM 判定テストは
  `presVLLM` に重み / KV 注記が無く、「入力不明なら寛容」の規約で通る。
- **まだ入っていない節**: ホスト側の窓判定は入ったが、`#575` が残っている —
  今のカタログには 24GB 未満の vLLM 変種が無いので、この判定が実機で効く帯は
  まだ狭い。広がるのは vLLM の棚が広がったとき。

## Refs

- waired-ai/waired-agent#1061, #1029, #575
- `proto/hostfit/vllm.go`, `proto/hostfit/window.go`, `proto/modelrank/vllm.go`
- waired-ai/waired#1283 (L75)
