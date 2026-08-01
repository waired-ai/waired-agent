---
status: accepted
---

# トレイのルーティングをトップレベルへ分離し、peer-only は fail-closed とする (20260801 18:40)

## Status
Accepted

## Context

rc7 レビュー（waired-ai/waired#986、waired-agent#327）で、トレイの "Inference"
サブメニューが 1 階層に詰め込まれすぎている、という指摘が出た。同じ階層に
エンジン制御（Disable / Stop / Stop sharing）、状態表示（Engine: ready、
Sharing: enabled、Mesh: …、Model: …、Worker: …）、そして "Inference worker" の
ラジオ群（Auto / Local only / Peer preferred）と**具体的なノード pin** が並ぶ。

レビュー時に確定した指摘は 3 点。

1. 抽象モード（自動選択）と具体ノード pin が同列で、どちらを選んでいるのか
   分からない。
2. `Local only` があるのに、その鏡像である「ほかのマシンだけ」が無い。
3. 「このマシンのエンジン設定」と「自分のリクエストがどこで動くか」は別の
   関心事で、トップレベルで分けるべき。

3 の実装で「pin を 1 段深いサブメニューに落とす」案は**採れない**ことが調査で
判明した。`fyne.io/systray` v1.12.2 の Windows バックエンドは 3 階層目を描画
しない（waired#809 で worker 行を "Inference" 直下へ平坦化したのと同じ制約。
`internal/gui/tray/tray.go` のメニュー構築部にコメントとして残っている）。
サブメニュー内にセパレータも置けない（`systray.AddSeparator()` はトップレベル
専用）。

## Decision

1. **分割はトップレベルで行う。** "Inference"（このマシンのエンジン）と
   "Inference routing"（どのマシンが答えるか）を並列のトップレベル項目に
   分ける。`Worker: …` と `Mesh: …` の状態行は routing 側へ移す。
2. **サブメニュー内は無効な見出し行で区切る。** "Choose automatically"（自動
   選択の 4 モード）と "Pin to one peer"（具体ノード）。Claude Code サブメニュー
   の Main conversation / Subagents と同じ既存パターンで、3 階層制約の下で
   取れる唯一の分離手段。pin 行が 0 本のときは見出しも出さない。
3. **`peer-only` を追加し、fail-closed とする。** mesh 上のどのピアも応答
   できないとき、ローカルエンジンが動いていても**ローカルへは落とさず**
   `ErrModelNotReady` を返す。落としてしまえば「このマシンでは動かしたくない」
   という操作者の指定が黙って無効化され、それは waired-agent#325 が pin から
   取り除こうとしている「黙ってローカルへ戻る」欠陥そのものになる。
   overlay 側 Selector（`MeshSnapshotFn == nil`）でも同様に失敗させる。
4. **表示文言は現行語彙を維持**（Auto / Local only / Peer preferred + 新
   Peer only）。ワイヤ値と `waired worker set --mode=` の値が一致するため、
   CLI とトレイを突き合わせられる。

## Consequences

- `peer-only` は agent ローカルの routing mode であり proto に触れない。
  CP との契約変更も proto タグも不要。
- Claude 経路は追加実装なしで fail-closed になる。`selectWithWorkerPref` の
  ローカル再試行は `Mode == pinned` のときだけ走るため、`peer-only` は最初の
  選択エラーがそのまま呼び出し元へ返る。この不変条件は
  `cmd/waired-agent/claude_selector_test.go` で固定した。
- モード行はトレイ側で事前確保したスロットに位置で流し込まれるので、行数は
  `workerModeSlots` と一致していなければならない。5 本目を足すと**クリック
  できない行**が黙って生まれるため、`TestApplyWorkerModeSlotsMatchPreallocation`
  で件数を固定した。
- トップレベル項目が 1 つ増える。トレイのトップレベルは常時表示ではなく、
  `ShowRoutingMenu`（worker 行か mesh 行のどちらかに内容がある）で開閉する
  ため、両エンドポイントを持たない古い daemon では従来どおり出ない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/327
- https://github.com/waired-ai/waired-agent/issues/326
- https://github.com/waired-ai/waired-agent/issues/325
- docs/decisions/20260801/1030-engine-stop-commit-to-kill.md
