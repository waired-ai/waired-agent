---
status: accepted
supersedes:
  - docs/decisions/20260827/2307-uninstall-stops-the-running-tray.md
---

# アップデートは動いていたアプリを元に戻す — 閉じたものだけを、新しい版で (20260828 00:27)

## Status

Accepted。docs/decisions/20260827/2307-uninstall-stops-the-running-tray.md の
うち、Windows の停止手段の記述と、waired-agent#1046 をスコープ外とした項
(`causeRestart` 行の予告を含む) をこの記録が引き継ぐ。同記録の裁定本体
(アンインストールは動いていても止めて消す) と POSIX 側は有効のまま。

## Context

2 つの issue、1 つの変更: waired-agent#1046 (アップデートが旧アプリを走らせた
まま残す — 3 OS とも) と waired-agent#1059 (Windows のトレイに graceful な
停止が無く、Windows のログアウトは何も畳まない)。

### 2307 が open のまま残したもの

2307 の Consequences は #1046 をスコープ外とし、ブロッカーを名指ししていた:
macOS では kill + `open -g` の単純な再起動にはできない。開き直したトレイの
`ensureAutostartOnFirstLaunch` (internal/gui/tray/tray.go) は plist が無ければ
LaunchAgent plist を書き、`firstLaunchAutostartApplies` は darwin で true —
つまり開き直すだけで、「Start Waired on login」を切ったユーザーの login item を
黙って再登録してしまう。それは install.sh (`darwin_tray_autostart_notice`) の
コメントが自分の言葉で拒んでいる、まさにその覆しである:

> nothing distinguishes "never registered" from "the user switched it off" —
> the tray infers first launch from the plist's presence alone, with no
> marker. Registering here would silently overturn that choice on every
> update.

### 実測が覆したもの

2307 は当初、Windows について「POSIX シグナルの等価物はウィンドウメッセージで
あり、`Stop-Tray` はまず `CloseMainWindow` で頼み、`Stop-Process -Force` を
最後の砦に残す」と書いていた。**それは読んで書いたもので、測っておらず、誤り
だった。** sv-evox2 (Windows 11、PowerShell 5.1、2026-08-27)、走っている rc4 の
実トレイに対する実測:

```
tray pid=6360  path=C:\Program Files\Waired\waired-tray.exe
tray mainwindow=[0] title=[]
CloseMainWindow returned=False   -> still alive
taskkill /IM waired-tray.exe     -> ERROR: ... could not be terminated.
                                    Reason: This process can only be terminated
                                    forcefully (with /F option).
                                 -> still alive after 20s
```

`waired-tray.exe` は `-H windowsgui` でリンクされ、ウィンドウを一度も表示
しないので、`Process.MainWindowHandle` は 0 で `CloseMainWindow()` は `$false`
しか返せない。`/F` 無しの `taskkill` も `WM_CLOSE` を post する先を見つけられ
ない。fyne.io/systray は確かに `WM_CLOSE` を処理する (`systray_windows.go` の
`wndProc`) し、そのウィンドウは `SystrayClass` クラスの実在する
`WS_OVERLAPPEDWINDOW` である — ただ一度も表示されない。それが両方の呼び手から
届かない理由のすべてである。

同時に見つかった第二の Windows の穴: `internal/gui/tray/tray.go` は systray の
`onExit` に `func() {}` を渡していた。systray は `WM_DESTROY` と
`WM_ENDSESSION` で `runSystrayExit()` を呼ぶので、Windows の**ログアウト/
シャットダウン**は、アイコンを消す以外に何も走らない経路でトレイを落として
いた。2307 に記録されたオーナー裁定 (2026-08-27) — シグナルは Quit と同じ意味 —
は、したがって Linux と macOS では成立し、`-H windowsgui` のプロセスへそもそも
シグナルを配送しない Windows では、静かに成立していなかった。

## Decision

1. **畳む処理は systray の `onExit` にぶら下げ、tray 側の `sync.Once` でガード
   する** (internal/gui/tray/shutdown.go の `windDown` / `onSystrayExit`)。
   これで Windows はログアウト/シャットダウン時の畳みを `WM_ENDSESSION` 経由で
   得る — 新しい機構なしで。Once は飾りではない: メニューやシグナルで抜ける
   ときは畳んで**から**ループを quit し、ループの quit 自体が `onExit` を呼ぶ。
   systray 自身の `systrayExitCalled` ガードは *systray* がコールバックを二度
   呼ばないことしか保証せず、こちら側がデーモンに二度届くかどうかについては
   何も言わない。`planShutdown` には `causeWindowClose` が増え、それは畳む。
2. **アップデートは自分が閉じたアプリを開き直す — それ以外は何もしない。**
   `tray_restart_plan` (install.sh) と `Get-TrayRestartPlan` (install.ps1) は
   両側で同じテーブル: `restart` / `skip:no-tray` / `skip:not-running` /
   `skip:not-shipped` / `skip:no-session` (Windows は `skip:no-console-user`)。
   `skip:not-running` の行が節度そのもの — アップデートはユーザーが閉じた
   アプリを**開いては**ならない。`darwin_tray_autostart_notice` がユーザーの
   代わりに決めることを拒んでいるのと、同じ判断である。
3. **トレイは「走ったことがある」を記録する** (per-user の対話 state dir の
   first-run マーカー; internal/gui/tray/autostart_firstrun.go の
   `planFirstLaunchAutostart` が判断: `register` / `skip:already-enabled` /
   `skip:user-decided` / `skip:not-applicable`)。2307 が名指しした曖昧さ
   (「未登録」と「ユーザーが切った」を区別できない) を取り除くのはこれで、
   それ自体が独立の修正でもある: これ以前は「Start Waired on login」を外しても、
   macOS と Windows ではアプリの次回起動で復活していた。
4. **再起動の機構は OS ごと。** Linux: セッション環境は走っているプロセス自身の
   `/proc/<pid>/environ` から採取する — それこそが当のアプリが描画に使っている
   環境なので、構成上正しい唯一の源である。`$SUDO_USER` は複数人がログイン
   しうるマシンで 1 人を選んでしまうし、`curl | sh` の形では空になる。
   `systemd-run --scope` で新プロセスをインストーラ自身のセッション scope から
   出す (`setsid` は制御端末を切るが cgroup は切らないので、無いとインストーラを
   走らせたシェルを閉じただけでアプリが道連れになる)。macOS: トレイが launchd
   ジョブなら `launchctl kickstart -k` (launchd 自身の再起動で、何も登録しない)。
   install.sh (`darwin_start_app`) が開いた LaunchServices のアプリ — その
   ジョブではない — なら、止めてから `open -g`。Windows: `Extract-Zip` の前に
   止め、サービス復帰後に `explorer.exe` 経由で開き直す。前に止めるのは、直後に
   開き直す以上スワップと競走させないためである。`Move-IntoInstallDir` に
   held image を退避させずに済むという副次効果もあるが、**それは機構ではない** —
   sv-evox2 の実測 (2026-08-27) では動いているトレイに対しても置換が成功し、
   `.displaced-` は 1 つも残らなかった。版ずれを起こしていたのは単に
   「プロセスを誰も再起動しない」ことだった。この開き直しは、新規
   インストールの起動と違って意図的に `Test-InteractiveStdin` でゲートしない —
   あの述語は「*この*プロセスにコンソールがあるか」を訊くもので、トレイ発の
   アップデートは昇格経由でコンソール無しにインストーラへ届くから、その
   ゲートは閉じたアプリを閉じたままにしてしまう。正しい質問は
   `Get-ConsoleUser` である。
   同じ実機調査から出たガードがもう 1 つある: evox2 では **ssh ログインが
   session 0、デスクトップが session 2** で、`Start-Process` は自セッションに
   しか届かない。つまり ssh から回したインストーラはアプリを見つけて止められ、
   戻せない — それは版ずれより悪い(次のサインインまでアイコンが消える)。
   `Get-TrayRestartPlan` に `skip:other-session` を置き、戻せないときは
   **そもそも止めない**。その場合の挙動は従来どおりである。
5. **Windows のアンインストールは terminate し、そう言う。** 2307 が記述した
   `CloseMainWindow` の腕はデッドコードとして撤去し、`Stop-Tray` は
   `Stop-Process -Force`。その経路では畳みは走らない — そこでエンジンを解放
   するのは数ステップ後のサービス撤去で、それはどのみち走る。Windows の
   **ログアウト**のほうは 1. 経由で畳む。

## Consequences

- 2307 の Windows の Consequence は上の実測が置き換える。裁定本体
  (アンインストールは Waired が動いていても Waired を消す) と POSIX 側は有効。
- `planShutdown` に「restart」の cause は意図的に足さない (2307 末尾の
  `causeRestart` の予告はこれで置き換え)。インストーラの再起動はトレイへ通常の
  シグナルとして届き、その時点でデーモンは既に再起動済み — それ自体がエンジンを
  止める — なので、その経路の畳みはコストが無く、見分ける手段も要らない。
  答えを変える cause なら行に値するが、変えるものが無い。
- 既存ホストはマーカーへ収束する (反転しない): login item を既に持つホストは
  `enabled` として見つかり、切っていたホストは高々もう 1 回の再登録
  (今日の挙動) を経て、以後は二度と起きない。
- マーカーは per-user state dir に住み、そこはテストスイートが読み書きする —
  internal/gui/tray の `TestMain` がパッケージに対して `$WAIRED_STATE_DIR` を
  seal し、first-launch のテストは各自 fresh を取る (CLAUDE.md §Test
  discipline)。
- Linux の「このホストはトレイパッケージを入れたか」は
  `linux_pkg_installed waired-tray` で答え、パスへの `test -x` では答えない —
  `linux_apt_update` の他の部分が既に訊いている質問と同じで、実バイナリの
  無い環境でもテスト可能な形が残る。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1046
- https://github.com/waired-ai/waired-agent/issues/1059
- https://github.com/waired-ai/waired-agent/issues/1031
- https://github.com/waired-ai/waired-agent/issues/1045
- docs/decisions/20260827/2307-uninstall-stops-the-running-tray.md
- docs/decisions/20260821/0228-uninstall-removes-what-is-running.md
- internal/gui/tray/shutdown.go
- internal/gui/tray/autostart_firstrun.go
- packaging/install/install.sh
- packaging/install/install.ps1
- packaging/install/uninstall.ps1
