---
status: accepted
---

# メッシュ共有の一時停止は非永続な軸にする (20260801 10:35)

## Status
Accepted

## Context

waired-agent#316 で「トレイを Quit したらエンジンを止め、メッシュ共有も止める」
という方針が決まった。素直に実装するなら既存の
`POST /waired/v1/inference/share/disable` を Quit から叩けばよい。

しかしこのエンドポイントは `desired-share` ファイルに `not_shared` を**永続化**
する。つまり「今日はもう使わないのでアイコンを閉じた」だけで、そのユーザーの
共有設定が恒久的に revoke され、手で戻さない限り翌日も共有されない。
Quit は操作であって、ポリシーの変更ではない。

エンジン電源軸（#186）は既にこの区別を持っている: hard stop は
`OllamaAdapter.parked` というメモリ上のラッチだけで、デーモンを再起動すれば
設定由来の起動状態に戻る。「操作は非永続、ポリシーは永続」という原則が
すでに存在していた。

なお、エンジンを park するとプローブが `Reachable=false` を CP に push するため、
heartbeat 1 周期（15s）以内に peer は本ノードを避ける。つまり共有フラグを
触らなくても実質的な引き上げは起きる。それでも明示的に共有を止める価値はある:
広告そのものを止めれば、peer は 503 を踏まずに済む。

## Decision

`shareController` に**非永続の `suspended` ラッチ**を足し、
`POST /waired/v1/inference/share/{suspend,unsuspend}` として公開する。

- `IsShared()` は `shared && !suspended`。既存の消費者（CP への push スキップ、
  peer overlay の 503 ゲート）はそのまま乗る。
- 何もディスクに書かない。デーモン再起動で自然に消える。
- 明示的な `Share` / `Unshare` は suspend を必ずクリアする。ラッチが詰まって
  ユーザーの操作を飲み込む状態を作らない。
- トレイは Quit で suspend し、次回起動で unsuspend する。「suspend =
  ユーザーのセッションが不在」という意味づけが閉じる。
- `unsuspend` は共有を ON にはしない。ラッチを外すだけなので、ユーザーが
  明示的に共有 OFF にしていたなら OFF のままである。
- ステータスは `share_with_mesh`（永続の選択）と `share_suspended`（今の実効）を
  **別フィールド**で返す。`share_with_mesh` に第 3 の値を足すと、古いトレイが
  どちらの case にも当たらず行ごと消えて劣化する。

Quit から Public share（waired#833）には触れない。あちらは無効化に確認ダイアログを
要求する設計で、CP への同期 push に最大 10s かかる。Quit から黙って叩くのは
その設計に反する。

## Consequences

- Quit → 起動 の往復でユーザーの共有設定は保存される。「閉じただけで共有が
  切れた」というクラスの問い合わせが構造的に発生しない。
- トレイが起動していないマシン（サーバ、ヘッドレス）は suspend されない。
  トレイを一度も起動しないなら常に共有されたまま、というのが正しい。
- suspend 状態は通常ユーザーの目に触れない（起動時に解除されるため）。解除に
  失敗したときのみ「Sharing: paused」として描画され、クリックで解除できる。
- 管理 API に 2 verb 増える。`waired inference share status` の表示や CLI verb は
  今回追加していない（トレイのセッション機構であり、CLI から叩く動機がない）。

## Refs
- https://github.com/waired-ai/waired-agent/issues/316
- docs/decisions/20260801/1030-engine-stop-commit-to-kill.md
