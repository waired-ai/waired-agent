# 手元の Linux で通り、他の OS の CI でだけ落ちる型と、hosted runner で測れること (20260921 21:20)

## Issue

Linux の開発用コンピュータで `go build` / `go test` / `go vet` が全部通っても、CI の
Windows / darwin のジョブやビルドタグ付きのジョブでだけ落ちる型が繰り返し出た。
push の前に手元で潰せるものと、CI でしか見えないものを分けて記録する。あわせて、
「CI では観測できない」と書く前に引けるよう、GitHub の hosted runner で実際に
測れたこと(デスクトップセッション、UAC の昇格、SAC)をまとめる。

セッションのメモリに在った知見を、オーナーの指示(2026-09-21、
waired-ai/waired-ai#12)でリポジトリへ移した。

## Learnings

### 1. ビルドタグ付きのファイルは、既定の go vet / go test / golangci-lint が見ない

いつ読むか: シンボル(型・定数・関数・ヘッダ名)を削除・改名したとき。製品の文字列や
managed settings の中身を変えたとき。

`go vet ./...` / `go test ./...` / `golangci-lint run` は、ビルドタグ付きのファイルを
コンパイルしない。手元で全部通して push しても、タグ付きのジョブだけ CI で落ちる。

実例(2026-09-04、waired-agent#1184): `internal/runtime/state` から
`ClaudeRouteAnthropic` を削除した。手元は全部通ったが、CI の lint と routing sentinel が
`vet: internal/e2e/integration/harness.go:409:35: undefined: state.ClaudeRouteAnthropic`
で落ちた。`internal/e2e/integration` は `//go:build integration`。同じ push で
`scripts/dev/installtest-windows.ps1` の要件アサート(Stop フックの形)も落ち、CI を
1 往復使った。

このリポジトリのビルドタグ(`find . -name '*.go' -exec grep -h '^//go:build' {} +` で
数えられる): `integration` / `e2e` / `e2e,gpu` / `testharness` / `prod` / OS 別。

シンボルを削除・改名したら、push の前に:

```sh
for t in integration e2e testharness prod; do go vet -tags "$t" ./...; done
go vet -tags "e2e,gpu" ./...
golangci-lint run --build-tags integration
```

- `grep -rn '<消したシンボル>' .` を先に打つのでも捕まる。CI が見ているのはソース
  全体で、既定のビルドが見るのはその一部。
- PowerShell / bash のハーネス(`scripts/dev/installtest-*.ps1|sh`)は Go のツールが
  一切見ない。製品の文字列や managed settings の中身を変えたら、別に grep する。
- push 前のほかの確認(`go test ./...`、`make verify-cross`、`scripts/ci/` のガードを
  回す `make ci-lint-local`)は CONTRIBUTING.md §Building and testing に在る。タグ付きの
  vet はその一覧に無く(`-tags prod` だけ)、CI の lint ジョブの「build-tag smoke」で
  初めて落ちる(2026-09-21 時点)。CI がモジュール全体をタグごとに vet する決定は
  `docs/decisions/20260829/1500-a-harness-reports-what-it-observed.md` §3。

### 2. 手元の go build / go test は、自分の OS の実装しか見ない

いつ読むか: 新規ファイルにビルドタグを書いたとき。OS 別の定数や分岐を変えたとき。
OS 別のフックを持つパッケージのテストを書くとき。

手元の `go build ./...` と `go test ./...` は、GOOS=linux の実装しかビルドも実行も
しない。他の OS の分は、CI の `install test (windows)` や `unit tests (darwin)` で
初めて落ちる。起きた形は 3 つ。

**2a. 隣のファイルから写したビルドタグ。** 参照する側が untagged だと、その OS で
だけ未定義になる(waired-agent#1298、`undefined: download.HFFileLister`)。写した元の
`internal/download/hf.go` が `linux || darwin` なのは `syscall.SysProcAttr` を使う
からで、足した `hf_files.go` は net/http と os だけだった。タグを付ける理由は、
ファイルの中身で決まる。

**2b. 他の OS のタグ付きテストが固定している定数。** `internal/hardware` のように
`_linux.go` / `_windows.go` / `_darwin.go` が同じ関数名を実装しているパッケージで、
ある OS の定数や分岐を変えると、その OS 版を固定しているテストが CI で落ちる。

**2c. 本物の OS 別フックを残したテストは、走っているホストから答える**
(waired-agent#1462、#1479 で 2 度)。`NewProfiler` に `WithUMA` を渡さず本物の
`defaultUMA` を使うと、darwin の CI の runner は arm64 なので `UnifiedMemory=true` を
立て(Linux では立たない)、sysctl で答えて早期 return するので、共有の規則に到達
しない。手元で通り、CI で落ちる。逆向きに誤って通る形もある。darwin の
`integratedFromOS` は `GOARCH == "arm64"` だけを見て、どんなデバイスにも "integrated" と
答えるので、darwin の runner では「検出が効いていること」のアサートが、被験体を
消しても通る。

対処:

- 新規ファイルにビルドタグを書いたら、push の前に
  `for os in windows darwin linux; do GOOS=$os go build ./cmd/... ./internal/...; done`。
  `./...` 全体は、darwin で fyne.io/systray が cgo 無しで落ちるので、触った
  パッケージだけを指定する。
- OS 別の定数・分岐を変えたら、`GOOS=<os> GOARCH=<arch> go test -c -o <out>.test ./<pkg>/`
  でテストバイナリをクロスビルドする(コンパイルエラーはこれで出る)。その OS の
  ホストが使えるなら、バイナリを送って実行する。
- OS ごとに分かれたフックの中身を検証したいときは、スタブに置き換えない(規則を偽物に
  したら何も試せない)。中身を untagged な `(goos, facts) -> plan` に出す(CLAUDE.md
  §Test discipline)。実例は `internal/hardware/uma_common.go` の
  `applyUnifiedBudget(goos, p)` で、`defaultUMA` は
  `applyUnifiedBudget(runtime.GOOS, p)` の 1 行、テストは
  `WithUMA(func(_ ctx, p *Profile){ applyUnifiedBudget("linux", p) })` を注入する。
- アサートしたい軸以外の情報源は黙らせ(`WithIntegratedDetector` で unknown を
  返させる)、被験体を壊すとテストが落ちることを確かめる
  (`docs/knowledges/20260921/2100-tests-that-pass-without-testing.md` §1)。

### 3. darwin の unit test を実行できるのは CI だけ — OS とハードウェアの事実は注入する

いつ読むか: プロファイラなど、ホストの事実を読むコードのテストを書くとき。CI の
darwin のジョブが、runner の実機の数字(VRAM の量など)を含む失敗を出したとき。

Linux(WSL2 を含む)の開発用コンピュータからは、darwin の unit test を実行できない。
できるのは `GOOS=darwin go vet ./...` と `go build`(クロスコンパイルの型とビルドの
検査)まで。`make verify-cross` も darwin は vet しかしない。Windows には手段が在る
(`GOOS=windows go test -c` で作ったテストバイナリを Windows の下で実行する。WSL2 なら
同じコンピュータの Windows 側で実行できる)。darwin は、Go の入った macOS のホストに
ソースを送って ssh 越しに実行する以外に無い。だから、darwin の runner の実
ハードウェアの特性に依存するテストは、手元では原理的に検出できない。

実例(2026-08-13、PR #773): `unit tests (darwin)` と `(darwin, seeded host)` が落ちた。
失敗は `TestProfile_VRAMFreeIsFrozenAfterTheFirstReading: ollama budget = 5120, want
20000`。`5120` は macOS の runner の実際の `UsableVRAMMB`。プロファイラを組むテストで
OS 別の UMA のフックを塞いでいなかったため、darwin では `defaultUMA` が実機から
`UnifiedMemory=true` を埋め、統合メモリのホストは(設計どおり)空き VRAM の読みを
無視した。製品コードは正しく、テストが「規則の適用外のホスト」に、ディスクリート
GPU 用の規則を主張していた。§2c と同じ形。

- ハードウェアと OS に依存する事実は、テストに注入する(hermetic)。runner から
  読ませない。プロファイラを組むテストでは `WithGPU` だけでなく `WithUMA` も塞ぐ
  (`WithUMA(func(context.Context, *Profile) {})`)。`WithRAMAvailableAtInstall` も同型。
- CLAUDE.md §Test discipline の「A clean CI runner hides every dependency on the
  developer's machine」の裏面。自分のテストが「ホストが UMA でないこと」に依存する
  向きもある。
- 失敗の値が runner の実機の数字(VRAM の量など)なら、インフラの障害でも既知の失敗
  でもなく、自分のテストのホスト依存を疑う。

### 4. テストの前提を作る手段に、OS の前提が残る

いつ読むか: OS タグ付きのテストを 1 つも書いていないのに、Windows / macOS の CI で
だけ落ちたとき。ファイルの権限、testdata、OS のエラー文言、時刻の比較を使うテストを
書くとき。

push 前の `make verify-cross`(他の OS 向けの `go vet`)や `GOOS=windows go test -c` は
コンパイルまでしか見ないので、次の 4 つはどれも捕まらない。

**4a. 状態を誘発する手段が OS 依存。** `os.Chmod(dir, 0o000)` で「読めない
ディレクトリ」を作ったつもりが、Windows の `os.Chmod` は read-only 属性しか触らず、
traversal を拒否しない。`os.Stat` が ENOENT を返し、意図と逆の分岐が走った
(waired-agent#800)。判定は OS 非依存でも、誘発する手段が OS 依存だと、テストは
OS ごとに別の分岐を検査している。

- 状態そのものを注入する(`var xStat = os.Stat` の seam にエラーを差す)。実関数は
  present / absent など移植可能なケースで別に駆動する。
- このリポジトリの慣習は、chmod 系を `*_unix_test.go` に隔離する形
  (`cmd/waired/doctor_perm_unix_test.go`)。新規テストの前に、同種の既存テストの
  隔離を grep する。

**4b. testdata と `go:embed` は、Windows の CI で CRLF になる。** Windows の runner は
`core.autocrlf` で checkout を CRLF にする。`.gitattributes` が守っているのは
`packaging/install/testdata/**` だけ(2026-09-21 時点)。

- `testdata/*.log` を `os.ReadFile` して `strings.NewReplacer("...MiB\n", "")` で 1 行
  消す表テストが、Windows でだけ行を消せずに落ちた(waired-agent#1384)。フィクスチャを
  読む helper で `strings.ReplaceAll(s, "\r\n", "\n")` してから使う。パーサ自体も入力を
  正規化する(Windows の実機の engine.log も CRLF)。
- `go:embed` のテンプレートも同じで、行末を `$` で固定した正規表現が外れる。
  `docs/knowledges/20260912/1230-embedded-templates-are-crlf-on-windows.md`。
- 再現は Linux で書ける。テストの中で `\n` を `\r\n` に置換して流す。

**4c. OS のエラー文言を手書きのフィクスチャにしない。** 製品が OS のエラー文言を
部分文字列で照合しているなら、テストの中で OS 自身に生成させる(同じアドレスに 2 回
`net.Listen` すると 3 OS とも失敗し、その `err.Error()` が本物の文言)。手書きは
書いた日に古び、外れたときの失敗は無言になる。
`docs/knowledges/20260828/1900-engine-failure-detail-carries-the-log-tail.md` §3b。
補足: CI は `-v` を渡さないので、合格時の `t.Logf` は捨てられる。逐語は doc コメントと
knowledge note に置き、テストは壊れたときに落ちる役目にする。

**4d. 2 点の時刻の順序を `time.Now()` の比較で決めない。** Windows の Go は単調時計も
既定で 15.6 ms の粒度で、マイクロ秒差の 2 回の読みが同値になる。macOS の CI でも
同じ形で落ちた。Linux では再現せず、直しを戻しても落ちない。順序の判定は
`atomic.Uint64` のカウンタで書く(`OllamaAdapter.ProcessGeneration` の形)。時間の
長さ(経過・予算・タイムアウト)は時計でよい。
`docs/knowledges/20260912/1400-clock-resolution-is-an-os-property.md`。

3 OS 版のハーネスの 1 つだけが条件を忘れている型は、
`docs/knowledges/20260921/2100-tests-that-pass-without-testing.md` §10。タイミングの
寸法は `docs/knowledges/20260921/2110-timing-and-concurrency-test-shapes.md`。

### 5. hosted runner には、デスクトップセッションが在る

いつ読むか: 「CI では観測できない」と書く前。デスクトップ・UAC の昇格・SAC の 3 件とも、
最初は「原理的に不可能」と書いて決定記録・ハーネスのコメント・issue に広げ、後から
実測で訂正した。先に実際の runner で測る。

- **windows-latest**(Windows Server 2025): `Win32_ComputerSystem.UserName` が
  `…\runneradmin` を返す = コンソールユーザが居る。`HKEY_USERS\<SID>` も引ける。
- **macos-14**: `launchctl print gui/$(id -u)` が成功し、`open` した Cocoa アプリが
  実際に起動する = Aqua セッションが在る。
- だから、installer が書いた HKCU の Run 値と、tray が自分で登録した LaunchAgent を
  CI でアサートできる。
- `-NonInteractive` は別の軸。install.ps1 の `Test-InteractiveStdin` は false のまま
  なので、tray の起動はスキップされ、autostart の登録だけが行われる。
- CI が届かないのは、コンソールユーザが全く居ないホスト(ログオン画面のままの
  サーバ、無人のコンピュータへの ssh 越しのインストール)。その分岐は、純関数と
  表テストで持つ。

### 6. UAC: トークンのフィルタは、対話ログオンでしか起きない

いつ読むか: Windows で「昇格しているか」で分岐する挙動を CI で試すとき。

| 経路 | トークン | ハーネスの関数 |
|---|---|---|
| `schtasks` + 保存パスワード = バッチログオン | フィルタされない(管理者なら完全なトークン)。ウィンドウステーション無し。`/RL HIGHEST` の有無は無関係 | `Invoke-AsStandardUser`(標準ユーザの拒否側) |
| `runas /trustlevel:0x20000` = SAFER の制限 | ファイルアクセスはフィルタ済みの管理者と同類。ただし `TokenElevation` は昇格済みと答える | `Invoke-AsBasicToken` |
| `Start-Process -Credential` = `CreateProcessWithLogonW` = 対話ログオン | RID 500 でない第 2 の管理者なら、フィルタ済み(ElevationType=3)。そこから `-Verb RunAs` が通る | `Invoke-AsInteractiveUser` |

- 昇格しているかで分岐する挙動を試すなら `Invoke-AsInteractiveUser`。
  `runas /trustlevel` は代わりにならない(waired-agent#1419 で実測)。
- 最初の誤りは、失敗した 2 つの経路から「不可能」を推論し、3 つ目の経路を試して
  いなかったこと。決定の本体、`lpDesktop = NULL` の条件(明示すると子が
  `0xC0000142` で終わり、閉じる人のいないダイアログを出したまま止まるのでハングに
  見える)、対照に `cmd.exe` が使えない理由(user32 を読まない)、
  `ConsentPromptBehaviorAdmin=0` による範囲(通るのは install.ps1 の Phase 1 →
  Phase 2 の受け渡しの機構で、人が「はい」を押す動作ではない)は
  `docs/decisions/20260823/1248-granted-uac-elevation-is-testable.md` に在る。3 OS の
  権限の形の行列と、実測で潰れた 2 経路は
  `docs/decisions/20260822/1924-installtest-runs-both-privilege-shapes.md`。
- runner 自身のトークンが使えないのは、RID 500 で Admin Approval Mode が off だから。
  hosted runner だからではない。「GitHub の Windows runner は UAC が無効」という Web 上の
  記述は誤りで、実測は `EnableLUA=1`。レジストリを直接読む。
- 作りたてのアカウントの子プロセスは、ジョブの環境を継ぐ。`TEMP` / `LOCALAPPDATA` /
  `PSModulePath` を直さないと、Windows PowerShell 5.1 の子が `Get-FileHash` を解決
  できず、SAFER のトークンと同じ症状で終わる。直しは
  `scripts/dev/installtest-windows.ps1` の `New-ItInteractiveEnv` と、`Invoke-As*` の
  上のコメントに在る。`PSModulePath` の機構は
  `docs/knowledges/20260727/0336-pwsh7-psmodulepath-poisons-ps51.md`。

### 7. SAC(Smart App Control)

いつ読むか: 未署名のバイナリや SAC の判定を CI で試せるかを考えるとき。

- 署名の要件は、Microsoft が配布する署名済みの監査ポリシー
  `SmartAppControlAuditNoISG.bin` で測れる(再起動不要、SAC の無い Server でも適用
  できる)。`installtest-windows.ps1 -SacAudit`。
  `docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md`。
- 評判の判定は、監査モードのカスタムポリシーで「許可されなかった側」だけ読める。
  強制の経路は実機だけ。
  `docs/decisions/20260904/0340-the-reputation-verdict-is-readable-in-audit-mode.md`、
  `docs/knowledges/20260904/0300-the-reputation-verdict-is-in-event-3118.md`。

### 8. Windows の hosted runner での診断の道具と、小さな罠

いつ読むか: Windows の CI のジョブが、原因の分からないハングや終了コードで落ちたとき。

- `HKLM\SYSTEM\CurrentControlSet\Control\Windows\ErrorMode = 2` でハードエラーの
  ダイアログを抑止すると、ローダの失敗がハングではなく終了コードとして出る
  (1248 の決定記録の Consequences)。
- カーネルデバッガ無しでローダのスナップを採る(これまでリポジトリに記録が
  無かった): 子を `CREATE_SUSPENDED` で作り、
  `cdb -p <pid> -G -c "!gflag +sls; ~0m; g; g; g; q"`。cdb は windows-latest に同梱
  されている(`C:\Program Files (x86)\Windows Kits\10\Debuggers\x64\`)。失敗した DLL を
  名指しさせられる。
- 落ちているジョブには、環境を丸ごと出す診断(版、各パスとその存在、`PSModulePath`、
  `Get-FileHash` が解決するか)を持たせる。原因を 1 つずつ CI で試すより速い。
  `Test-Path` は空文字で throw するので、ガードする。
- 否定の実験ほど、固定している変数を先に疑う。`lpDesktop` を全ラウンドで固定して
  いたので、「ウィンドウステーションは原因でない」という反証が丸ごと無効だった
  (1248 の決定記録の Consequences)。
- PowerShell の比較: `0 -ne $false` は False、`1 -eq $true` は True。0 になり得る値の
  番兵は `$null` にし、真偽は `($v -is [bool]) -and $v` と型を見る。
- cmd は、リダイレクト先がロックされていると、コマンドを飛ばして `ERRORLEVEL` を 0 の
  まま残す。マーカーの有無ではなく、期待する終了コードで判定する。

## Refs

- waired-ai/waired-ai#12(この移行の指示)
- waired-agent#800, #997, #1184, #1298, #1384, #1419, #1462, #1479、PR #773
- CLAUDE.md §Test discipline、§Cross-OS parity、CONTRIBUTING.md §Building and testing
- `docs/decisions/20260822/1924-installtest-runs-both-privilege-shapes.md`
- `docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md`
- `docs/decisions/20260823/1248-granted-uac-elevation-is-testable.md`
- `docs/decisions/20260829/1500-a-harness-reports-what-it-observed.md`
- `docs/decisions/20260904/0340-the-reputation-verdict-is-readable-in-audit-mode.md`
- `docs/knowledges/20260727/0336-pwsh7-psmodulepath-poisons-ps51.md`
- `docs/knowledges/20260828/1900-engine-failure-detail-carries-the-log-tail.md`
- `docs/knowledges/20260904/0300-the-reputation-verdict-is-in-event-3118.md`
- `docs/knowledges/20260912/1230-embedded-templates-are-crlf-on-windows.md`
- `docs/knowledges/20260912/1400-clock-resolution-is-an-os-property.md`
- `docs/knowledges/20260921/2100-tests-that-pass-without-testing.md`
- `docs/knowledges/20260921/2110-timing-and-concurrency-test-shapes.md`
- `scripts/dev/installtest-windows.ps1`(`Invoke-As*`、`New-ItInteractiveEnv`、`-SacAudit`)
