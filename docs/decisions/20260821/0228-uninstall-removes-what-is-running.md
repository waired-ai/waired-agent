---
status: accepted
---

# アンインストールは動いているものも消す (20260821 02:28)

## Status
Accepted

## Context

waired-agent#793 の後半は、オーナー裁定を求めて open のままだった項目である。
rc9 の実機検証 (L37-B) は、`waired.exe` を**確実にロックしている**別プロセスがある
状態で `uninstall.ps1 -Clean -Yes` を回した:

- ロックは本物だった。直接 `Remove-Item waired.exe` は "Access to the path ... is
  denied" で失敗する状態を先に確認している。
- それでも uninstall は **exit 0 で完走**し、`Program Files\Waired` は完全に消え、
  ロック保持者のプロセスも実行後には居なくなっていた。
- 機構: uninstaller がまずサービスを止める → デーモンが消えたので対話中の
  `waired init` が自分で終了する → ロックが解放される → 削除が成功する。
- ただし**それを言うメッセージは 1 行も出ていない**。

#660 のチェックリストは「ロック下では非ゼロで終わり、残ったパスとロック保持者を
名指しする」を期待していたので、期待と挙動が食い違っていた。

## Decision

**アンインストールは、Waired が動いていても Waired を消す。**(オーナー裁定、
2026-08-21、waired-agent#793 の triage 要請に対して)

そのため、削除の**前に** `InstallDir` から走っているプロセスを列挙して停止し、
どれを止めたかを 1 行ずつ出す (`Stop-InstallDirProcesses`)。副作用として偶然
ロックが外れるのに頼るのをやめる。

`Assert-Removed` (#660) はそのまま**最後の砦**として残す。これが消えるのではなく、
役割が変わる: 「自分では外せなかったロック」だけが到達する場所になり、そこでは
従来どおり非ゼロで死に、ロック保持者を名指しする。

## Consequences

- 実行中の `waired.exe` / `waired-tray.exe` / `waired-agent.exe` は、アンインストール
  時に予告のうえ停止される。これは今までも実質そうなっていた (デーモン停止の
  巻き添えで終了していた) が、今後は意図された動作として記録され、可視化される。
- 第三者プロセス (エディタ、アンチウイルス、cwd が `InstallDir` のシェル) が
  握っている場合は、こちらでは止めない。`Assert-Removed` が名指しして失敗させる。
  「Waired を消す」であって「他人のプロセスを殺す」ではない。
- **Windows 限定**。POSIX では実行中のバイナリを unlink できるので、この問題は
  構造的に存在しない (CLAUDE.md §Cross-OS parity の「他の 2 OS を確認して、
  同じ PR で変えるか、なぜ不要かを述べる」に対する答え = 不要)。
- #660 のチェックリストの当該項目は、この裁定に合わせて読み替える。期待は
  「非ゼロ + 名指し」ではなく「消える。消せないものだけが非ゼロ + 名指し」。

## Refs

- https://github.com/waired-ai/waired-agent/issues/793
- https://github.com/waired-ai/waired-agent/issues/660
- packaging/install/uninstall.ps1 (`Stop-InstallDirProcesses`, `Assert-Removed`)
