---
status: accepted
---

# 許可される UAC 昇格は hosted runner で実行できる — `CreateProcessWithLogonW` + `lpDesktop=NULL` (20260823 12:48)

## Status

Accepted。20260822 19:24 の
`1924-installtest-runs-both-privilege-shapes.md` の「昇格が『許可される』経路は
GitHub-hosted ランナーでは自動化できない」という結論を**この 1 点についてだけ**
改める。同記録の本体（3 OS の root/非 root 起動形の行列、拒否アーム、Phase 2
アーム、および実測で潰れた 2 経路の記述）は**すべて有効なまま**である。

## Context

1924 は 2 つの経路を実測で潰したうえで、そこから「不可能」を導いていた。

- `schtasks` + 保存パスワード = **バッチログオン**なので UAC のトークン分割が
  起きない（run 32567682964）— **この測定は今も正しい**。
- `runas /trustlevel:0x20000` の SAFER 制限トークンでは `Get-FileHash` が動かず
  SHA-256 照合で死ぬ（run 32568318138）— **これも今も正しい**。

誤りは**その次の一文**、2 つの失敗から不可能性を推論した部分である。3 つ目の
経路が試されていなかった。

## Decision

**`CreateProcessWithLogonW`（`runas.exe` が使っている API）を
`STARTUPINFO.lpDesktop = NULL` で呼ぶ。** これで RID 500 でない第 2 管理者が
**対話ログオン**で起動し、LSA が linked token を作るのでトークンは UAC
フィルタ済みになる。そこから `Start-Process -Verb RunAs` が通る。

windows-latest（Windows Server 2025）で**連続 2 回**実測
（run 32593631629 / 32593788357）:

```
親  : whoami=...\waired-uacprobe  IsAdmin=False              (Medium)
孫  : elevated_IsAdmin=True       integrity=S-1-16-12288     (High)
```

`installtest-windows.ps1` はこれを `-Contract` レグのアーム 1b として実行し、
`install.ps1` の Phase 1 → Phase 2 受け渡しを実物で通す。

**範囲**: `ConsentPromptBehaviorAdmin=0` なので AppInfo は無プロンプトで承認
する。検証されるのは**受け渡しの機構**であって、人間が「はい」を押す動作では
ない。同意 UI そのものは 1924 のとおり実機チェックのまま残る。

## Consequences

- **`lpDesktop` を明示すると壊れる。** `winsta0\default` を渡すと子は
  `0xC0000142 STATUS_DLL_INIT_FAILED`（USER32 の `DLL_PROCESS_ATTACH` 失敗）で
  死に、しかも `NtRaiseHardError` で**誰も閉じられないダイアログを上げたまま
  止まる**のでハングに見える。これは第 2 ユーザー固有ではなく、**ジョブ自身の
  トークンでも同じように落ちる**（run 32593263826 の掃き出し）。
  `Invoke-AsStandardUser` の隣にあった「第 2 ユーザーはこのセッションの
  ウィンドウステーションに対して初期化できない」という説明は、症状は正しいが
  原因の見立てが誤りだったので同じ PR で訂正した。
- **`Start-Process -Credential` は同じ API の上でも通らない。** レグは比較の
  ためにその結果を毎回 `ItLog` に残す。理由は Start-Process 側の性質であり、
  本リポジトリは測っていないので主張しない。
- ハングと即死を見分ける道具として、
  `HKLM\SYSTEM\CurrentControlSet\Control\Windows\ErrorMode = 2` が効く。
  ダイアログを閉じる者がいないセッションでは、ローダ失敗はハングと区別が
  付かない。抑止すると**終了コードとして出る**。
- 否定的な実験ほど交絡変数を先に潰す必要がある。今回、`lpDesktop` が全ラウンド
  固定だったため「Everyone にフル権限を与えても駄目 → ウィンドウステーションは
  無罪」という**反証が丸ごと無効**だった。撤回して測り直した。

## Refs
- waired-agent#997
- `docs/decisions/20260822/1924-installtest-runs-both-privilege-shapes.md`
- run 32593631629 / 32593788357（通した実測）、32593263826（`lpDesktop` の掃き出し）
