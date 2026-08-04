---
status: accepted
supersedes:
  - docs/decisions/20260803/1909-withhold-a-model-that-cannot-call-a-tool.md
---

# 退役は後継マップで行い、qwen2.5-coder-0.5b を最初の利用者にする (20260804 19:43)

## Status

Accepted。`docs/decisions/20260803/1909-withhold-a-model-that-cannot-call-a-tool.md`
の**「削除しない」半分だけ**を supersede する。あの記録の判断（`--require-pass` を
有効化し、0.5b を withhold する）は正しく、その exit condition
（「#200 の機構が入ったら 0.5b を消す」）を今回果たした。`--require-pass` に関する
部分は引き続き有効。

## Context

#200 は「陳腐化した qwen2.5 エントリを退役させる。ただし退役マシナリを先に作れ」。
マシナリは**存在しなかった** — `grep -i "retired|successor|deprecat|superseded"` を
`proto/ internal/ cmd/` にかけてモデル関連のヒットはゼロ。カタログ解決は
`proto/catalog/manifest.go` の `LookupByAlias` 一本で、ミスは `(Manifest{}, false)`
というエラーですらない値を返し、約 20 の非テスト呼び出し元がそれぞれ独自のミス処理を
発明していた。この状態でエントリを消すと、そこにピンしたユーザは移行ではなく
`ErrModelNotFound` を受け取る。

ピンは 5 か所に散っている: `agent.json` の `bundled_model_id`、
`preferred-model.json`、`state.json` の `Active.ModelID`、CP の
`desired_model_id`、コーディングエージェントの設定ファイル。

## Decision

### 1. 事実は proto に、置換ポリシーはエージェントに

`proto/catalog/retired.go` に `Retirement{Names, SuccessorModelID, Reason}` と
`Retirements()` / `LookupRetirement()`。ワイヤに乗らないコンパイル済みデータで、
json タグは持たない。

**proto に置く理由**は、CP も同じ表を必要とし、しかも**逆のことを必要とする**から。
エージェントは置換する（設定ファイル・CP の行・到来リクエストの名前を後継へ）。
CP は拒否する（新規の `desired_model_id` が退役名を指すのは、取り下げたものを
operator が要求している状態で、そう答えるべき）。どちらのポリシーもこのパッケージの
ものではない。事実だけが proto にある。

`Names` が id とエイリアス**全部**を持つのは load-bearing: `model_aliases` は削除する
JSON の中にあり、消えると他のどこからも復元できない。

### 2. `IsValid` / `IsRetired` の 2 述語に割らない

`proto/signer/integration.go` の退役統合先はこの形だが、あちらはペイロードの無い
enum なので「valid」側が「はい」としか答えられない。こちらの valid 側はリストではなく
**22 エントリの埋め込みカタログというデータ**で、その隣に手で保つ第 2 の真実の出所を
置けば、次にマニフェストを足した人が確実にずらす。CP の拒否 / 移行の分岐は 2 値の
戻りで足りる: `LookupByAlias` ミス ＋ `LookupRetirement` ヒット＝「引っ込めた」、
両方ミス＝「聞いたこともない」。

### 3. 置換してよい所とダメな所 — instruction / observation

> **名前が「指示」なら置換する**（何を取ってきて、動かして、経路に載せるか）。
> **名前が「観測」なら置換しない**（ディスクに何があるか、その tier は何か、
> 宣言している窓は何か）。

机上の区別ではない。`DeclaredContextWindow` が証拠で、0.5b はネイティブ 32k、
qwen3.5-0.8b は 262k。ここで置換すると**実際には古い 32k の重みを動かしている
ホストがメッシュに 262k を宣言する** — そのフィールドの doc がまさに防ぐために
存在する嘘。`resolveTuningTarget` は 1 つの関数に両方が入っている（preferred /
bundled は指示、`state.Active.ModelID` は観測）。`LookupByAlias` 自体を書き換える
一括対応が近道ではなく欠陥である理由がこれ。

置換: router の `resolveModel`（唯一の serve 経路リゾルバ）、`bundledModelID`、
`preferredManifest`、`PullModel`、`SwapPreferredModel`、tuning の leg 1・3、
setup の fold、`canonicalBundledModelID`、`SelectBundledModel` の pin leg。

### 4. 新しいピンを書く口だけは拒否する

`POST /waired/v1/inference/preferred-model` は退役名に **409 `model_retired`** ＋
後継名を返す。このハンドラは `model_id` をエコーして `SavePreference` するので、
黙って置換すると operator が選んでいないピンを書き、頼んでいない id を返す。
トレイのメニューは offered カタログから作るのでこの枝に到達できず、届きうるのは
古いタブ・スクリプト・昔の値を再生する `init` — つまり「引っ込めたものを名指しした
要求」で、名前のある答えに値する。

setup reconciler の入力は**何か月も前に書かれたかもしれない CP の保存行**なので
移行する。非対称は `IsRetiredIntegrationTarget` と同じ分岐。

### 5. 収束のハングを 1 か所で直す（副次だが独立した欠陥）

`desired_model_id` は一度も解決されずに setup 状態へ折り込まれる一方、収束判定は
`setupPreferredModelID()` との**生の文字列比較**。両端の綴りが違うと
`setupModelState` が `state.Models`（正規 model_id で keyed）でミスし続け、
`converged` が恒久的に false になる。ウィザードの「Download the AI model」行が
pending のまま止まり、`modelApplied` はプロセス内メモリなので**収束済みホストが
毎起動エンジンをバウンスする**。

`setupCanonicalModelID` を `setupProvider` に足し、`d` を組み立てる 1 か所で呼ぶ。
**これはエイリアスでも今日壊れている**: 保存済み `waired/medium` は origin/main でも
収束しない。A/B で確認済み（canonicalisation を外すと alias ケースも同じ形で赤）。

`cmd/waired/init_modelselect.go` の `canonicalBundledModelID`（CLI 側の対）にも
同じ退役解決を入れた。なおこの関数がフィルタ済み集合を引いていた件（`waired/tiny`
が解決できずウィザードが永久に待つ）は、本 PR の作業中に **#495 が並行して修正済み**。
残っていたのは退役解決だけ。

### 6. 削除しても証拠は残す

`internal/catalog/agentgrade.json` の 0.5b レコードは**残す**。退役の理由文が
90% の下限を引用しており、その数値が書かれているのはこのファイルだけ。
`TestAgentGradeKeysExistInCatalog` を「shipped **or retired**」に緩めた
（タイプミスは従来どおり捕まる: shipped でも retired でもない名前は落ちる）。

## Consequences

* **0.5b のマニフェストが消え、3 つの名前が `qwen3.5-0.8b` に解決する。**
  #475 が開いたままにしていた exit condition が閉じた
* **bundled は 21 → 20 ファミリ。** `docs/reference/models.md` は**変わらない** —
  offered 集合から生成されており、0.5b は #475 以降そこに居なかった
* **ホスト影響ゼロ。** #476 の実測どおり（RAM 2–24 GB のどこでも自動選定されず、
  フロア下フォールバックの選にも入らず、`waired/*` エイリアスが 1 つも指していない）。
  カタログは `//go:embed` なので、エージェント側ではマップと縮んだカタログが常に
  同じバイナリで出荷される。エージェント側にバージョンずれは存在しない
* **2 つの変更が溶接された。** テーブル項目を消しても、マニフェストだけ消しても、
  同じテスト群が同じように赤くなる（`TestRetiredNamesAreGoneFromTheCatalog` が
  両方向を守る）
* **`--require-pass` は offered-only のまま。** #200 が「削除には後継マップが要る、
  だから完全集合をゲートすれば誰にも消せない赤になる」という旧根拠を偽にしたが、
  ゲートを広げれば恒久的な CI fixture まで一度も課されたことのない基準に晒される。
  別issue
* **やらなかった: 退役モデルを現に動かしていて重みがディスクにあるホストの移行。**
  `state.json` の書き換え・重みの退避・`Active` の付け替えをしない。そのホストの
  `localBestTier` → 0、`BestTier` → 0、`ContextWindowFor` → 0（fail-open）。
  0.5b では実測ゼロ台。**14b では blocking**（16GB NVIDIA dGPU が現に引く）
* **14b は今回やらない。** #200 は「strictly superseded、今すぐ退役可能」と書くが、
  後継候補 gpt-oss-20b は `min_ram_gb` こそ同じ 16 でも重みが 9.0 → 14.0 GB。
  収まらなければ 7b に落ちて tier 55 → 45 の劣化になる。着手前に測る
* `granite4-350m` が唯一の withheld エントリになった。その `internal_only` は
  0.5b を「暫定的な対比相手」として引いていたので書き換えた
* `internal/e2e/agentgrade` のコメントが #484 で撤回済みの主張
  （「granite は構造化ツール呼び出しを出す、だから 0.5b より選ばれた」）を
  そのまま持っていたので直した。同じ主張の 2 度目の引用は、#484 が張った pin の
  射程外だった

## Refs
- https://github.com/waired-ai/waired-agent/issues/200
- https://github.com/waired-ai/waired-agent/issues/322
- https://github.com/waired-ai/waired-agent/issues/475
- docs/decisions/20260803/1909-withhold-a-model-that-cannot-call-a-tool.md
- docs/decisions/20260801/1900-ci-fixture-model-withheld.md
- docs/decisions/20260804/1751-withheld-models-rank-in-the-same-table.md
- proto/signer/integration.go（退役 enum の前例）
