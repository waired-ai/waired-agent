# サインインブラウザと権限境界 — 3 OS それぞれの落とし穴 (20260726 18:05)

## Issue

`waired init` はサインインリンクを表示したあとブラウザを開くが、その起動が
3 OS すべてで壊れていた(#181 / #182 / #183)。症状も原因も OS ごとに違うのに、
壊れている継ぎ目は 1 つ — **インストーラが越える権限境界**(waired#932 の G4)。
インストーラは init を昇格して実行するのに、「既定のブラウザ」は root ではなく
**デスクトップユーザの属性**である、という一点に全部が帰着する。

## Learnings

### Windows: `CreateProcess` の `lpApplicationName` は `%PATH%` を探索しない

`lpApplicationName` が非 NULL のとき、Win32 は **カレントディレクトリ基準でしか**
名前を解決しない(`%PATH%` は見ない)。裸の `"rundll32.exe"` を渡していたため、
CLI の通常の作業ディレクトリ(ユーザのホーム)から呼ぶと `err=2`
"The system cannot find the file specified" で何も開かなかった。

- 報告者の実機検証: ホームから `err=2` / `app=NULL` で成功 / CWD を System32 に
  すると成功。**セッションや SYSTEM トークンの問題ではない**(同じ対話セッション、
  HKCU の `http` UserChoice も正常)。
- 退行の経緯: 元実装は `exec.Command("rundll32", ...)` で `LookPath` を通っていた。
  トレイから共有パッケージを切り出した際に raw `CreateProcess` 形へ変わって混入。
- 対処は `windows.GetSystemDirectory()` で絶対パス化。解決できない場合は
  `lpApplicationName = NULL` に落とす(コマンドライン側の通常の探索順に戻る)。

### macOS: `open(1)` は **euid** の LaunchServices を見る($HOME ではない)

`open(1)` は実効 uid のハンドラマップを引く。root には `http`/`https` の
ハンドラ登録がないので Safari にフォールバックする。実機で確認済み:
**`HOME` を差し替えても結果は変わらない — 効いているのは euid**。

これは見た目の問題ではない。セットアップチケットは OAuth を完了した
ブラウザセッションに紐づくので、違うブラウザが開くと「そのユーザが使っていない
ブラウザからしかウィザードを操作できない」状態になり、リンクを Chrome に
貼り直しても(そのセッションはサインインしていないので)通らない。

`launchctl asuser <uid> <cmd>` は **ユーザの bootstrap namespace に入るだけで
root のまま**なので、内側の `sudo -u <user>` が実際の降格として必須。

### Linux: `sudo` は DISPLAY/XAUTHORITY を残し、XDG_RUNTIME_DIR/DBUS を落とす

一般的な設定の `env_reset` では `DISPLAY` と `XAUTHORITY` は素通りするが
`XDG_RUNTIME_DIR` と `DBUS_SESSION_BUS_ADDRESS` は落ちる。結果、root の
`xdg-open` は「ディスプレイは見つかるがセッションバスがない」状態で走り、
root の MIME データベースからハンドラを解決して root プロファイルの
ブラウザインスタンスを立ち上げる。

(「Linux は print-only に劣化しているのでは」という初期仮説は否定済み。
呼び出し自体は行われている。)

### 検証コマンド

```sh
# macOS: 既定のブラウザが euid で決まることの確認
sudo /usr/bin/open https://example.com                    # Safari になる
sudo -u <console-user> /usr/bin/open https://example.com  # 本来の既定ブラウザ

# Linux: セッション env を戻すと本人のブラウザで開く
sudo xdg-open https://example.com                         # root プロファイル
sudo runuser -u "$USER" -- env XDG_RUNTIME_DIR=/run/user/$(id -u) \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus \
  xdg-open https://example.com
```

### 実装上、繰り返し踏みそうな点

- **darwin ビルドは `CGO_ENABLED=0`**(Makefile: `build-agent-darwin`)。この構成の
  `user.Lookup` は `/etc/passwd` しか読まず、macOS の実ユーザは OpenDirectory に
  いるので **必ず引けない**。uid 解決には `id -u` フォールバックが要る。
  `stat -f "%u %Su" /dev/console` は名前と uid を同時に取れるので、`SUDO_USER` が
  ない root ログインのフォールバックとして使える(ログインウィンドウでは root が
  所有しているため、root は弾く)。
- ホップ失敗は **エラーとして返して直接起動にフォールバック**する。一方
  **タイムアウトはエラー扱いにしない** — ランチャは大抵起動済みで、
  フォールバックすると 2 枚目のウィンドウが開く。
- 子プロセスの stdin は必ず nil。サインインの前後で端末から Enter を読むため、
  stdin を継承した子がキー入力を奪い合う(#184 / #185 と同じ系統の事故)。

## Refs

- https://github.com/waired-ai/waired-agent/issues/181
- https://github.com/waired-ai/waired-agent/issues/182
- https://github.com/waired-ai/waired-agent/issues/183
- https://github.com/waired-ai/waired/issues/932 (G4: 権限境界で文脈が失われる)
- internal/platform/browser/desktopuser.go
