---
status: accepted
---

# ブラウザ起動は入口を 1 つに保ち、root なら常にデスクトップユーザへ降格する (20260726 18:05)

## Status
Accepted

## Context
サインインリンクを開く経路が 3 OS すべてで壊れていた(#181 Windows /
#182 macOS / #183 Linux)。原因は OS ごとに違うが継ぎ目は同じで、インストーラが
init を昇格して実行するのに「既定のブラウザ」は root ではなくデスクトップ
ユーザの属性である、という一点に帰着する(詳細:
docs/knowledges/20260726/1805-desktop-user-browser-open.md)。

#182 の起票時の提案は `browser.Open` はそのままに `OpenAsDesktopUser` を
別入口として足す形だった。しかし #182 の欠陥そのものが「呼び出し側が権限を
意識した方を選び忘れた」(issue の言葉では "Only the sign-in path skips it")
というものであり、入口を 2 つにするとその踏み外しが構造として残る。

加えて、トレイが 3 OS 分の `OpenBrowser` を丸ごと重複保持しており、CLI 側を
共有パッケージへ切り出した際にトレイが取り残されていた。Windows の欠陥は
その重複のせいでトレイの「ログイン…」も同時に壊していた。

## Decision
- **入口は `browser.Open` ひとつ**。`OpenAsDesktopUser` は作らない。euid != 0 の
  ときは従来どおり、euid == 0 のときは `$SUDO_USER`(macOS はさらに
  `/dev/console` の所有者、Linux は `PKEXEC_UID`)へ降格してから起動する。
  呼び出し側(login gate / codeui / トレイ)は無変更で修正が乗る。
- **ホップ失敗は直接起動へフォールバック**する。降格できないことを理由に
  「何も開かない」に落とさない。
- **タイムアウトは失敗として扱わない**(フォールバックしない)。ランチャは
  大抵起動済みで、フォールバックすると 2 枚目のウィンドウが開く。
- OS 差のある argv はすべて**タグなしの純粋関数**に寄せ、GOOS テーブルテストで
  固定する。CI のユニットジョブは Linux のみで、`make verify-cross` は型検査
  しかしないため、そうしないと 3 OS 分の argv は誰にも見えない。
- **トレイの重複は削除**し、`internal/gui/tray/browser.go` の薄いラッパ 1 本に
  統合する。`trayhost` の `hasDisplay` 複製も `browser.HasDisplay` に寄せる。

## Consequences
- 「昇格中に開いたブラウザが本人のものでない」という欠陥は、呼び出し側の
  選択ミスとしては再発しえなくなった。新しい呼び出し側も自動的に正しい。
- Windows には降格の概念を持ち込まない(UAC 越えの環境ブロックと引数の
  クォートは #192 / #177 として別建て)。
- `HasDisplay` は昇格中も **現プロセスの env** を見るまま据え置いた。
  `DISPLAY`/`XAUTHORITY` は一般的な設定では sudo を越えるため gate の判定は
  変わらない。`/run/user/<uid>` から画面の有無を推定する案はゲートの分岐を
  変えるので、必要になったら別 issue で扱う。
- 副作用として `Open` は空 URL と `-` 始まりの URL を弾くようになった
  (`open(1)`/`xdg-open` にフラグとして解釈されるのを防ぐ)。実呼び出し側は
  すべて http(s) なので影響はない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/181 / #182 / #183
- https://github.com/waired-ai/waired/issues/932 (G4)
- internal/platform/browser/desktopuser.go, internal/gui/tray/browser.go
- docs/knowledges/20260726/1805-desktop-user-browser-open.md
