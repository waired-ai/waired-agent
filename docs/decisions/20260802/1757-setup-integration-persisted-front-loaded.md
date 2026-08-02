---
status: accepted
---

# コーディングツール行を永続化し、モデル DL の前へ移す (20260802 17:57)

## Status
Accepted

## Context

rc7 の内部レビュー(waired-ai/waired#986、F18/F21)で Windows 実機に 2 つの症状が出た。

1. セットアップ完了済みのデバイスを再起動して再ログインすると、ウィザードの
   「Connect your coding tools」行だけが ERR「needs administrator access to
   continue」に戻る。エンジンもモデルも OK なのに、そのデバイスは
   `setup_complete` を二度と満たさない。
2. coding tools ステップがモデル DL の**後ろ**にいる。DL は数十 GB・数時間に
   なりうるので、「唯一まだ人の操作と昇格が要るステップ」が「最も長い無人待ち」
   の後ろに置かれていた。席を外して戻ると、そこで止まっている。

1 の原因は、この行だけが observable な真実の源を持たないこと。engine 行と
model 行は毎スナップショットでディスクとエンジンを実測して再導出するので
再起動しても同じ答えになるが、integration 行は in-memory の executor リース
状態(`setupReconciler.executorSteps` / `executorEverSeen`)からの投影しかなく、
daemon 再起動で「一度も attach されていない」arm に落ちていた。

その arm が返すコードが `permission_denied` だったのが誤読の直接原因。しかも
この行には**同じコードの別の producer** がいる — executor が実際に走って
"permission denied" を含むエラーを報告したとき、`classifyIntegrationFailure`
が同じコードを返す。1 コード 2 意味で、`error_detail` は「parse しない」と
契約されているので、ウィザード側に切り分ける手段がなかった。

## Decision

**1. 結果を daemon の state dir に永続化する。**
`<state-dir>/runtime/setup-integrations` に、executor が `done` を報告した
時点の指示(targets)を記録する。`integrationStep` の default arm は、
どの liveness arm よりも先にこの記録を見る。

- 書くのは daemon だが、**ファイルを作るのは相変わらず executor だけ**。
  daemon がユーザ home や root 所有の managed settings に触れないという
  制約は変わっていない。「他人がやった事実を記録する」のは daemon に無い
  特権ではない。
- 判定は**包含(superset)**。指示が縮んだ場合(トグルを外して再実行)、
  今求められているものは既に書かれているので done は正直。外す作業は
  `waired unlink` の仕事で、この行の仕事ではない。
- **失敗は記録しない**。赤を永続化すると `waired link` で直したホストでも
  赤が残る。失敗の復旧は「コマンドを再実行」で、それ自体が記録を書く。
- 「聞いた上で全部オフ」も記録しない。その行は指示そのものから `skipped`
  として毎ブート再導出されるので、二つ目の真実の源になってしまう。

**2. 新しいワイヤコード `setup_command_not_run` を足す**(proto は別 PR)。
`permission_denied` を言い換えるのではなくコードを分けたのは、上記のとおり
このコードに本物の producer がもう 1 つあるため。3 つの「書き手がいない」は
3 つの別の質問に答える — 一度も走っていない / 走って途中で消えた / 走って
拒否された。最初の 2 つは同じコマンドに送るが、「進捗は保存されています」と
言えるのは片方だけで、権限の話なのは 3 つ目だけ。

**3. ステップ順を engine → integration → model へ。**
ワイヤの配列順が NAVI の描画順なので、この投影が順序の在り処。CLI 側も
`runSetupIntegrations` を engine install 直後へ移した。

- ターミナル自身の質問(`runPostLoginIntegration`)は**動かさない**。
  #186 が「モデル DL を中断して人に聞くな」で今の位置に置いたもので、
  その理屈は人が答えるプロンプトについてのもの。ウィザードの指示は誰も
  待たせない。
- 呼び出しは 2 箇所になる。早い方が通常経路で、遅い方(従来位置)は
  ①DL 中に commit したブラウザ(#308)②engine/model を書いた一拍あとに
  coding tools の答えを書いたウィザード、の取りこぼし用。
  `runWizardIntegrations` の戻り値(「指示があったか」)が二重実行を防ぐ。
- keep-open 行は、integration が済んだ時点で `setupTerminalDoneLine` に
  差し替える。executor の仕事が終わっているのに「開けたままに」と言うのは
  waired#939 が禁じた「もう当てはまらない指示の繰り返し」。ここで閉じても
  安全なのは 1 の永続化が入っているから — この 2 つを同じ PR で出す理由。

## Consequences

- 完了済みデバイスがサービス再起動で「未完了」に戻らなくなる。`setup_complete`
  もそれに従う。
- ウィザードの coding tools 行が engine 直後に緑になり、長い DL は本当に
  無人の尻尾になる。エンジンのインストールに失敗したホストでも coding tools
  は接続される(home のファイル書き込みにエンジンは要らない)。
- 記録ファイルが 1 つ増える。`<state-dir>/runtime/setup-integrations`。
  消せば「一度も走っていない」に戻る — 診断としてはそれで正しい。
- コード追加は CP 側が先に受け付けられる必要がある(CP のバリデータは
  `signer.IsValidSetupErrorCode` を通す)。proto → CP → agent の順で入れた。
- `TestSetupIntegrationTerminatesWhenNobodyCanWriteIt/no executor ever attached`
  の期待値を反転した(`permission_denied` → `setup_command_not_run`)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/312
- https://github.com/waired-ai/waired-agent/issues/311
- https://github.com/waired-ai/waired-agent/pull/450 (proto)
- waired-ai/waired#1002 (レーン L10), waired-ai/waired#986 (rc7 レビュー F18/F21)
