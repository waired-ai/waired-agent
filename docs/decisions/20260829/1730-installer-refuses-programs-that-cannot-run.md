---
status: accepted
---

# インストーラは「この機で実行できないプログラム」を置かない (20260829 17:30)

## Status

Accepted。オーナー裁定 2026-08-29(waired-agent#1087 の作業中)。

## Context

`waired update` が Smart App Control 有効の Windows で、サービスを止め →
バイナリを入れ替え → 新しい `waired.exe` が Application Control に拒否され →
そこで停止した。ホストは **サービス STOPPED・CLI 実行不能・復旧手段の記載なし**
で残った(waired-agent#1087)。`waired doctor` は #315/#653 でこの拒否を診断
できるが、ここでは役に立たない — 実行すること自体が拒否されている。

sv-xps15(Windows 11 Pro、SAC 有効)で実測した事実:

- **拒否はファイル単位**。同じ日に、edge ビルドは `waired.exe` が拒否されて
  `waired-agent.exe` は動き、リリースビルドは**その逆**だった。
- **判定は勝手に変わる**。朝に拒否されたファイルが、機側は何も変えていないのに
  午後には実行できた。
- 依って、**どのビルドでも 3 本のうち 1 本は拒否されうる**状態が現実に起きる。

ここで「新規インストール経路で、拒否されたバイナリをどう扱うか」が分岐した。
`waired-agent.exe` が拒否 = 製品が何もできないので中断は自明。争点は
`waired.exe`(CLI)だけが拒否された場合で、デーモンとアプリは動くため
「警告して続行」も選べた。

## Decision

**インストールでも更新でも、`waired.exe` か `waired-agent.exe` のどちらかが
この機で実行できないなら、そこで止める。** 規則は 1 つ:
*Waired はこのコンピューターが実行を拒否するプログラムを、置きも入れ替えもしない。*

オーナー裁定の理由: **リリース前なので、既存の機を救う必要がない**。加えて、
CLI 抜きの「インストール済み」は成立しない — `waired init` が CLI であり、
セットアップの完了も `waired doctor` も `waired update` もそこにある。それを
「入った」と呼ぶのは #1087 が問題にしている不正直さと同じ形になる。

`waired-tray.exe`(Waired アプリ)だけが拒否された場合は**警告して続行**する。
アプリが開けないだけで、常駐サービスと `waired` コマンドは影響を受けない。

判定はチェックとサービス起動の間にも変わりうるので、**入れ替え後に失敗した
場合は旧バイナリを戻してサービスを再起動する**。戻せなかった場合だけ、
元の版を指定した再インストールのコマンドを名指しする。

## Consequences

- **拒否している機には Waired が入らない。** 上の実測どおり、その日その機が
  3 本のうち 1 本を拒否していれば導入は止まる。文言は理由と「判定は変わりうる
  ので後でもう一度試せる」ことを言う。恒久的な解は署名(waired#759 Phase 0)。
- インストーラは入れ替えの**前に** staging の実行ファイルを起動する。よって
  Code Integrity は staging のイメージも判定する
  (`docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md` に訂正を
  追記済み。staging がインストール先の直下にある限り SAC 一覧は変わらない)。
- 更新経路は**必ず**旧バイナリの控えを自分で持つ。ReplaceFile が残す
  `~RF*.TMP` は当てにしない(残らないことがあり、実際 #1087 の機には
  `waired-agent.exe` のぶんが無かった)。
- ロールバックは try/catch で包めない。`Common-Die` は `exit` で終わり例外に
  ならないので、**失敗の集約点(`Common-Die` と script の `trap`)から呼ぶ**
  形にした。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1087
- docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md
- docs/knowledges/20260829/1740-sac-verdict-is-per-file-and-moves.md
