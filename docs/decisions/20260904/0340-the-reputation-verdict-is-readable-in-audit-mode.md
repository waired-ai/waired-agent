---
status: accepted
---

# 評判判定は監査モードなら hosted runner で読める — 2216 の第 5 項を狭める (20260904 03:40)

## Status

Accepted。20260822 22:16 の
`../20260822/2216-sac-signing-requirement-is-testable.md` の Decision 第 5 項
「(ii) は実機のまま。CI では試みない」と、同記録の Consequences
「nightly + 専用 dispatch 入力、per-PR ではない」を**この 2 点についてだけ**
改める。同記録の本体（(i) 署名要件と (ii) 評判判定の切り分け、`-SacAudit` の
集合一致、hosted runner 限定、有効化失敗は FAIL）は有効なまま。
2216 を覆すものではない — **強制経路は今も実機だけ**である。

## Context

2216 は Smart App Control を (i) 署名要件と (ii) 評判判定に分け、(i) を
Microsoft 署名の `SmartAppControlAuditNoISG.bin` で CI から測ることにした。
第 5 項は (ii) を実機に残し、理由は AMSI canary の soft-fail と同じ切り分け
（非決定的な判定は always-on のチェックを赤にしてはならない）だった。

waired-agent#1190 は、それでも (ii) を CI で読めないかと問い、選択肢を並べた。
本記録はその測定結果と、第 5 項をどこまで狭めるかを記す。

ランナーは以下すべての run で同じ: `windows-latest` = Microsoft Windows Server
2025 Datacenter (10.0.26100, build 26100)、UEFI、Secure Boot off、
`C:\Windows\system32\CiTool.exe` 同梱、`VerifiedAndReputablePolicyState` 不在
（Server SKU に Smart App Control は無い）。

### A. 署名済みの ISG 版は有効にならない

aka.ms/sacauditpolicies の署名済みアーカイブは**ポリシーを 2 つ**同梱している。
`SmartAppControlAuditNoISG.bin` と並んで、ISG を参照する側の
`SmartAppControlAudit.bin` がある: sha256
`8093E811DDD3CC55D0322D5F9C4549C56342499843C5FFB013D65591C84BD023`、PolicyID
`1283AC0F-FFF1-49AE-ADA1-8A933130CAD6`、friendly name
`VerifiedAndReputableDesktopEvaluationAudit`（NoISG 側は `5283AC0F-…`、実機の
Smart App Control が強制に使うのは `0283AC0F-…`）。

ハーネスが NoISG で既に使っている文書どおりの経路 — EFI システムパーティション
にポリシー自身の GUID で置き、`citool -r`（exit 0、"Operation Successful"）—
で適用しても、`citool -lp` は `IsEnforced=False IsAuthorized=False` を返す。
run 33788162425 / 33789050223 / 33789642929。

### B. グラフ側の前提条件が原因ではない

run 33789050223 はジョブ内で `appidsvc` と `applockerfltr` を起動し（両方
RUNNING）、Defender のクラウド保護を on にした（`MAPSReporting` 0 → 2、
`SubmitSamplesConsent` 2 → 3）。リアルタイム保護は**有効にできず** False の
まま。それでもポリシーは有効にならなかった。

付記: ランナーイメージは `MAPSReporting=0`、`SubmitSamplesConsent=2`、
`RealTimeProtection=False`、`TamperProtected=False` で出荷されている。イメージ
は意図的に Defender を落としているので、これらを先に上げようとせずに
「unknown」を記録するレーンは、グラフではなくランナーイメージを測っている
ことになる。

### C. Windows 自身の記録

run 33789642929 は窓の中で CodeIntegrity が記録したものを全部出した:

```
3101 Code Integrity policy refresh started for 3 policies.
3105 Trying to refresh Code Integrity policy with policy ID {1283ac0f-…}.
3105 … {784c4414-…}   3105 … {5951a96a-…}   3105 … {a072029f-…}
3096 No change in active Code Integrity policy {784c4414-…} … Status 0x0
3096 No change in active Code Integrity policy {a072029f-…} … Status 0x0
3096 No change in active Code Integrity policy {d2bda982-…} … Status 0x0
3102 Code Integrity policy refresh finished for 3 policies.
```

4 つの PolicyID が試され、3 つが決着し、我々のものは 3096 にも 3099 にも
名指しされない。試されて落とされ、理由を述べるイベントは無い。

### D. 文書どおりのカスタム経路は動く

Microsoft の App Control のページは、
`%windir%\schemas\CodeIntegrity\ExamplePolicies\SmartAppControl.xml` を base
policy として使うには "you must remove the option Enabled:Conditional Windows
Lockdown Policy" と書いている。そのとおりに組んだポリシー — そのオプションを
外し、実機の Smart App Control が強制に使うポリシーと衝突しないよう
PolicyID/BasePolicyID を新しく採り、`ConvertFrom-CIPolicy` で変換し、同じ EFI
経路で適用 — は、**同じランナーの同じジョブ内で有効になる**。run 33790336299 が
最初に有効化し、run 33791032706 が最初の全緑。有効になったポリシーのルール
オプションはログにこう出た:

> Enabled:Unsigned System Integrity Policy, Enabled:Advanced Boot Options Menu,
> Enabled:UMCI, Enabled:Inherit Default Policy, Enabled:Update Policy No Reboot,
> Enabled:Intelligent Security Graph Authorization, Enabled:Developer Mode
> Dynamic Code Trust, Enabled:Revoked Expired As Unsigned, Enabled:Allow
> Supplemental Policies, Disabled:Script Enforcement, Enabled:Audit Mode

**署名済み版が落とされる理由は観測されていない。** conditional lockdown の
オプションが原因だというのは、A と D を合わせたときの最も経済的な**推論**で
ある（署名済みポリシーはおそらくそのオプションを保持しており、conditional
lockdown policy は Smart App Control の無い SKU では取り付く先が無い）が、
それを述べるイベントは一つも出ていない。

### E. 最初の実測値

run 33791032706（緑）: 窓の中に 3076 が 4 件、すべてポリシー名
`VerifiedAndReputableDesktop`。監査された（= 許可されなかった）のは
`ProgramFiles/waired-agent.exe` / `ProgramFiles/waired-tray.exe` /
`ProgramFiles/waired.exe`。"On the signing list but allowed here (reputation):
none" — グラフは 3 つのどれも許可しなかった。対照は両方とも期待どおり:
Microsoft 署名の `where.exe` は監査されず、run 中にコンパイルした Go バイナリ
（どのサービスも見たことのないハッシュ）は監査された。assert は 66 本走った。

### F. イベント 3090/3091/3092 は出ない

build 26100 では、文書にある `TestFlags=0x300` と再起動なしには、窓の中に
3 つとも 0 件。hosted のジョブは再起動できない。#1190 はこの build なら
それでも出るのではないかと明示的に問うていたが、出ない。したがって
**ALLOW 側** — ISG が許可したファイルに対する答え — は CI では今も読めず、
許可されたファイルはイベントを一つも残さない
（`docs/knowledges/20260904/0300-the-reputation-verdict-is-in-event-3118.md`）。

### G. 記録しておく価値のある誤り

run 33790336299 はカスタムポリシーを有効化したあと、窓の中に 3076 が 4 件
ある状態で "audited under VerifiedAndReputableDesktopEvaluationAudit: 0" と
報告した。`SmartAppControl.xml` から組んだポリシーは**そのファイルの**
friendly name（`VerifiedAndReputableDesktop`）を持ち、署名済み版の名前では
ない。台帳はハードコードした名前でフィルタされていた。

誤った結論を公開せずに済んだのは、レーンの負の対照のおかげである。対照は
「一度も見られていない未署名バイナリが監査されなかった」と報告した — 何にも
一致しないフィルタを内側から見るとこう見える — ので、モードは自分たちの
ファイルについて何も結論しなかった。対照が無ければ、この run は出荷 3 本
すべてが評判で許可されたと報告していた。今は名前を有効化したポリシー自身の
行から読み、窓の中で見えたポリシー名を毎 run 全部ログに出す。同じ形の
落とし穴は実機側でも踏みかけた
（`docs/knowledges/20260904/0310-a-permissive-window-makes-every-test-pass.md`）。

## Decision

1. **2216 第 5 項を狭める。** 評判判定の**監査モード**の読みは hosted runner で
   観測でき、`installtest-windows.ps1 -SacIsg` がそれを行う。実機に残るのは
   **強制経路** — ユーザが実際にブロックされる、イベント 3077 と 3118 — で、
   そちらは `scripts/dev/sac-verdict.ps1` で読む（waired-agent#1191）。2216 の
   切り分けは強制経路については正しく、監査経路については広すぎた。
2. **経路は `SmartAppControl.xml` から組むカスタムポリシーであって、署名済み
   版ではない。** `-SacIsg` は署名済み版を先に試して落ちたら切り替えるので、
   Microsoft がいつか署名済み版を有効化させる日には、編集なしで短い経路を
   採る。A・B・C は、誰もこの実験をやり直さないために記した。
3. **自分たちのファイルの判定は記録するだけで、assert しない。** ジョブが赤に
   なるのはハーネスの欠陥のときだけ — ポリシーが有効にならなかった、対照が
   期待どおりに振る舞わなかった。これは 2216 第 5 項の理由を捨てたのではなく
   保ったものである。AMSI canary の前例が今も支配する。
4. **2 つの対照は方法の一部であって飾りではない。** 許可されなければならない
   署名済みファイルと、監査されなければならない初見の未署名ファイルを、同じ
   窓に入れる。G がその対照が役に立った実例である。
5. **署名監査 `-SacAudit` は nightly に加えて PR ごとにも走る。** これは 2216
   の Consequences を変える。あの記述は「答えはインストーラの出荷ファイル集合
   が変わったときにしか動かない」を根拠に nightly + dispatch で per-PR ではない
   とした。根拠は正しく、結論が逆だった — その集合を動かす出来事が pull
   request なので、nightly はマージされた翌朝に報告していた。出荷集合を変え
   得るパスにフィルタし、1 ランナーで 4〜5 分（run 33512151202 / 33630971474 /
   33756569404、最初に踏んだ PR で 4m35s）。オーナー裁定（2026-09-04）: 参照で
   あってゲートではない。実装は required checks から外すことによって行い、
   `continue-on-error` は使わない。soft-fail するジョブは緑に見えて読まれ
   なくなる — waired-agent#1112 がその形だった。
6. **ALLOW 側は開いたまま。** F のとおり、イベント 3090/3091/3092 には
   レジストリ変更と再起動が要る。オーナー裁定（2026-09-04）: その実験は
   xps15 で行い、dell では行わない。

## Consequences

- **最初の記録値: 2026-09-04、グラフは出荷 3 本のどれも許可しなかった**（E）。
  ここから先は夜ごとの系列になる。成果物の保持は 14 日ではなく 90 日 —
  拒否から許可への転換の遅れは週単位で測るものだから。
- **`-SacIsg` の assert 下限は 65。** run 33791032706 で 66 を実測した。実測
  より 1 つ低いのは意図的で、66 のうちちょうど 1 つは「署名済み版が有効に
  ならなかったところで option 1 が有効になった」の assert であり、署名済み
  版が動く経路では発火しない。
- 新しい 2 レーンは nightly レポートの `needs`、その env ブロック、
  `scripts/ci/nightly-red-lanes.sh` に入っており、自己テストは
  waired-agent#1112 の形で持つ。
- 2216 から変わらないもの: hosted runner だけで走らせ、self-hosted プールには
  張らない（第 4 項）。ポリシーが有効にならなかった run は FAIL し、空の表を
  合格として報告しない（第 3 項）。

## 訂正した記述

| 場所 | 誤っていた記述 | 今 |
|---|---|---|
| `../20260822/2216-sac-signing-requirement-is-testable.md` Decision 第 5 項 | 「(ii) は実機のまま。CI では試みない」 | 広すぎた。監査モードの読みは観測できる。実機に残るのは強制経路 |
| 同 Consequences | 「nightly + 専用 dispatch 入力、per-PR ではない」 | 署名監査は PR ごとにも走る。パスでフィルタし、参照として |
| `scripts/dev/installtest-windows.ps1` の Smart App Control 解説ブロック | 「(ii) … Not attempted here」 | この変更で更新済み。`-SacIsg` が何を記録するかを述べる |
| waired-agent#1190 の option 1 | 「未署名のカスタムポリシーは再起動なしで有効化されるか不明」 | ジョブ内で有効になる。再起動不要（run 33790336299 / 33791032706） |

## Refs

- waired-agent#1190（本記録の問い）、waired-agent#1191（強制経路の実機読み、
  `scripts/dev/sac-verdict.ps1`）、waired-agent#1112（soft-fail が読まれなく
  なる形）
- `../20260822/2216-sac-signing-requirement-is-testable.md`、
  `../20260822/1924-installtest-runs-both-privilege-shapes.md`
- `docs/knowledges/20260904/0300-the-reputation-verdict-is-in-event-3118.md`、
  `docs/knowledges/20260904/0310-a-permissive-window-makes-every-test-pass.md`
- Microsoft Learn, "Test App Signatures with Smart App Control"（署名済み
  監査ポリシー 2 種）; App Control for Business の example policies のページ
  （`SmartAppControl.xml` を base policy にする際の "Enabled:Conditional
  Windows Lockdown Policy" の除去）
