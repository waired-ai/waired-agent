# トレイの Public Share サブメニュー — systray のネスト制限と onReady 事前確保 (20260720 17:55)

## Issue

waired#833 の Public Share 系 UI(共有トグル・同意ダイアログ導線・状態行)を
トレイに載せるにあたり、`fyne.io/systray` の 2 つの構造的制約に当たった。既存の
Inference / This device / Settings 各サブメニューがいずれも「親 1 段 + 子を平坦に
並べる」形になっている理由がこれで説明できる。

## Learnings

- **サブメニューのネストは実質 2 段まで**。`AddMenuItem` の下に
  `AddSubMenuItem` で子を作れるが、その子の下に更に
  `AddSubMenuItem` した孫は Windows バックエンドで描画されない(macOS/Linux
  では出るため見落としやすい)。このため Public Share の各行は、独立した
  第 3 階層を作らず、トップレベルの親(または既存の 2 段目)へ平坦に並べる。
  worker ルーティング行や peer ハードウェア行が「サブメニューではなく
  disabled のセクションラベル + 兄弟行」になっているのと同じ理由。
- **メニュー項目は `onReady` で全部事前確保する必要がある**。systray は
  項目の線形リストを一度だけ構築する前提で、`onReady` 実行後に既存項目の
  間へ新規項目を挿入したり、サブメニューを後から生やしたりできない。動的に
  出したり隠したりする項目も、あり得る最大数を最初に確保しておき、
  実行時は `Show`/`Hide` + `SetTitle` を切り替えるだけにする。よって
  Public Share の行も、可視条件が満たされる前提でトップレベルに親を
  事前確保しておき、非該当のデーモン(エンドポイント未提供)では
  `Hide` で畳む。区切り線は隣接グループが隠れると各 OS が自動で
  畳むので、余分な空行にはならない。
- 帰結として「新しい機能グループ = 新しいトップレベル親 + その直下に
  平坦な子行」というのが本トレイの定石になっている。第 3 階層が欲しく
  なったら、それは平坦化して 2 段目に落とすかトップレベルへ昇格させる。

## Refs

- internal/gui/tray/tray.go (`onReady` の事前確保、Inference/This device/Settings の平坦化コメント)
- https://github.com/waired-ai/waired-agent/pull/118
