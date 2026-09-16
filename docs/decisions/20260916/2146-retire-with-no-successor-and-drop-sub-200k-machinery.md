---
status: accepted
supersedes:
  - docs/decisions/20260804/1943-retire-a-catalog-entry-with-a-successor-map.md
superseded_by:
  - docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md
---

# 退役は後継なしでもよく、200k に満たないモデルのための機構は撤廃する (20260916 21:46)

## Status

Accepted。オーナー判断 2026-09-16。waired-ai/waired-agent#1400 の実装計画を確認する場で、質問 Q1 への答えとして決まった。`docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md` の決定 4 を実装する記録。

オーナーの言葉（原文）: 「gpt-ossを退役させ後継なしでいい。またこれで不要になる、200kにコンテクストサイズが満たないモデルをclaude codeなどで使ったときの注意を促す機構など関連する機構も撤廃したい」

次の記録を**部分的に**置き換える。記録の `## Status` に鏡の一文を置いた。

`docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md`(オーナー決定 2026-09-16、waired-ai/waired-agent#1434)が、「残るもの」の 1 つ目のうち「vLLM の `--max-model-len` による切り詰め」を置き換える。vLLM は 200,704 か 1,048,576 で配信し、KV プールが 200,704 を保てないビルドはコンテキストウィンドウを宣言しない。まだ宣言していないエンジン(0)と granite4-350m(32k)は残る。

- `docs/decisions/20260804/1943-retire-a-catalog-entry-with-a-successor-map.md` §1 の「退役は必ず後継を名指す」（`retired_test.go` が強制していた）。後継は任意になる。ほかはそのまま有効: 事実は proto に・置換ポリシーはエージェントに、指示 / 観測の区別（§3）、名前は永久に予約、新しいピンを書く口への 409。

## Context

### gpt-oss-20b / gpt-oss-120b

- どちらもネイティブのコンテキストウィンドウが 131,072 トークンで、manual_only だった。
- カタログに入れる条件（`0340` 決定 3: 参照機で約 200k のコンテキストウィンドウ込みで完全常駐できる）は、ネイティブ 131k のモデルには満たしようがない。

### 退役の表に「後継なし」を書く方法が無かった

- 後継の解決に失敗した名前は、すべての利用側で「知らないモデル」と扱われていた。
- 409 の文面は `use "" instead` になるところだった。

### 選んでいたモデルが後継なしで退役したとき、ホストはどうなっていたか

- vLLM のホストは「no model has been chosen」を記録し、エンジンを起動しなかった。
- ollama のホストは古い重みを載せたまま、ルーティングからは何も届かなかった。
- これとは別に、preference ファイルの退役 id は `waired/default` を model-not-found にしていた。ルータがエイリアスだけで引くため。後継の有無に関わらず起きていた。

### 200k に満たないモデルの周りに組まれていた機構

- 自動選択のネイティブのコンテキストウィンドウの下限（`hostfit.NativeContextFloorTokens` / `MeetsNativeContextFloor`、`modelrank` の絞り込みパス 1）。
- 推奨判定の節 `window_too_small`（`hostfit.ReasonWindowTooSmall`。`OllamaRecommendModel` の節 1、`VLLMRecommendModel` の唯一の節）。
- ollama の tuning の警告「preferred model overrides the ~200k coding-agent context floor」/「configured model is below…」。
- init のピンの注記「not enforced for pins」と、選択の注記「this model's own window is below…」。
- CLI の `window_too_small` の節とトレイのツールチップ。
- docs の aside「Models with a 131k window」。

### 残るもの

- 別の理由で 200k 未満のコンテキストウィンドウで**配信している**コンピュータはこれからもある: vLLM の `--max-model-len` による切り詰め、まだコンテキストウィンドウを宣言していないエンジン（0）、CI 専用の内部モデル granite4-350m（32k）。
- ルータとゲートウェイの下限（`docs/decisions/20260916/0410-the-window-floor-is-the-same-on-every-client.md`）、Public Share の `min_context_window`、`CLAUDE_CODE_MAX_CONTEXT_TOKENS` は、モデルのではなく**配信している**コンテキストウィンドウで判定する。これらは残る。

## Decision

1. **gpt-oss-20b と gpt-oss-120b は後継なしで退役する。** `proto/catalog/retired.go` に行を足し、マニフェストは削除。`Retirement.SuccessorModelID` は空でよい。`retired_test.go` は、名指した後継が解決することを確かめる。空は許し、理由（`Reason`）は引き続き必須。
2. **いま出された指示は拒否する。** 文面は `catalog.RetirementRefusal`。
   - 対象: pull、モデルの切り替え、`POST /inference/preferred-model`、そのモデルを名指したルーティング要求。
   - 文面: `"<name>" was retired with no replacement; choose another model`。
   - ルータの not-found エラーがこの文を運び、409 `model_retired` もこの文を運ぶ。
3. **退役より前に書かれた名前は、このホストがいま渡されるモデルに落ちる。**
   - 対象: preferred-model.json、agent.json の bundled ピン（デーモンの `resolveWrittenModel`）、インストーラのピン（`setup.SelectBundledModel` が自動選択に落ちる）。
   - 落ち先は、offered カタログに対する `router.PickModel`。このホストと、このホストが配信に使うエンジンで引く。新規インストールが受け取る答えと同じ。
   - 名前ごとに 1 回、`catalog.RecommendedInsteadNotice` で記録する: `"<name>" was retired with no replacement; using "<id>", the model recommended for this computer, instead`。
   - 理由: 名指しの後継はホストに依らない落ち先で、後継が無いときのホストに依る落ち先が推奨モデルになる。拒否すると、ホストは古い重みを載せたまま何も配信しない。
   - 制御プレーンに保存された `desired_model_id` がこれを名指していても、**置換しない**（拒否されたモデルとして報告する）。制御プレーンはホスト固有の id に正規化できず、両端が収束しないため。
4. **`waired/default` は、書かれた名前を先に解決する**（`resolvedModelCfg`）。後継のある退役の穴も、これで閉じる。
5. **200k に満たない**モデル**の周りの機構を撤廃する。**
   - proto の識別子は残し、`// Deprecated:` を付ける（proto モジュールは追加のみ）: `NativeContextFloorTokens`、`MeetsNativeContextFloor`（hostfit と modelrank）、`ReasonWindowTooSmall`、`VLLMRecommendModel`（常に fit を返す）。
   - `DeclarableNativeWindow` は使い続ける。ホストが 1M のコンテキストウィンドウを宣言するとき、262k のモデルと 1M のモデルを見分けるのはこれ。
   - `modelrank.RankModels` は、自動の選択で internal_only のモデルを manual_only と同じく外す（明示のピンは届く）。CI 専用の granite4-350m（ネイティブ 32k）を自動の選択から外していたのは、ネイティブの下限だけだったため。
   - 受け入れのガード `internal/hardware.TestBundledCatalog_EveryBuildFitsTheReferenceHost` が、代わりにカタログをネイティブ ≥ 200,704 トークンに保つ。判定はカタログの時点。

## Consequences

- **ホスト。** gpt-oss を選んでいたホストは、この変更を含む版を動かすと推奨モデルに移り、1 回ダウンロードする。fleet の vLLM 検証機のうち gpt-oss-20b を配信している 1 台は、更新時に影響を受ける。
- **私設側の記録を散文で置き換える。** waired-ai/waired の決定 20260704/1239（#624 のネイティブ側の半分）、20260803/1332 決定 5（131k の帯は警告付きの opt-in）、20260804/1716（131k の mesh 参加条件）、20260812/2200（サイズ入りの `window_too_small` の文）は、200k に満たないモデルに関する部分が追い越された。リポジトリをまたぐ supersede は散文でしか書けない。制御プレーンと NAVI の `window_too_small` / `recommended_window_tokens` は、後続の waired の PR で落とす。
- **消した・反転させたテスト**（レビュアーが見えるように列挙）:
  - router: `TestMeetsNativeContextFloor`、`TestRankModels_PreferredBypassesFloor`、`TestRankModels_BestEffortFallbackWhenNothingServesFloor`、`TestLighterCandidate_StaysAboveContextFloor`、`floorCatalog` の下限未満のフィクスチャ。
  - agent: `TestModelDecisionReasons` の下限未満の 2 arm。
  - CLI: `TestNotRecommendedBecause_WindowTooSmallNamesTheSessionLimit`。
  - hostfit: `TestMeetsNativeContextFloor` のカタログを二分する半分。`TestVLLMRecommendModel` と `TestProjectModelVLLMWindowVerdict` は、コンテキストウィンドウの verdict が出ないことを固定する側に反転。
  - setup: ピンの注記の subtest は、注記が無いことを固定する側に反転。
- **汎用のコードは残す。** scoring / catalog-tool の sliding-window と MXFP4 の導出はアーキテクチャ一般の対応で、いまカタログに利用者がいないだけ。sliding-window のガードは空の表のまま残し、次にそういうモデルが来たら行が要る。
- **gpt-oss の agentgrade のレコードは残す**（`1943` §6）。request-shape の baseline のエントリと benchmarks の行は削除。

## Refs

- waired-ai/waired-agent#1400 / waired-ai/waired#1357 / waired-ai/waired#1427
- `docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md`（決定 4）
- `docs/decisions/20260804/1943-retire-a-catalog-entry-with-a-successor-map.md`
- `docs/decisions/20260916/0410-the-window-floor-is-the-same-on-every-client.md`
