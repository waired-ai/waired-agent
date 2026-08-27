# systray のサブメニュー親は、子を作り終えるまでメニューに残す (20260828 01:30)

## Issue

Windows の tray が、起動のたびに**サブメニューの行をランダムに落としていた**。
pc-dell-premium(0.0.3-rc4、`90dd4a5`)で tray を 3 回連続で再起動し、他は何も
変えずに `This device` を採ると、8 行 / 5 行 / 4 行と毎回違った。トップレベルは
3 回とも 17 行でバイト一致。欠けたのはこの機自身の名前と overlay IP、
`Peers (4)` の見出し、ピア行 1 本。

projection は正しかった。同じプロセスの `WAIRED_TRAY_DEBUG` ダンプは
`"DeviceName": "pc-dell-premium"`, `"OverlayIP": "100.95.113.3"` を出していた。

## Learnings

### 1. 親を `Hide()` してから子を足すと、子は入らない

`onReady` は各サブメニュー親をこの順で作っていた:

```go
t.miDeviceLabel = systray.AddMenuItem("This device", "…")
t.miDeviceLabel.Hide()
t.miDeviceName = t.miDeviceLabel.AddSubMenuItem("", "")
```

`fyne.io/systray` v1.12.2 の Windows backend では、**最初の
`AddSubMenuItem` がサブメニューを作る**。`addOrUpdateMenuItem` は
`t.menus[parent]` が無いのを見て `convertToSubMenu(parent)` を呼び、その中身は

```go
mi := menuItemInfo{Mask: MIIM_SUBMENU, SubMenu: menu}
res, _, err = pSetMenuItemInfo.Call(uintptr(t.menuOf[menuItemId]), uintptr(menuItemId), 0, …)
if res == 0 { return 0, err }
```

`Hide()` は `RemoveMenu` なので、親は `t.menuOf[parent]` から**既に外れている**。
`SetMenuItemInfo` は失敗し、`convertToSubMenu` はエラーを返し、
`addOrUpdateMenuItem` がそれを伝播して **子は挿入されない**。しかも
`t.menus[parent]` は未設定のままなので、**次の子も同じ経路で失敗する**。

### 2. だから `Show()` の順序が「入るかどうか」を決めてしまう

サブメニューは、どれかの `Show()` が**親の `Show()` より後に**来たときに初めて
できる。その順序を配るのは `endRowPass`(`internal/gui/tray/rows.go`)で、
中身は `t.rowStates` の **map 走査 = Go のランダム順**。親より先に回ってきた子が
落ちる。しかも `setVisible` は既に `rowState.visible = true` を記録しているので、
以降の pass は差分なしの no-op になり、**プロセスが生きている間ずっと戻らない**。

`endRowPass` の doc は「Map iteration order is unspecified and does not matter:
the position a backend inserts a row at comes from the item, not from the order
the Show() calls arrive in」と書いていた。**position については正しく、
insertion については誤り**だった。

### 3. 直し方は「親に子を持たせたまま作る」

親の `Hide()` を手で書かない。`paintCreationBaseline` が `force=true` で
ゼロ `MenuModel` から全行の可視性を主張し直すので、親も
そこで隠れる — 手書きの `Hide()` を約 40 個やめて model から導出する、という
のが `paintCreationBaseline` を入れた動機そのもの(waired#808)。
`t.menus[parent]` は一度できたら **削除されない**ので、以後どの子の `Show()` も
素の `InsertMenuItem` になり、`endRowPass` は本当に順序非依存に戻る。

守るために `scripts/ci/tray-submenu-parent-guard.sh`(+ self-test)を置いた。
`t.miFoo.AddSubMenuItem(` が在る `miFoo` について、`t.miFoo.Hide()` が
最初の子より前に来ていたら落とす。Linux(dbusmenu が本物の木を持つ)と
macOS(`-[NSMenu setSubmenu:]`)では起きないので、**Linux のテストレグでは
永久に緑**である。

### 4. 実機で採るときの落とし穴 2 つ

- `#32768`(Win32 ポップアップ)は**閉じても生き続ける**。開く前後の差分で
  「新しく出たウィンドウ」を探すと、2 回目以降は何も見つからない。
  `GetWindowThreadProcessId` で **tray の pid のもの**に絞り、
  `IsWindowVisible` で足切りする。トップレベルは「`Quit` を含むほう」で判別できる。
- PowerShell 5.1 は BOM 無しの `.ps1` を ANSI(cp932)で読む。スクリプト中の
  日本語リテラルで比較すると黙って一致しなくなるので、**照合は ASCII で書く**
  (`AutomationId` が `SystemTrayIcon` の最初の `SystemTray.NormalButton` が
  オーバーフローの山形、など)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1063
- https://github.com/waired-ai/waired-agent/issues/1032
- docs/knowledges/20260822/1906-tray-row-ab-capture-on-real-hardware.md(MSAA の採り方)
- 同じ v1.12.2 の別の癖: `systray.Quit` は `quitOnce` で一回限りで、backend が
  立つ前に呼ぶとアプリが二度と終了できなくなる(waired-agent#1045 / PR#1062)
