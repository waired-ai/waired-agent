# pwsh 7 の PSModulePath が Windows PowerShell 5.1 の子プロセスを壊す (20260727 03:36)

## Issue

`waired runtimes install ollama` が Windows 実機で毎回失敗し、報告される
エラーは `ollama install: exit status 1` だけだった。実際の原因は
`Get-AuthenticodeSignature ... CommandNotFoundException`。

Go 側 (`cmd/waired/runtimes_install_windows.go`) が `powershell`
(= Windows PowerShell 5.1) を `cmd.Env` 未設定で起動していたため、子プロセスが
親の環境をそのまま継承していた。サポートされたインストール経路では
`waired init` が昇格済みの **PowerShell 7** セッション配下で走るので、
継承された `PSModulePath` の先頭は `C:\Program Files\PowerShell\7\Modules`
になる。

## Learnings

- **pwsh 7 と 5.1 は同名モジュールの互換性のないコピーを別々に持つ。**
  5.1 が pwsh 7 側の `Microsoft.PowerShell.Security` を自動ロードしようと
  すると、その `Security.types.ps1xml` が 5.1 組み込みの型ファイルと衝突し
  `FormatXmlUpdateException: ... "AuditToString" is already present` で落ちる。
  結果として `Get-AuthenticodeSignature` が永久にロードできない。
- **`Import-Module Microsoft.PowerShell.Security` は回避策にならない。**
  明示 import も同じ経路で同じ失敗をする。直すべきは環境変数そのもの。
- **`PSModulePath` を「書き換える」より「消す」方が正しい。** 変数が無ければ
  5.1 は `$PSHOME` とレジストリから正しいパスを自分で再構築する。
  → `internal/platform/pwsh` の `ChildEnv` は該当エントリを落とすだけ。
- **パス判別のコツ**: pwsh 7 側は `...\PowerShell\...`、5.1 側は
  `...\WindowsPowerShell\...`。`[\\/]PowerShell[\\/]` は後者にマッチしない
  (直前が区切り文字ではなく `Windows` のため) ので、そのまま篩に使える。
- **`$ErrorActionPreference = 'Stop'` が被害を拡大する。** スクリプト側
  (`scripts/install/ollama-windows.ps1`) の設定により、ロード失敗が
  terminating error に昇格してスクリプト全体が exit 1 で終わる。
- **なぜ CI で出なかったか**: per-PR の Windows レグ (`installtest.yml`) は
  `-WithInference` を渡さないので Ollama を一切インストールしない。nightly の
  self-hosted レグはインストールするが、ホストが使い回しで既に Ollama が
  入っているため base-install 分岐 (= `Verify-Signature`) に到達しない。
  どちらのワークフローも `shell: pwsh` なので、境界自体は再現し得た。
- Go から Windows PowerShell を起動している箇所は 4 つあり、**いずれも
  `cmd.Env` を設定していなかった**: Ollama インストーラ、`waired update` の
  インストーラ再実行 (`cmd/waired/update_client.go`)、
  `internal/platform/proclist`、`internal/platform/logdump`。
- 退避策として `Verify-Signature` に
  `X509Certificate2::CreateFromSignedFile` + `X509Chain` のフォールバックを
  入れたが、AMSI (`scripts/dev/amsi-scan.ps1`, Defender ライブ) では clean。
  ただしこの経路はファイルのハッシュ照合をしないので、cmdlet 版より弱いことを
  `Write-Warning` で明示している。

## Refs
- internal/platform/pwsh/pwsh.go (`ChildEnv` / `Env`)
- scripts/install/ollama-windows.ps1 (Desktop エディション時の PSModulePath 修復、`Verify-Signature`)
- https://github.com/waired-ai/waired-agent/issues/178
- https://github.com/waired-ai/waired-agent/pull/221
