---
status: accepted
---

# Smart App Control は「署名要件」と「評判判定」に分けて扱い、前者は CI で測る (20260822 22:16)

## Status

Accepted。20260822 19:24 の
`1924-installtest-runs-both-privilege-shapes.md` の「対象外」節を**この 1 点に
ついてだけ**改める。同記録の本体（3 OS の root/非 root 起動形の行列と、
Windows の昇格拒否側・Phase 2 の検証）は有効なまま。

## Context

`1924-…` の「対象外」節は、こう書いていた:

> **SmartScreen / Smart App Control の評判判定は CI では観測できない。** SAC は
> コンシューマ Windows 11 の機能で、クリーンインストールと登録を要し、ランナー
> イメージは登録されていない。

同趣旨が `scripts/dev/installtest-windows.ps1` のコメント、waired-agent#991 の
本文、waired-agent#997 の "Not the same thing" 節にも書かれた。

これは **2 つの別々の問いを 1 つに畳んでいた**。分けると片方は CI で測れる。

| 問い | 何が答えるか | 決定的か | CI で観測できるか |
|---|---|---|---|
| **(i) 署名要件** — このインストーラが置くファイルのうち、Windows が信頼する証明書で署名されていないのはどれか | `SmartAppControlAuditNoISG.bin` | **はい**（ISG を参照しない） | **はい** |
| **(ii) 評判判定** — 未署名バイナリを ISG がその日どう採点するか | `SmartAppControlAudit.bin` ＋ コンシューマ Win11 の evaluation mode | いいえ | いいえ |

Microsoft は開発者向けに**署名済みの監査ポリシーを 2 つ配布している**
(["Test App Signatures with Smart App Control"][ms-sac-test])。
`SmartAppControlAuditNoISG.bin` は "Use this policy to test your own apps as a
developer." と名指しされたもので、Intelligent Security Graph を参照せず、
"only apps that a trusted certificate properly signs are allowed without audit
events" という判定をする。**ブロックせず監査イベントだけを出し**、
"You can apply this policy even when you set Smart App Control to Off" と
明記されている。つまり **Smart App Control の登録も evaluation mode も要らない**。

適用は EFI システムパーティションの
`\efi\microsoft\boot\cipolicies\active\{5283AC0F-FFF1-49AE-ADA1-8A933130CAD6}.cip`
へ置いて `citool.exe -r`。`citool.exe` は Windows 11 22H2 以降と
**Windows Server 2025** の Windows イメージに同梱されており
([CiTool のリファレンス][ms-citool])、`windows-latest` はその Windows Server
2025 である。監査結果は `Microsoft-Windows-CodeIntegrity/Operational` の
イベント **3076**（evaluation）/ **3077**（enforcement）に出る。

(ii) が観測できないという記述は今も正しい。非決定性は実測されている: 同じ zip
から入った 2 つの実行ファイルで判定が割れ、数日動いていたファイルが後から
拒否に反転する
(`docs/knowledges/20260822/1906-tray-row-ab-capture-on-real-hardware.md` §5)。

## Decision

1. **`installtest-windows.ps1 -SacAudit` が (i) を測る。** 監査ポリシーを
   **インストールの前に**適用し、インストーラが置いた／読み込んだファイルのうち
   監査されたものを一覧にする。Microsoft の指示（"test all of your app's install
   and uninstall binaries"）に従い、アンインストール経路も同じ窓に入れる。
2. **一覧は `scripts/dev/testdata/sac-signing-inventory.txt` と集合一致で
   突き合わせる。** 双方向:
   - 一覧から消えたファイル = 署名されたファイル。**その日にこのテストが落ち**、
     一覧は意図的に更新される。
   - 一覧に無いのに監査されたファイル = 署名対象と誰も判断しないまま出荷に
     混ざったバイナリ。
   今日はこの一覧が「我々の全バイナリ」になる。それは失敗ではなく**測定結果**で
   あり、署名を入れる日（waired#759 Phase 0）のチェックリストになる。
3. **ポリシーが有効にならなければ FAIL する。skip しない。** 黙って先へ進んだ
   実行は空の一覧を報告し、「全部署名済み」と読める。これはこのモードが避ける
   べき唯一の失敗の形である。
4. **hosted runner でだけ走らせる。** Microsoft 署名の App Control ポリシーの
   適用はきれいに巻き戻せないので、実行後に破棄される VM でしか張らない。
   self-hosted プール（`installtest-inference.yml` の banner レグ）には**絶対に
   張らない**。
5. **(ii) は実機のまま。** CI では試みない。既存の AMSI canary が
   「非決定的な Defender の判定は always-on のチェックを赤にしてはならない」と
   して soft-fail になっているのと同じ切り分けである。

## Consequences

- **`-SacAudit` は CI が Tier 1 で走らせる最初の構成**になる。
  `1924-…` 以前から `installtest-windows.ps1` にある「Tier 1 には floor を
  置かない（CI は -Tier 2 しか走らせないので、緑の実行から数を採れない）」と
  いう理由がここでは成り立たないので、このモードは**自分の assert 数の下限**を
  持つ。他の下限と同じく**推定せず実測**する。
- **dispatch 起動のみで着地する。** 署名済みポリシーがこの SKU で再起動なしに
  有効化できるかは未実測で、再起動は hosted runner にできない唯一のことである。
  dispatch が 1 回緑になってから nightly に載せる。
- **hosted で成立しなかった場合の退路**は GCP の Windows Server VM。
  `installtest-inference.yml` の GPU レーンが確立したパターン（VM を作る →
  guest attributes で制御 → GCS でログ → 削除、inbound 無し）をそのまま使える。
  VM なら再起動できる。**実測で必要と分かってから着手する。**
- **per-PR のゲートには載せない。** 署名の状態は PR ごとに変わるものではなく、
  CI 負荷を増やす理由がない。

## 訂正した記述

以下は本記録が誤りとして置き換える。いずれも (ii) についてなら正しく、
Smart App Control 全体についての主張としては偽だった。

| 場所 | 誤っていた記述 |
|---|---|
| `1924-installtest-runs-both-privilege-shapes.md` §対象外 | 「SAC はコンシューマ Windows 11 の機能で、クリーンインストールと登録を要し、ランナーイメージは登録されていない」を SAC 全体に適用した |
| `scripts/dev/installtest-windows.ps1`（自己昇格節の冒頭） | 同上 |
| waired-agent#991 | "a hosted runner has SAC off" を「だから観測できない」の根拠に使った。**SAC が off でも NoISG ポリシーは適用できる** |
| waired-agent#997 §"Not the same thing" | "SAC needs a consumer Windows 11 clean install plus enrolment" |

あわせて、**「SAC の無効化は Windows では一方通行（再有効化には OS 再インストール
が要る）」も誤り**だった。一方通行なのは **Settings 経由**（現在 evaluation の
ときを除く）で、[ドキュメントはレジストリで任意のモードに強制する手順を
載せている][ms-sac-test]（BitLocker の保護を一時停止 → 動的シグネチャを削除 →
回復環境から SYSTEM ハイブをオフライン編集 → 再起動）。
ただし**実機ではこれを行わない**方針は変えない。理由が「不可能だから」から
「保護を落とすことになり、かつ実機は (ii) の観測所なので判定条件を変えたくない
から」に変わるだけである。

[ms-sac-test]: https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/test-your-app-with-smart-app-control
[ms-citool]: https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/operations/citool-commands
