---
status: accepted
---

# メニューラベルは「仕様」ではなく「描く相手」に合わせて書く (20260828 21:40)

## Status
Accepted

## Context

dbusmenu の `label` はテキストではなくマークアップで、仕様は `_` に Win32 の
`&` と同じ役割を与えている:

> two consecutive underscore characters `__` are displayed as a single
> underscore, any remaining underscore characters are not displayed at all,
> the first of those remaining underscore characters (unless it is the last
> character in the string) indicates that the following character is the
> access key.

`fyne.io/systray` はラベルを素通しするので、下線を含む行は 1 文字消えて描かれる。
該当する行は多い — Recent activity 行はルータのワイヤタグ(`engine_not_ready`)、
デバイス ID は `dev_28ab996e` 形、`active_model` を持たない旧エージェントのピア行は
エンジン生タグ `qwen3.6:35b-a3b-q4_K_M`、Claude 行は `ANTHROPIC_BASE_URL`、
アカウント行はメールのローカル部、OpenCode の Config 行はホームディレクトリ。
**下線 2 個以上は例外ではなく主流。**

agent#1096 で Windows の `&` を直したときにこの半分は直せなかった。仕様どおり
`_`→`__` と書くと、GNOME で**別の形に壊れる**からである
(agent#1100)。GNOME の唯一の SNI ホストである
gnome-shell-extension-appindicator の `dbusMenu.js` は

```js
propertyGet('label').replace(/_([^_])/, '$1')
```

で、`g` フラグが無い。**最初の「`_` + 非 `_`」を 1 個消すだけで、`__` を畳まない。**
つまり両方のレンダラが正しく描く 1 つの文字列は存在しない。

## Decision

**両方に正しい 1 つの文字列は無いが、レンダラごとに正しい文字列は両方存在し、
どちらが描いているかは実行時に分かる。** だからラベルは仕様にではなく
**描く相手**に合わせて書く。

1. 既定は **dbusmenu 仕様どおり**(`_`→`__`)。Plasma
   (`plasma-workspace/libdbusmenuqt` が `swapMnemonicChar` を in-tree fork)、
   `libdbusmenu-gtk`(したがって xfce4-panel / Waybar / snixembed)がこれを実装する。
2. **実測で仕様を誤読すると分かっているレンダラにだけ**、そのレンダラ用に書く。
   今日その集合は gnome-shell 1 つ。エスケープは
   「最初の下線ランに `_` を 1 個足す。ただしそのランが文字列末尾なら何もしない」。
3. 判定は **SNI ホストの正体**。`org.kde.StatusNotifierWatcher` の所有者を
   `GetConnectionUnixProcessID` で引き、`/proc/<pid>/comm` を読む
   (`internal/platform/trayhost.MenuLabels`)。**環境変数は使わない** —
   autostart から起動した実機の `waired-tray` の環境には `XDG_RUNTIME_DIR`
   しか無く、`XDG_CURRENT_DESKTOP` は空だった。既存の `detectDesktop()` は
   ここでは「other」と答える。
4. **不明・失敗は必ず仕様側**。仕様準拠のレンダラは仕様準拠のマークアップを
   正しく描くので、既定側に倒して損をしない。逆向きの誤りだけが画面に
   下線を増やす。
5. エスケープの位置は変えない。`internal/gui/tray/rows.go` の `setTitle`、
   ラベルが Go を出る最後の点。状態レポート・クリップボード・デバッグダンプは
   同じ `MenuModel` を読むが、メニューではない。

## Consequences

- Linux の実機検証で `GetLayout` が返す `label` は**マークアップを含む**。
  `q4_K_M` で grep する検証スクリプトは外れる。
- gnome-shell 用のエスケープは第三者の実装の癖に合わせたもので、上流が
  `g` フラグを足せば我々が古くなる。ただしその行は**2013 年の初回コミットから
  一度も変わっておらず**、v33〜v65 で同一、Debian/Ubuntu もパッチ無しで配る。
  `ubuntu-appindicators@ubuntu.com` は `metadata.json` の 3 キーだけを
  書き換えたバイト同一の上流コード。追随が必要になったら
  `escapeMenuLabel` の 1 分岐と表テストの 1 列で済む。
- Plasma の実描画は確認していない(艦隊に Plasma 機が無い)。仕様側は
  GTK の `use_underline` を実レンダラとして回して確認した — `libdbusmenu-gtk`
  と Plasma の `swapMnemonicChar` が実装しているのと同じ規則。目視は
  waired#179 / #189 に残る。
- これは新しい仕事ではなく、`fyne.io/systray` が落とした仕事である。Chromium は
  `ConvertAmpersandsTo` で全 Electron トレイの下線を二重化し、
  `libdbusmenu-gtk/parser.c` は書き手側で二重化し、`getlantern/systray`
  (cgo + libappindicator)はその恩恵を自動で受けていた。純 Go 書き換えで消えた。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1100
- https://github.com/waired-ai/waired-agent/issues/1096
- docs/knowledges/20260828/0510-menu-labels-are-markup-on-two-of-three-os.md
- docs/knowledges/20260828/2145-writing-a-menu-label-per-renderer.md
