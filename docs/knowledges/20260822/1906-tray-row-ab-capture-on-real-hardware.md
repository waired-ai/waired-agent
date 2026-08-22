# tray の行を実機で証明する — 同一実行の A/B (20260822 19:06)

## Issue

waired-agent#986(統合行がサービスユーザの HOME を見ていた)と、その副産物
(OpenCode の `Reconfigure…` にクリックハンドラが無く、押しても何も起きなかった)を
実機で示す必要があった。tray の行は「ユニットテストでは緑」「実機では嘘」という
形で壊れうるので、**画面に出ている文字列そのもの**を証拠にしたかった。

座標クリックは使えない(4K/200% の機では UIA が論理座標を返したり物理座標を
返したりして誤爆する)。スクリーンショットも GNOME では `AccessDenied`。

## Learnings

### 1. 「観測者だけ動かす」A/B が一番強い(Linux)

daemon は据え置き(rc3 のまま)、**tray バイナリだけ差し替える**と行が
`○ not configured` → `● configured` に変わる。同じ daemon・同じホーム・同じ
ファイルなので、変わったのは「誰が見ていたか」だけだと一撃で示せる。
sudo も要らない(tray はデスクトップユーザのプロセス)。

- `waired-tray` は **SIGTERM を無視する**ので `kill -9`。セッション env は
  `/proc/<pid>/environ` から採って `setsid nohup env … /usr/bin/waired-tray -mgmt …`。
- 終わったらパッケージ版に戻す。戻ったことは `dpkg -V waired-tray`(無出力=一致)で確認できる。

### 2. Windows は UIA で開き、MSAA で読む

- アイコンは既定でオーバーフローに隠れる →
  `HKCU\Control Panel\NotifyIconSettings\<id>\IsPromoted`(DWORD)= 1。
  エントリは**実行ファイルのパス単位**。終わったら値を削除して戻す。
- アイコンは UIA の Button だが **Name はツールチップ**(例 `⚠ Claude Code routing inactive`)。
  `InvokePattern.Invoke()` でメニューが開く。
- Win32 ポップアップ(`#32768`)は **UIA からは空に見える**。`AccessibleObjectFromWindow`
  (`OBJID_CLIENT` = `0xFFFFFFFC`、PS では `[uint32]4294967292`)+ `AccessibleChildren` で
  `accName` / `accState` が読める。列挙は **C# 側(Add-Type)に押し込む** — COM の
  `IAccessible` は PowerShell の引数変換で落ちる。
- サブメニューは**新しい `#32768`** として現れるので、親をクリックしてから
  ウィンドウ一覧を採り直す。
- 出力は cp932 に化けるので `[Console]::OutputEncoding` を UTF-8 にしてから読む
  (`● ○ ✓ ✗ ⚠` が全部潰れる)。

### 3. 「押しても何も起きない」の証明は、同一実行の 2 行比較で

`accDoDefaultAction` でクリック → 数秒待って `#32770`(MessageBox)を数える、を
**同じ実行の中で 2 つの行に対して**行う。片方はダイアログ 0 件、もう片方は
`Reconfigure OpenClaw integration?` が出た。**同一実行の A/B なので「押し方が悪かった」
という反論を潰せる**のが要点。既存の常駐ユーティリティの `#32770` が混ざるので、
クリック前後の差分で見る。

ダイアログ本文は `IAccessible` の子の `accName` で採れる(製品文言の逐語証拠になる)。
答えるのは `SendMessage(hDlg, WM_COMMAND, IDYES=6 / IDNO=7, 0)` — ボタンは Pane として
出るため UIA の Invoke は効かない。

### 4. パッケージ更新は「ディスク上のバイナリ」しか替えない

before/after を採るには **tray プロセスの再起動が要る**。Windows は
`Stop-Process -Name waired-tray -Force` → `Start-Process`。これを忘れると
「新 daemon × 旧 tray」という、どちらの版でもない組み合わせを測ることになる。

### 5. Smart App Control は版ごと・ファイルごとに判定する

未署名バイナリを SAC 有効機で走らせるとき、`StartService FAILED 4551` /
`0x800711C7`(CodeIntegrity event 3077 + 3118)で拒否されることがある。
**同じ zip から入った 2 つの実行ファイルで判定が割れる**(片方は起動し、片方は拒否)、
**数日前まで動いていたファイルが後から拒否に反転する**、の両方を実測した。
詰まったら版を上げて再試行する — 「このマシンでは無理」と結論しない。
SAC の無効化は Windows では一方通行なので、検証のために落とさない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/986
- https://github.com/waired-ai/waired-agent/pull/988
- https://github.com/waired-ai/waired-agent/issues/315 / #653(SAC と doctor の診断)
- docs/decisions/20260822/1742-integration-rows-belong-to-the-desktop-user.md
