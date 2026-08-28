# メニューラベルはレンダラごとに書き分けられる (20260828 21:45)

## Issue

`0510-menu-labels-are-markup-on-two-of-three-os.md` は「`_` は GNOME と
Plasma の両方で正しく出る文字列が存在しないので直せない」で止まっていた
(agent#1100)。**前提が 1 つ足りなかった** — 両方に正しい 1 つの文字列は無いが、
**レンダラごとに正しい文字列は両方存在し、どちらが描いているかは実行時に分かる。**

## Learnings

### 2 つのエスケープは互いに補完的

実機(sv-mag = Ubuntu 26.04 / GNOME Shell 50.1 / ubuntu-appindicators)で、
インストール済み拡張の正規表現を gjs に、仕様側は GTK の `use_underline` を
python3-gi に流して採った:

| 出したい文字列 | 今(GNOME) | 今(仕様) | `_`→`__`(GNOME) | `_`→`__`(仕様) | GNOME 用(GNOME) |
|---|---|---|---|---|---|
| `…-q4_K_M` | `q4K_M` | `q4KM` | `q4_K__M` | OK | OK |
| `ANTHROPIC_BASE_URL` | `ANTHROPICBASE_URL` | `ANTHROPICBASEURL` | `…BASE__URL` | OK | OK |
| `first_last@corp.com` | `firstlast@…` | `firstlast@…` | OK | OK | OK |
| `abc_`(末尾) | OK | `abc` | `abc__` ← 悪化 | OK | OK |
| `a__b` | `a_b` | `a_b` | `a___b` | OK | OK |

- **仕様側** = `strings.ReplaceAll(s, "_", "__")`。
- **GNOME 用** = 「最初の下線ランに `_` を 1 個足す。**そのランが文字列末尾なら
  何もしない**」。拡張の正規表現は末尾ランにマッチしないので、`abc_` は
  今でも正しく描かれている — そこを倒すと直すのでなく壊す。

### 仕様側は Plasma 機が無くても実レンダラで測れる

GTK の `use_underline` は `libdbusmenu-gtk/genericmenuitem.c` と Plasma の
`swapMnemonicChar` が実装しているのと同じ規則。追加インストール不要:

```python
import gi; gi.require_version("Gtk","3.0")
from gi.repository import Gtk
l = Gtk.Label(); l.set_use_underline(True); l.set_label(escaped)
l.get_text()   # 画面に出る文字列(マークアップ除去後)
```

### 描いている相手は D-Bus で名指しできる。環境変数では無理

```sh
busctl --user call org.freedesktop.DBus /org/freedesktop/DBus \
  org.freedesktop.DBus GetConnectionUnixProcessID s org.kde.StatusNotifierWatcher
# → pid → /proc/<pid>/comm → gnome-shell
```

**`XDG_CURRENT_DESKTOP` は使えない。** autostart から起動した実機の
`waired-tray` の `/proc/<pid>/environ` には `XDG_RUNTIME_DIR` しか無かった。
godbus は `DBUS_SESSION_BUS_ADDRESS` が無くても
`tryDiscoverDbusSessionBusAddress()` で `$XDG_RUNTIME_DIR/bus` を拾うので、
セッションバス自体には繋がる。

### GNOME 側の癖は「版のずれ」ではなく「GNOME の挙動」

`dbusMenu.js` の

```js
propertyGet('label').replace(/_([^_])/, '$1')
```

は **2013 年の初回コミットから一度も変わっていない**(2013-03-08 の
「fixed handling of `_` keyboard shortcuts」も同じ正規表現を更新経路に複製した
だけ)。v33 / v42 / v57 / v61 / v65 で同一。Debian sid `64-2` も
Ubuntu 26.04 `61-2` も**パッチ無し**で同じ行を配る。
`ubuntu-appindicators@ubuntu.com` は `metadata.json` の 3 キーだけを書き換えた
**バイト同一の上流コード**。上流に該当 issue は 1 件も無い。

### エスケープはアプリではなくツールキットの仕事だった

- Chromium `ConvertAmpersandsTo` が `_`→`__`。**全 Electron トレイ**(Signal /
  Slack / VS Code / Discord)はこれで正しく出ている。
- `libdbusmenu-gtk/parser.c` は `use_underline == FALSE` のラベルを書き手側で
  二重化する。KF6 の `dbusmenuexporter.cpp` も同じ。
- `getlantern/systray`(cgo + libappindicator)はその恩恵を**自動で**受けていた。
  `fyne.io/systray` が純 Go の D-Bus 実装に置き換えたときに落ちた。
- 逆に Qt の `QSystemTrayIcon`(`qdbusmenutypes.cpp` の `convertMnemonic`)は
  `&`→`_` を 1 個置換するだけで literal `_` を守らない → Nextcloud /
  KeePassXC / Telegram は今の我々と同じ壊れ方をしている。

### プロトコルに逃げ道は無い

dbusmenu のプロパティは `type / label / enabled / visible / icon-name /
icon-data / shortcut / toggle-type / toggle-state / children-display /
disposition` だけ。ニーモニック解釈を切るフラグは無く、`x-*` ベンダ拡張を
読むレンダラは Plasma の `x-kde-title` 1 つのみ。`__` が唯一の逃げ道。

なお systray v1.12.2 が dbusmenu に書く**呼び出し側の文字列は `label` 只一つ**
(`applyItemToLayout`)。ほかは定数・bool・int・bytes で、**メニュー項目の
ツールチップは Linux では送られてすらいない**。

### 運用上の副作用 — 検証スクリプトが外れる

この変更以降、Linux で `GetLayout` が返す `label` は**マークアップを含む**。
`docs/knowledges/.../linux-tray-via-dbusmenu` 系の手順で `q4_K_M` を grep して
いると当たらない(`q4__K_M` になる)。逆に `WAIRED_TRAY_DEBUG=1` の JSON ダンプは
`MenuModel` を読むので**素のまま**で、そちらが照合用の正本になる。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1100
- docs/decisions/20260828/2140-a-menu-label-is-written-for-its-renderer.md
- docs/knowledges/20260828/0510-menu-labels-are-markup-on-two-of-three-os.md
- https://github.com/ubuntu/gnome-shell-extension-appindicator/blob/master/dbusMenu.js
- https://invent.kde.org/plasma/plasma-workspace/-/blob/master/libdbusmenuqt/utils.cpp
- https://source.chromium.org/chromium/chromium/src/+/main:ui/base/accelerators/menu_label_accelerator_util_linux.cc
- https://github.com/fyne-io/systray/blob/master/systray_menu_unix.go
