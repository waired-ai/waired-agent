---
status: accepted
---

# 「再起動して」の終了コード 17 は、各 supervisor に報告する。偽装しない (20260812 03:10)

## Status

Accepted。waired-agent#684。

## Context

`service.RestartRequestedExitCode`（17）はエージェントが supervisor に再起動を頼む手段。
Linux の unit だけが `SuccessExitStatus=17` / `RestartForceExitStatus=17` で解釈していた。

**issue の前提のうち darwin の分は誤りだった。** plist は
`KeepAlive{SuccessfulExit=false}` なので**非ゼロ終了なら再起動する**し、再起動
スケジューラも `//go:build linux || darwin` だった。つまり macOS でも 17 は届いており、
欠けていたのは「クラッシュと区別が付かない」ことと「どこにもそう書いていない」こと。

**Windows は実際に壊れていた。** `internal/management/restart_windows.go` が
サービスプロセスの中から `os.Exit(1)` を呼んでいたため、SCM から見て**ハードクラッシュ**。
`svcHandler.Execute` は `(false, 0)` か `(false, 1)` しか返さず
`ServiceSpecificExitCode` を一度も設定しないので、**17 は SCM に一切届かない**。
graceful shutdown を飛ばし、5 分ウィンドウ内の復旧スロットを 1 つ消費し、
Event Log に何も残らない。

この非対称は既に 1 件のバグを隠している（#656 / #670）。「Linux は緑、他 2 つは壊れている」は
CLAUDE.md §Cross-OS parity が存在する理由そのもの（waired#746–#758）。

## Decision

### 1. Windows は 17 を SCM に報告する（in-process 再起動は採らない）

`RequestRestart` が SCM ハンドラの ctx を cancel し、`run()` が正常復帰し、
`Execute` が `(true, RestartRequestedExitCode)` を返す。x/sys がこれを
`Win32ExitCode = ERROR_SERVICE_SPECIFIC_ERROR` + `ServiceSpecificExitCode = 17` に写す。
非ゼロ停止なので、`applyRecoveryConfig` が入れた復旧アクションが再起動を担う
（#315 の `SetRecoveryActionsOnNonCrashFailures(true)` が「綺麗な STOPPED も失敗として数える」を
成立させている）。

**採らなかった案: プロセス内で自分を再起動する**（re-exec / 監視シム）。3 OS すべてに
第 2 の supervisor を持ち込むことになり、SCM・launchd・systemd がそれぞれ既に
やっている仕事を二重化する。得られるのは darwin の区別可能性だけで、割に合わない。

### 2. darwin は区別できない。それを**書いて**、テストで留める

launchd の `KeepAlive` は条件の dict（`SuccessfulExit` / `Crashed` / `NetworkState` /
`PathState` / `OtherJobEnabled`）で、**exit code 別のキーが無い**。
「17 とクラッシュで再起動し、exit 0 では止まる」は
`KeepAlive{SuccessfulExit=false, Crashed=true}` が最良で、それは既にそうなっている。
区別を持ち込むには判断を launchd の外へ出すしかない（上記で退けた案）。

なので **`RestartOnExitFor(goos)` に `Named` を持たせ、darwin だけ false** と記録する。
既存の plist テストは `<key>Crashed</key>` と `<false/>` を**独立した部分文字列**として
見ており値の入れ替わりを捕まえられないので、キー→値の対応を pin するテストを足した。

### 3. 判定は untagged な `(GOOS) -> plan`、機構は per-OS ファイル

`StartHintFor` と同じ形。CLAUDE.md §Test discipline が既定と定めており、
**必須チェックは Linux の `unit tests` だけ**なので、3 OS の主張が required レーンで
検証される唯一の形でもある。

per-OS の `osRequestRestart` は**フェイクを挟まない**。どれもこのプロセスを終わらせる関数で、
その後どうなるかは supervisor の性質。順序の決定（意図を記録してから機構を呼ぶ）だけを
`requestRestart(mechanism)` に切り出してテストする。

### 4. 再起動機構は 1 か所に集約する。ただし import で結合はしない

`internal/management` が持っていた per-OS の複製を削除した。**2 つの複製が実際に乖離していた**のが
この issue の Windows 側の中身であり、意図フラグは SCM ハンドラも読む必要がある
（`main.go` の exit には SCM 経路が構造的に到達しない）。

**最初 `DefaultRestartScheduler = service.RequestRestart` と書いて CI に止められた。**
`scripts/ci/routing-sentinel-paths-guard.sh` が、その import によって
`internal/platform/service` と、その依存の `internal/deauth` / `internal/controlclient` /
`internal/devicekeys` / `internal/identity` が **routing harness の依存閉包に入った**ことを検出した。
ガードは「ワークフローの paths を広げるか ALLOW に足すか、意識的に選べ」と言うが、
**正しい答えはどちらでもなく「結合しない」**だった。管理 API がプロセスの再起動方法を知る必要はない。

なので `DefaultRestartScheduler` ごと削除し、`CatalogConfig.RestartScheduler` の
既存シームだけを使う。未配線（nil）は配線漏れなので、**202 を返さず 500 で断る** —
「起きない再起動」を適用済みとして報告するのが、置き換えた側の失敗だった。
配線するのは `cmd/waired-agent` で、そこは元から両方を知っている。

## Consequences

- Windows のモデル切替が **graceful shutdown を経る**ようになり、復旧スロットを
  クラッシュとして消費しなくなる。`sc queryex` が 17 を名指しできる。
- **挙動変更**: `RestartScheduler` 未配線のデーモンで `/preferred-model` の再起動経路が
  500 を返す（以前はパッケージ内蔵の既定で再起動しようとした）。実デーモンは
  `cmd/waired-agent` が必ず配線しており、未配線なのはテストの組み立てだけ。
- **`internal/platform/servicediag` は未対応**。`waired doctor` は依然として
  「再起動を頼んだ終了」を失敗として説明しうる。診断系は別レーンの領域なので触っていない。
- `scripts/dev/installtest-windows.ps1` に実機アサートを足す余地がある
  （`sc.exe qfailure` は既に読んでいる）。同ファイルを触る PR が同時に走っているので同梱しない。
- **実機未確認**。Windows / macOS でのモデル切替往復は 3 台の占有キュー待ち。

## Refs
- https://github.com/waired-ai/waired-agent/issues/684
- https://github.com/waired-ai/waired-agent/issues/656
- https://github.com/waired-ai/waired-agent/pull/670
- https://github.com/waired-ai/waired-agent/issues/347
- https://github.com/waired-ai/waired/issues/315
