# Windows の昇格まわりで実機確認した PowerShell の挙動 (20260801 22:02)

## Issue

#314（昇格した子コンソールを閉じると `-1073741510` だけが出る）を直す際、
「たぶんこう動くはず」で書くと**コンパイルも通り、レビューでも正しく読め、
しかし一度も発火しない**コードになる箇所が複数あった。Windows PowerShell
5.1 (5.1.26100) と pwsh 7.6.3 の両方で実機確認した結果を残す。

## Learnings

### 1. `Start-Process -Verb RunAs` は Win32Exception を握り潰す

UAC を拒否したときのエラーコード 1223 (`ERROR_CANCELLED`) は**取得できない**。
`StartProcessCommand` が `Win32Exception` を捕まえ、素の
`InvalidOperationException` を投げ直すため、catch 側に届く時点で情報が消えている。

```
outer type   : System.InvalidOperationException
fqeid        : InvalidOperationException,Microsoft.PowerShell.Commands.StartProcessCommand
InnerException : $null          <- 5.1 / 7 ともに null
NativeErrorCode: 存在しない
Message      : OS ロケール依存（日本語環境では日本語）
```

つまり以下はすべて「書けるが動かない」:

- `catch [System.ComponentModel.Win32Exception]` … 型が違うので発火しない
- `$_.Exception.InnerException.NativeErrorCode` を辿る … InnerException が null
- `Message` の文字列マッチ … ローカライズされるので不可

**結論**: 原因を特定せず「よくある原因は UAC で「いいえ」を選んだこと」と述べ、
OS のメッセージはそのまま引用する。install.ps1 のコメントにも
「typed catch や InnerException walk に"改善"するな」と明記した。

なお `-ErrorAction` は効かない（`ThrowTerminatingError` なので try/catch のみ）。

### 2. `[uint32]` への負値キャストは throw する

`$proc.ExitCode` は `System.Int32` で、NTSTATUS は負値で来る
(`-1073741510`)。`[uint32](-1073741510)` は**チェック付き変換なので例外**。
正規化は不要で、`'{0:X8}' -f [int]$code` がそのまま `C000013A` を返す
（Int32 が 2 の補数のビットパターンを出力するため）。`-band 0xFFFFFFFF` も不要。

### 3. Start-Transcript が開いている最中のログは Get-Content でしか読めない

| 読み方 | 結果 |
|---|---|
| `[System.IO.File]::ReadAllText` | **失敗**（共有違反。`FileShare.Read` を要求するため） |
| `Get-Content` / `Get-Content -Tail` | OK（`FileShare.ReadWrite`） |
| `[System.IO.File]::Open(..., FileShare::ReadWrite)` + StreamReader | OK（tail -f 相当が書ける） |

install.ps1 は state file の読み書きに `File.ReadAllText`/`WriteAllText` を
使っているので、同じ流儀でトランスクリプトを読もうとすると必ず踏む。

### 4. `-Wait` なしで ExitCode を読むには `.Handle` を先に触る

親が子のトランスクリプトをライブ表示するには `-Wait` を外して自前で待つ必要が
あるが、`Start-Process -PassThru` が返す Process は**ハンドルを開いたまま
持っている間しか ExitCode を読めない**。

```powershell
$proc = Start-Process ... -Verb RunAs -PassThru   # -Wait なし
$null = $proc.Handle                              # ここでハンドルをキャッシュ
# ... HasExited を見ながらログを追従 ...
$proc.WaitForExit()
$proc.ExitCode                                    # 読める
```

medium IL の親から high IL の子の ExitCode を読むのは問題ない
（ShellExecuteEx のハンドルに `PROCESS_QUERY_LIMITED_INFORMATION` が付く）。

### 5. トランスクリプトのヘッダは「行内容」で判定できない

5.1 のヘッダには `ユーザー名:` `RunAs ユーザー:` `コンピューター:` が入り、
**ラベルがローカライズされる**ので英語文字列でのマッチは不可。ヘッダ・本文・
フッタは `*` だけの行（`^\*{5,}$`）で区切られているので、区切り行を数えて
「2 本目以降 3 本目未満」だけを本文とみなすのがロケール非依存で確実。

これを怠ると、親コンソールに昇格した管理者（invoking user とは別人でありうる）
のユーザー名が出る。`Protect-PII` は自プロセスの `USERPROFILE`/`USERNAME` しか
マスクしないので救えない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/314
- https://github.com/waired-ai/waired-agent/pull/344
- packaging/install/install.ps1 — `Get-ExitCodeReason` / `Watch-ElevatedConsole` / `Invoke-SelfElevate`
- scripts/dev/installtest-pwsh.ps1 — `Stub-StartProcess`（実物と同じ形の例外を投げることで 1. の退行を検出する）
