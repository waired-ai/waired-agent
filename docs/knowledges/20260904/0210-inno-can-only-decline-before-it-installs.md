# Inno Setup が「やめる」と言えるのは、置き始める前だけ (20260904 02:10)

## Issue

waired-agent#1181。GUI インストーラ(`packaging/windows/waired-setup.iss`)が、
Smart App Control に拒否されたサービス登録を握りつぶして完走し、
`waired claude enable` まで実施して成功を報告していた。
「失敗したらインストールを失敗させる」を実装しようとして、Inno Setup 6 の
どのフックなら中断できるのかを、ソース(jrsoftware/issrc、タグ `is-6_7_3`)と
実機で確かめた。**答えは「1 か所だけ」**だった。

## Learnings

### 中断できる場所は `PrepareToInstall` だけ

| フック | インストールを失敗させられるか | 根拠 |
|---|---|---|
| `PrepareToInstall` が非空文字列を返す | **できる**。終了コード **7**、`[Files]` の前・サービス停止の前・レジストリの前 | `Setup.WizardForm.pas` `ClickThroughPages`(silent 時は `LoggedMsgBox(..., Suppressible=True)` → `SetupExitCode := ecPrepareToInstallFailed` → `Abort`) |
| `[Files]` の `AfterInstall` / `BeforeInstall` で例外 | **できない**。Inno が意図的に握りつぶす | `Setup.MainFunc.pas` `NotifyInstallEntry`: *"Don't allow exceptions raised by Before/AfterInstall functions to be propagated out"* → `Application.HandleException(nil)` |
| `[Run]` エントリの失敗 | **できない**。結果を読まない | `Setup.MainForm.pas` `ProcessRunEntries` |
| `CurStepChanged(ssPostInstall)` で例外 | **できない**。`SetStep(ssPostInstall, True)` が握りつぶす | `Setup.MainForm.pas` `SetStep` |
| `[Files]` の `Check` で例外 | 失敗はさせられるが**使えない**。Ready ページの `CalcFilesSize` と `CopyFiles` の **2 回**評価される | `Setup.Install.HelperFunc.pas` `CalcFilesSize` / `Setup.Install.pas` `CopyFiles` |

`AfterInstall` は実機でも確かめた(Windows 11 / Inno Setup 6.7.3、SAC off、
`/VERYSILENT /SUPPRESSMSGBOXES`): `RaiseException` を投げても
**終了コード 0・全ファイル設置済み**で完走した。設計を組み直すまで、
これに気づかないまま「exit 4 + ロールバックになるはず」と読んでいた
(`Setup.Install.pas` の `except` は確かにそう書いてあるが、そこまで例外が届かない)。

実行順は `ssInstall` → `[Files]`/`[Registry]`/`[Icons]` → `[Run]`(非 postinstall)
→ `ssPostInstall` → 完了ページ → `[Run]`(postinstall) → `ssDone`
(`Setup.MainForm.pas:233-254`)。**`[Run]` は `ssPostInstall` より前**。

### だから、失敗しうる仕事は全部 `PrepareToInstall` に置く

`.iss` はこう組み直した:

- 3 本の exe を `Flags: dontcopy noencryption` で**1 回だけ**埋め込み、
  `ExtractTemporaryFile` で `{tmp}` に出し、`{app}\.waired-staging` に置いて
  **実際に起動して**確かめる(`install.ps1` の `Get-StagedBinaryChecks` と同じ表)。
  設置は `Source: "{tmp}\x.exe"; Flags: external` — セットアップ実行ファイルに
  2 つ目のコピーは入らない。
- **`waired-agent.exe` だけは Inno に設置させない**。`PrepareToInstall` が自分で
  置き、`install` / `start` してサービスが Running になるまで確かめる。ここが
  「まだ断れる」最後の瞬間だから。`[UninstallDelete]` がその 1 本を消す。
- `claude enable` は `ssPostInstall` で、サービスが Running のときだけ。

`ExtractTemporaryFile` は `DestName` のファイル名一致で探すが、
**`LocationEntry <> -1`(= 埋め込み済み)しか見ない**ので、同名の `external`
エントリと共存しても取り違えない(`Setup.ExtractFileFunc.pas`)。

### Inno の例外ダイアログはサイレント実行を止めない

`Application.HandleException` は Inno の `ShowExceptionMsgText` を経由し、
`LoggedMsgBox(..., Suppressible=True)` を使う。`/SUPPRESSMSGBOXES` で答えられる
ので、**例外がサイレントインストールを固まらせることはない**(waired#760 で
踏んだ「抑制されない MsgBox」とは別物)。実機で 2.1 s で完走を確認。

### ロールバックは「元から在ったファイル」を消さない

`Setup.UninstallLog.pas:894` — `CallFromUninstaller or (ExtraData and
utDeleteFile_ExistedBeforeInstall = 0)`。つまりインストール中の巻き戻しでは、
インストール前から存在したファイルは削除されない。更新経路で自前に書き戻した
旧バイナリは、Inno のロールバックを生き延びる。

### 実機で確かめた 4 通り(Windows 11、SAC off、Inno Setup 6.7.3)

| 状況 | 結果 |
|---|---|
| 新規 / `waired-agent.exe` が起動できない | exit **7**、`%ProgramFiles%\Waired` は**空**、サービス無し、レジストリ無し、managed-settings 無し |
| 新規 / 起動はするがサービスが上がらない | exit **7**、同上(置いた `waired-agent.exe` も消える) |
| 新規 / 正常 | exit 0、**インストーラが返った時点でサービスが Running**、managed-settings 書き込み済み |
| 更新 / 新しい agent のサービスが上がらない | exit **7**、サービスは Running のまま、`waired-agent.exe` は**バイト一致で元のまま**、managed-settings 無変更 |

### おまけ: SAC は「その場でコンパイルした未署名 exe」を拒否する

2026-09-03、Windows 11 Pro(SAC 有効)で、ビルドしたばかりの
`WairedSetup-*.exe` **そのもの**が拒否された(CodeIntegrity 3077 + 3033 +
3118 Smart App Control Block Details)。GUI インストーラが起動すらしないので、
この機では機能検証が回せない。判定はファイル単位で時間とともに変わるので、
**方針の拒否は実機で再現できない** — 壊したペイロード(起動できないファイル、
`where.exe`)で同じ形を作るしかない。`install.ps1` の #1087 契約テストと同じ手口。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1181
- docs/decisions/20260829/1730-installer-refuses-programs-that-cannot-run.md
- docs/knowledges/20260829/1740-sac-verdict-is-per-file-and-moves.md
- https://github.com/jrsoftware/issrc (tag `is-6_7_3`)
