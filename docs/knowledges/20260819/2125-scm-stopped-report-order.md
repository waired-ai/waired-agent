# SCM への STOPPED 報告は「最初の1回」しか効かない (20260819 21:25)

## Issue

Windows で「再起動してほしい」終了 (`RestartRequestedExitCode` = 17) を
出しても SCM がサービスを起動し直さない (#855)。復旧アクション3本も
`FAILURE_ACTIONS_ON_NONCRASH_FAILURES: TRUE` (#315) も入っており、
イベントログにも exit 17 を報告した旨が出ているのに発火しなかった。

## Learnings

`golang.org/x/sys/windows/svc` の `serviceMain` は
`ec := exitCode{isSvcSpecific: true, errno: 0}` で始まり、ハンドラが
status チャネルに送った `svc.Status` は毎回 `updateStatus(&c, &ec)` を
通る。この時点では `ec.errno == 0` なので `Win32ExitCode = NO_ERROR` に
なる。終了コードが `ec` に入るのは **`Execute` が return した後**で、
ループを抜けた先の `updateStatus(&Status{State: Stopped}, &ec)` だけが
`ERROR_SERVICE_SPECIFIC_ERROR` (1066) + 17 を載せる。

つまり `Execute` の中から `status <- svc.Status{State: svc.Stopped}` を
送ると、SCM には
`SetServiceStatus(SERVICE_STOPPED, dwWin32ExitCode = 0)` として届く。

罠が3つある。

- `svc.Status` には `Win32ExitCode` フィールドが**ある**が、
  `updateStatus` はハンドラから来た値のそれを読まない。終了コードは
  `Execute` の戻り値からしか入らないので、送る側で埋めても効かない。
- SCM は**最初に見た STOPPED** でサービスを確定させる。復旧アクションが
  積まれるのは「STOPPED を報告せずに死んだ」か「STOPPED を報告したが
  `dwWin32ExitCode != 0`」のときだけなので、先頭のゼロが復旧対象を
  消してしまう。
- 2度目の報告は届かない。STOPPED を報告した時点でステータスハンドルは
  無効になるため、**`sc queryex` にも 0 のまま残る**。逆に言えば
  `sc queryex` の `WIN32_EXIT_CODE` は決め手として使える。

実測 (同一バイナリ・同一復旧設定で、この push の有無だけを変えた
使い捨てサービス):

```
push-then-return : WIN32_EXIT_CODE 0 (0x0)     / SERVICE_EXIT_CODE 0    75秒待っても停止のまま
return-only      : WIN32_EXIT_CODE 1066 (0x42a)/ SERVICE_EXIT_CODE 17   停止の5秒後に SCM が再起動
```

`CreateService` に渡した引数は**プロセス引数** (ImagePath 側) になる。
`Execute` が受け取る `args` は `StartService` の引数で、既定では
`[サービス名]` だけ。アームを引数で切り替えるプローブを書くときに
ここで一度空振りした。

## Refs

- https://github.com/waired-ai/waired-agent/issues/855
- https://github.com/waired-ai/waired-agent/issues/684
- https://github.com/waired-ai/waired-agent/issues/315
- `internal/platform/service/service_windows.go` (`svcHandler.Execute`)
- `golang.org/x/sys/windows/svc/service.go` (`serviceMain` / `updateStatus`)
