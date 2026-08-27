---
status: accepted
---

# トレイのグレーは「いまはできない」だけを意味する (20260828 03:20)

## Status
Accepted

## Context

waired-agent#1032 でトレイのトップに状態 3 行を入れた
（`● Engine: ready — Qwen3 8B Instruct` / `● Peers: 2 of 4 serving` /
`● Claude Code: routed through Waired`）。すべて `Disable()` した表示専用行
として作った。

オーナーの報告（2026-08-28）:

> status 情報の表示だけの行はグレーアウトしていますよね。直感的にみて、
> グレーアウトしている行はその行にある情報が inactive や failed になって
> いるように見えます。選択不可というよりは。

これは誤読ではない。グレーには既に意味があり、それは 1 つだけである:

- Windows UX Guide (`cmd-menus`): 「Refer to unavailable menu items as
  **unavailable**, not as dimmed, disabled, or grayed」
- GNOME HIG (`patterns/controls/menus`): 「Make a menu item insensitive
  **when its command is unavailable**」
- Nielsen Norman Group: グレーは初代 Macintosh 以来「原理的には在るが
  今はダメ」を意味する慣習である
- **Tailscale 自身の systray 実装**（`tailscale/client/systray`、こちらと
  同じ `fyne.io/systray` を使う）が `Disable()` を呼ぶのは 4 箇所だけで、
  すべて本当に使えないもの（未接続時の This Device / オフラインの exit
  node / read-only モード / バックエンド停止時）。一方 **This Device 行は
  有効行**でクリックすると IP をコピーする。グレーなのは
  `Tailnet Exit Nodes` のようなセクション見出しだけ

つまり「ready」と書いてある行をグレーにするのは、面が 2 つの矛盾する
ことを同時に言っている状態だった。#1032 が直したのと同じ種類の欠陥である。

## Decision

**1. `Disable()` は「いまはできない」専用にする。**

グレーのまま正しいのは 2 種類だけ:

- **セクション見出し** — グループの名前であって状態ではない
  （`Main conversation` / `Recent activity` / `Pin to one peer` など）
- **本当に実行できない操作** — ラベルと状態が一致している
  （`Model not loaded`、到達不能なピアの pin 行、起動できないエンジンの
  トグル）

`onReady` の各 `Disable()` はどちらであるかを `// grey: <理由>` で名乗り、
`scripts/ci/tray-grey-row-guard.sh` が理由の無い `Disable()` で lint を
落とす。この軸はテストからは見えない（enabled 状態は `onReady` で書かれ、
実 systray セッションが要る）ので、ガードが唯一の自動検出である。

**2. 状態を伝える行にはクリックで開く行き先を与える。**

第 3 の状態は無い。3 OS のバックエンドを読んだ結果:

| | 有効な行をクリック | 無効な行をクリック |
|---|---|---|
| Windows | `TrackPopupMenu` が `TPM_RETURNCMD` 無しで呼ばれ、選択時点でポップアップを閉じてから `WM_COMMAND` を送る。Win32 に「閉じない」フラグは無い | `MFS_DISABLED` で OS がイベントを出さない。メニューは開いたまま |
| macOS | AppKit が `NSMenuItem` のアクション発火前にメニューを閉じる。閉じないのは `NSMenuItem.view` だけで、systray は公開していない | `autoenablesItems=FALSE` + `enabled=FALSE`。イベント無し・開いたまま |
| Linux | shell がメニューを閉じてから `com.canonical.dbusmenu.Event` を送る。dbusmenu 仕様に「閉じない」プロパティは無い | shell が `enabled` を尊重してイベントを送らない |

**「押しても何も起きず、メニューも閉じない」は灰色行そのもの**であり、
灰色を保ったまま「壊れて見えない」ようにする方法は存在しない。したがって
灰色を外す行には行き先が要る。行き先は状態レポートのダイアログで、
`Status…` の行がその存在を告知する。

**3. レポートはページではなくダイアログにする。**

レポートが要る読み取り（`/identity` / `/inference/mesh` /
`/observability/state` / `/integration/claude`）は socket 専用である。
`internal/management/socket.go` は TCP で読める 5 経路について
「**they are not a judgement about which reads are harmless** — they are
the routes non-Go consumers actually call, which cannot reach a unix
socket」と書いており、`internal/platform/paths/paths.go` は socket を
選んだ理由を「**Browsers and network peers cannot open a unix socket /
named pipe, which is the point (waired#838)**」と書いている。daemon に
HTML を配らせる案はこの 2 つの決定が狭めた面をそのまま広げ直すことになる。

トレイは既に全事実を `Snapshot` に持っているので、自分で組み立てて出す。
新しいネットワーク面はゼロ。

## Consequences

- ダイアログは **Windows/macOS とも等幅でもスクロールでもない**ので、
  表を組めない。見出し＋字下げした `ラベル 値` の並びに限り、ピアは 10 台で
  切って残りは `+N more — on the clipboard` と言う。長さの上限はテストで
  固定する（`status_report_test.go`）。
- 切ったぶんは **Copy details** のクリップボード側に全部入る。こちらは
  上限が無いので、識別子・アドレス・direct/relay と RTT・engine type・
  map age といった、問い合わせで次に訊かれるものもここに入れる。
- 状態行のクリックは**メニューを閉じる**。これは回避できないので、
  ドキュメントで「クリックすると Status が開く」と明示する。
- 公開共有のピアはこの面でも擬名で出る。レポートは
  `inferencemesh.PeerDisplayName` / `PeerDisplayID` を通すので §8.5 の
  規則が自動的に効き、テストが DeviceID / DeviceName / OverlayIP の
  いずれも出ないことを主張する。
- **範囲外**: メニュー先頭の見出し行（`● Connected`）とアカウント行は
  オーナーの選択でこの回の対象外とし、グレーのまま残した。同じ議論は
  この 2 行にも当てはまるので、必要になったら同じ形で直せる。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1090
- https://github.com/waired-ai/waired-agent/pull/1068 (#1032 — この行を入れた変更)
- `docs/knowledges/20260828/0130-systray-submenu-parent-keeps-its-children.md`
