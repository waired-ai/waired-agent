---
status: accepted
supersedes:
  - docs/decisions/20260821/0228-uninstall-removes-what-is-running.md
---

# アンインストールは動いているトレイを止める — unlink は成功しても、プロセスが残る (20260827 23:07)

## Status
Accepted

## Context

waired-agent#1031 (0.0.3-rc4 実機検証、Ubuntu + GNOME): `uninstall.sh --clean` は
パッケージも state も apt ソースも消し、デバイス登録も解除して完走した — そして
per-user の `waired-tray` プロセスが、unlink 済みの `/usr/bin/waired-tray` から
走り続けていた。アイコンはデスクトップに残り、クリックに反応しなくなり、次の
クリーンインストール後は**消されたバイナリのまま新しいデーモンと話し続けた**。
ログアウトするまで。

2026-08-21 の裁定 (docs/decisions/20260821/0228-uninstall-removes-what-is-running.md)
は既に「アンインストールは、Waired が動いていても Waired を消す」と定めていたが、
その Consequences は実装を「**Windows 限定**」に絞っていた。理由はこうだった:
「POSIX では実行中のバイナリを unlink できるので、この問題は構造的に存在しない
(CLAUDE.md §Cross-OS parity の『他の 2 OS を確認して、同じ PR で変えるか、なぜ
不要かを述べる』に対する答え = 不要)」。

**#1031 が反証したのは裁定ではなく、この理由づけである。** unlink は確かに成功
する。実害は、生き残った**プロセス**のほうだった。

機構は両側にあった。

### トレイ側: シグナルを飲み込む GUI ループ

`cmd/waired-tray/main.go` は `signal.NotifyContext(SIGINT, SIGTERM)` で context を
作り、`internal/gui/tray/tray.go` の `Run()` に渡す。`Run()` は `systray.Run(...)`
でブロックするが、`ctx.Done()` で `systray.Quit()` を呼ぶものがどこにも無かった —
呼び出しは Quit メニューの腕だけ。`fyne.io/systray` v1.12.2 の `nativeLoop()` は
`<-quitChan` でブロックし、それを閉じるのは `Quit()` だけなので、ポーラーと
クリックループは戻っても GUI イベントループは戻らない。さらにハンドラ登録は
Go の既定の terminate 処理を外すので、**以後の SIGTERM は全部飲み込まれ、
SIGKILL しか効かない**。これが waired-agent#1045 である。実は
docs/knowledges/20260822/1906-tray-row-ab-capture-on-real-hardware.md §1 が
「`waired-tray` は SIGTERM を無視するので `kill -9`」と、バグではなく回避手順
として記録済みだった。

### アンインストール側: 誰もプロセスに触れない

- `packaging/install/uninstall.ps1` には `Stop-Tray` と `Stop-InstallDirProcesses`
  があった。`uninstall.sh` の Linux 側はプロセスに一切触れなかった。
- install.sh 自身の完了バナーが案内する `sudo apt purge waired waired-tray` 経路
  には `prerm` そのものが無かった。
- macOS 側の `launchctl bootout gui/<uid>/com.waired.tray.waired-tray` が届くのは
  **launchd が起動したトレイだけ**で、install.sh の `darwin_start_app` が
  `open -g /Applications/Waired.app` で起動するもの — LaunchServices の
  アプリケーション — には届かない。そのバンドルは生きているプロセスの足元から
  `rm -rf` されていた。しかも bootout は `common_run_user` (`sudo -u`) 経由で、
  これは uid は変えるが Mach bootstrap namespace を変えないので、別セッションの
  `gui/<uid>` にはそもそも届かない。届くのは `launchctl asuser` で、install.sh の
  `darwin_start_app` 自身が既にそれを使っていた。

## Decision

1. **0228 の裁定を POSIX に拡張する。** アンインストールは Linux / macOS でも、
   両段 (通常の remove と `--clean`) で、**何かを消す前に**走っている
   `waired-tray` を止める: SIGTERM → 有界の待ち (`TRAY_STOP_GRACE`、15 秒) →
   SIGKILL。各 PID と、どこから走っていたかを 1 行ずつ名指しする。実装は
   `packaging/install/uninstall.sh` の `common_stop_tray` / `common_tray_pids` /
   `common_tray_pids_from`。
2. **トレイはシグナルで終了し、そのとき Quit メニュー項目と同じ手順でこのマシンを
   畳む** — メッシュから引き上げ (`SuspendShare`)、エンジンを止める
   (`StopEngine`)。これは #1031 を直したセッションでの**オーナー裁定
   (2026-08-27)**。根拠: #316 が Quit の畳み方を裁定したのは「誰も鍵盤の前に
   いない間」に peer が本マシンへルーティングされ続けてはならないからで、
   デスクトップからのサインアウトは同じ事象が別経路で届いたものである。
   継ぎ目は `internal/gui/tray/shutdown.go` — `shutdownCause` / `planShutdown` /
   `shutdown` / `watchShutdown`。
3. **watcher は `onReady` の中から起動し、`systray.Run` より前には決して起動
   しない。** `systray.Quit()` は `quitOnce.Do(quit)` でプロセスの生涯に一発、
   そして 3 バックエンド中 2 つは立ち上がる前の呼び出しを取り落とす: darwin の
   `quit()` は nil の `owner` (`registerSystray` で設定される) にメッセージを送り、
   Windows はゼロのウィンドウハンドルへ `WM_CLOSE` を post する。どちらも
   `quitOnce` を消費し、以後アプリは**シグナルでも自分のメニューでも**終了
   できなくなる。`onReady` より早く届いたシグナルは、代わりに `cmd/waired-tray`
   自身の `shutdownDeadline` がプロセスを終わらせる。
4. **選択はプロセス名で行い、インストールパスでは行わない** — `uninstall.ps1` の
   `Stop-Tray` が従来から使っている規則と同じ。パスでゲートすると、まさに止める
   ために存在する当のプロセス (バイナリは unlink 済み) を取り逃がすし、
   再インストール後はパスが復活して**別の inode** を指す。解決したパスは情報と
   してログに出すだけで、ゲートにはしない。
5. **per-user の Linux autostart エントリ `~/.config/autostart/waired-tray.desktop`
   も消す。** アプリ自身の「Start Waired on login」トグルが書き、どのパッケージも
   所有しないファイルで、macOS の LaunchAgent plist・Windows の HKCU Run 値の
   扱いと揃う。
6. **`waired-tray` の `prerm` が `apt remove` / `apt purge` 経路をカバーする。**
   そこで走るスクリプトは他に無いからである。`upgrade` では意図的に何もしない:
   dpkg はバイナリをその場で置き換え、走っているプロセスは旧 inode を保持する
   ので、そこで殺して戻さなければ `apt upgrade` のたびにアイコンが消える。

## Consequences

- **Windows に鏡写しにする graceful な腕は無い**: Windows の `os/signal` は
  SIGTERM を配送しないし、トレイは `-H windowsgui` でリンクされるのでコンソール
  制御イベントの口も無い。Windows での等価物はウィンドウメッセージである —
  fyne.io/systray の隠しウィンドウが `WM_CLOSE` (`/F` 無しの `taskkill` が post
  するもの) を処理する — ので、`Stop-Tray` はまず `CloseMainWindow` で頼み、
  `Stop-Process -Force` を最後の砦として残す。Windows で SIGTERM を列挙するのは
  cargo-cult だという裁定は `cmd/waired-agent/signal_windows.go` に既にあり、
  `cmd/waired-tray` はタグ無しファイルでまさにそれをやっていた。
- **デスクトップからのサインアウトがマシンを畳むようになる。** パッケージの
  `/etc/xdg/autostart/waired-tray.desktop` はトレイを `gnome-session` の子にし、
  セッション終了時に子へ SIGTERM が送られる — つまりこれはアンインストール時
  だけでなく**毎回のログアウトで**発火する。
  docs/decisions/20260801/1035-mesh-share-suspension-is-live-only.md の拡張であり、
  同記録の Consequences「トレイが起動していないマシン（サーバ、ヘッドレス）は
  suspend されない」はそのまま真 (トレイが無ければ suspend も無い)。途中で
  打ち切られた shutdown は `share_suspended` をラッチしたまま画面上のトレイが
  無い状態を残しうるが、`resumeSharingOnStart` が次のトレイ起動で解除するし、
  この裁定の下ではログアウト済みのマシンはそもそも suspend されているのが正しい。
- `common_run` は `DID_COUNT` を養い、それが Waired の無いマシンで `print_done`
  に「Nothing to remove」と言わせている (waired-agent#793)。トレイ停止は
  「トレイが実際に居る」ことでゲートし、居たときはその停止が「何かを消した」
  ことに数えられる。
- 4 つの `*ViaElevation` ヘルパは特権つきの子プロセスをトレイ自身の context を
  持つ `exec.CommandContext` で走らせていた。何も cancel しない間この結合は
  不活性だったが、今回から cancel するものができたので切り離した
  (`context.WithoutCancel`) — さもないと「Update Waired」の途中でログアウト
  すると、昇格済みインストーラを入れ替えの最中に殺す。
- **スコープ外、waired-agent#1046 として追跡**: **更新**がバイナリを入れ替える
  とき、走っているトレイを止めも再起動もしない — 3 OS とも。「Updated」と報告
  するアプリは前のバイナリのままである。未解決の設計問題を名指ししておく:
  macOS では kill + `open -g` の単純な再起動にはできない。再起動したトレイの
  `ensureAutostartOnFirstLaunch` は plist が無ければ LaunchAgent plist を書く
  (`firstLaunchAutostartApplies` は darwin で true) ので、「Start Waired on
  login」を切ったユーザーの autostart を黙って再登録してしまう — install.sh の
  `darwin_tray_autostart_notice` のコメントが拒んでいる、まさにその覆しである。
  `launchctl kickstart -k` は安全だが、ジョブが bootstrap 済みのときに限る。
- `planShutdown` がテーブルなのは、#1046 が `causeRestart` の行を足すため
  である: 再起動はマシンを畳んでは**ならない**。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1031
- https://github.com/waired-ai/waired-agent/issues/1045
- https://github.com/waired-ai/waired-agent/issues/1046
- https://github.com/waired-ai/waired-agent/issues/793
- https://github.com/waired-ai/waired-agent/issues/316
- docs/decisions/20260821/0228-uninstall-removes-what-is-running.md
- docs/decisions/20260801/1035-mesh-share-suspension-is-live-only.md
- docs/knowledges/20260822/1906-tray-row-ab-capture-on-real-hardware.md
- internal/gui/tray/shutdown.go
- cmd/waired-tray/main.go
- packaging/install/uninstall.sh
- packaging/install/uninstall.ps1
- packaging/debian/waired-tray/prerm
