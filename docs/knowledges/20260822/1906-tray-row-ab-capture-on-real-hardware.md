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

- **訂正 (20260828)**: ここには「`waired-tray` は SIGTERM を無視するので `kill -9`」と
  書いていたが、**waired-agent#1045 / PR#1062 でシグナルは効くようになった**。ただし
  **どちらを送るかは「そのホストが配信中か」で決まる**。SIGTERM は `planShutdown` の
  wind-down を走らせる — **メッシュから外れ、エンジンが止まる**。これは欠陥ではなく
  ratified な契約(#316 + オーナー裁定 20260827: デスクトップからのサインアウトは
  Quit と同じ意味)。実測(20260828、sv-mag): `kill -TERM` → `engine_power: stopped`、
  VRAM 20990MiB → 30MiB。
  - **観測者だけ差し替える A/B では `kill -9`**。wind-down が走らないので、配信中の
    エンジンはそのまま動き続ける。差し替える tray は使い捨てなので片付けは要らない。
  - SIGTERM を使ってしまった/使う必要があるなら、**`waired inference engine start` で
    戻す**(sv-mag の vLLM で `ready` まで約 30 秒)。戻ったことは
    `subsystem_state`/`engine_power` と VRAM で確認する。
  - セッション env は `/proc/<pid>/environ` か、別セッションが置いた env ファイルから
    採って `setsid nohup env … /usr/bin/waired-tray -mgmt …`。
- **`pkill -f <パス>` を ssh 越しに使わない。** リモートの `bash -c` のコマンド行自体に
  そのパスが含まれるので、**pkill が自分のシェルを殺す**(出力ゼロで途中終了し、
  後続の復旧手順が走らない)。pid 指定か、パターンを分割して書く。
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
  ウィンドウ一覧を採り直す。ただし **`#32768` は閉じても破棄されない**ので、開く前後の
  差分で「新しく出たウィンドウ」を探すと 2 回目以降は何も見つからない。
  `GetWindowThreadProcessId` で **tray の pid のもの**に絞り、`IsWindowVisible` で
  足切りする(トップレベルは「`Quit` を含むほう」で判別できる)。
- **`accName` だけでなく `accRole` と `accState` も採る。** 名前だけ見ると
  セパレータ(`role=21` = `ROLE_SYSTEM_SEPARATOR`、`state=0x1` = DISABLED)が
  「無名の有効な行」に見え、実在しない欠陥を起票することになる
  (waired-agent#1032 の後半がこの誤読だった)。メニュー項目は `role=12`。
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
**検証のために SAC を落とさない。**

> **訂正 (20260822 22:16)**: ここには当初「SAC の無効化は Windows では一方通行
> なので」と書いていたが、それは誤り。一方通行なのは **Settings 経由**(現在
> evaluation のときを除く)で、[Microsoft はレジストリで任意のモードに強制する
> 手順を公開している](https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/test-your-app-with-smart-app-control)。
> 落とさない方針は変わらないが、理由は「不可能だから」ではなく **保護を落とす
> ことになり、かつこの実機が評判判定の唯一の観測所だから**である。
> なお**署名要件のほうは CI で測れる** —
> `docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md`。

## Refs
- https://github.com/waired-ai/waired-agent/issues/986
- https://github.com/waired-ai/waired-agent/pull/988
- https://github.com/waired-ai/waired-agent/issues/315 / #653(SAC と doctor の診断)
- docs/decisions/20260822/1742-integration-rows-belong-to-the-desktop-user.md
