# メニューのラベルは 3 OS 中 2 つで「マークアップ」として読まれる (20260828 05:10)

## Issue

`Privacy & safety…` が Windows の実機で `Privacy  safety…` と描画されていた
(agent#1096)。`&` が消え、`s` に下線が付く。rc4 のパッケージ版でも main の
ビルドでも同じなので、この行が生まれてからずっとそうだった。

「Windows だけ直せばいい」と思って調べたら、**Linux にも同じ形の欠陥があり、
しかもそちらは直せない**ことが分かった。

## Learnings

### 特殊文字は OS ごとに違う

| | `&` | `_` |
|---|---|---|
| **Windows**(`MFT_STRING`) | **食われる**(ニーモニック接頭辞)。`&&` で 1 個描画 | そのまま |
| **macOS**(`NSMenuItem.title`) | そのまま | そのまま |
| **Linux**(dbusmenu `label`) | そのまま | **食われる**(仕様) |

dbusmenu 仕様(`com.canonical.dbusmenu.xml` の `label`)の逐語:

> two consecutive underscore characters "__" are displayed as a single
> underscore, any remaining underscore characters are not displayed at all,
> the first of those remaining underscore characters (unless it is the last
> character in the string) indicates that the following character is the
> access key.

**方向が逆なので、エスケープは必ず OS ごとに分ける。** Linux/macOS で `&` を
倒すと画面に `&&` が出る。

### `_` は「仕様どおりに直す」と GNOME で別の形に壊れる

- **Plasma**(libdbusmenu-qt `swapMnemonicChar`)は**仕様準拠**。`__`→`_`、
  最初の `_`→Qt の `&`、`&`→`&&`。エスケープすれば完全に直る。
- **GNOME**(gnome-shell-extension-appindicator `dbusMenu.js`)は
  `label.replace(/_([^_])/, '$1')` — **`g` フラグが無い**。最初の単独 `_` を
  1 個消すだけで、`__` を畳まない。

実機(GNOME Shell 50.1 + ubuntu-appindicators)で、**インストール済み拡張の
その正規表現を gjs にそのまま流して**採った値:

```
"…q4_K_M"     -> "…q4K_M"      (今)
"…q4__K__M"   -> "…q4_K__M"    (エスケープ後)
"a_b"         -> "ab"
"a__b"        -> "a_b"          ← _ が 1 個なら直る
"Privacy & safety…" -> 変化なし  ← & は素通し
```

つまり **`_` が 1 個のラベルはエスケープで直り、2 個以上は別の形に壊れる**。
`q4_K_M` と `ANTHROPIC_BASE_URL` はどちらも 2 個。両方のレンダラで正しく出る
文字列は存在しない。

> **訂正(20260828 21:45)** — ここで止めたのは前提が 1 つ足りなかったから。
> 両方に正しい 1 つの文字列は無いが、**レンダラごとに正しい文字列は両方存在し、
> どちらが描いているかは実行時に分かる**(`org.kde.StatusNotifierWatcher` の
> 所有者 PID → `/proc/<pid>/comm`)。GNOME 用のエスケープは「最初の下線ランに
> `_` を 1 個足す。末尾ランなら何もしない」。詳細と上流調査は
> `docs/knowledges/20260828/2145-writing-a-menu-label-per-renderer.md`、
> 裁定は `docs/decisions/20260828/2140-a-menu-label-is-written-for-its-renderer.md`。

### `_` は思ったより多くの行に入る

ollama の量子化タグ(`qwen3.6:35b-a3b-q4_K_M`、Model 行とピア行のフォールバック)、
Claude 行の `ANTHROPIC_BASE_URL`、`first_last@` 形式のメール、
`/home/dev_user/…` を含む OpenCode/OpenClaw の設定パス、エンジンの `LastError`。

### エスケープはモデルではなく**描画の直前**でやる

`internal/gui/tray/rows.go` の `setTitle` が 56 箇所の動的ラベルの唯一の出口。
ここでやると:

- テストがほぼ壊れない(既存の約 200 のアサートは `MenuModel` を見ている)
- **状態レポート/クリップボード/デバッグダンプが汚れない** — これらは
  同じ `MenuModel` の文字列を読むが、メニューではない

`MenuModel` の中でエスケープすると、クリップボードに `q4__K__M` が入る。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1096
- https://github.com/waired-ai/waired-agent/issues/1100 (`_` の側 — 20260828 に解決)
- https://github.com/gnustep/libs-dbuskit/blob/master/Bundles/DBusMenu/com.canonical.dbusmenu.xml
- https://github.com/ubuntu/gnome-shell-extension-appindicator/blob/master/dbusMenu.js
- https://github.com/desktop-app/libdbusmenu-qt/blob/master/src/utils.cpp
