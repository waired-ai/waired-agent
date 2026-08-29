# Smart App Control の判定はファイル単位で、しかも数時間で反転する (20260829 17:40)

## Issue

waired-agent#1087(更新が新バイナリを実行できずホストを落とす)の検証で、
sv-xps15(Windows 11 Pro、`VerifiedAndReputablePolicyState=1`)を 1 日使った。
「拒否される状態」を再現したかったが、判定が動くので**待っても再現しない**。
その動き方自体が製品判断の根拠になったので記録する。

## Learnings

**1. 同じ日に、ビルドごとに拒否されるファイルが違う。**

| ビルド | `waired.exe` | `waired-agent.exe` |
|---|---|---|
| edge cd40f98 | 拒否 | 許可 |
| 0.0.3-rc4 (90dd4a5) | 許可 | 拒否 |

同じ zip から出た 3 本が同じ判定になるとは限らない。
`docs/knowledges/20260822/1906-tray-row-ab-capture-on-real-hardware.md` §5 の
「2 つの実行ファイルで判定が割れる」の追認で、**方向が逆のペアまで観測**した。

**2. 判定は数時間で反転する。** 8/27 に拒否された残置バイナリが 8/29 朝には
実行でき、8/29 朝に拒否された edge の `waired.exe` は同日午後に実行できた。
機側では何も変えていない(再起動なし・ポリシー変更なし)。よって
「あとでもう一度試す」は実際に有効な助言で、製品文言にも入れた。

**3. 拒否は CreateProcess で起きるので、PowerShell では例外になる。**
`$ErrorActionPreference` の値によらず `ApplicationFailedException` が投げられる
(Continue でも Stop でも実測)。だから「非致命的」と書いてある一歩でも
`try/catch` が無ければ install が落ちる — #1087 の変種 B がこれだった。

**4. 逆に、EAP=Stop では「stderr に 1 行書いた」だけで終了エラーになる。**
`waired-agent.exe -h` は usage を stderr に出すので、`2>$null` を付けていても
`RemoteException`(Message = "Usage of waired-agent:")が投げられる。**拒否と
区別できない**ので、スモークテストは呼び出しの間だけ `Continue` に落とす。

**5. `$_.Exception.Message` には PowerShell の位置情報が同じ行に付く。**
`... ではありません。発生場所 C:\...\install.ps1:2129 文字:9`。最初の 1 行を
取っても消えない。**最内側の例外**(`Win32Exception`)が OS 自身の文言で、
そこに位置情報は無い。無い場合は `InvocationInfo.PositionMessage` の 1 行目を
突き合わせて切る — コンソール言語を知らなくて済む。

**6. サービス起動の拒否は CodeIntegrity 3077 に `services.exe` として出る。**
`powershell.exe` から同じ exe を起動しても同じ 3077 が出るので、**サービスを
止める前に 1 回起動してみる**だけで、SCM が起こすはずの拒否は先に分かる。
Policy ID は `{0283ac0f-fff1-49ae-ada1-8a933130cad6}`。

**7. 再現は細工 zip で作る。** 実行できないファイル(有効な PE でないもの)を
zip に入れれば、インストーラから見える条件は拒否と同一。ロールバック側は
「起動はするがすぐ終わる」プログラムが要る — `where.exe` で足りる
(Microsoft 署名済みなので SAC 有効の機でも通り、サービスとしては即終了する)。

**8. `~RF*.TMP` は消えずに溜まる。** `[IO.File]::Replace` の作業一時ファイルで、
消せなかったぶんがインストール先に残る。sv-evox2 に 4 本(旧 `waired.exe` と
旧 `waired-tray.exe`)あった。#1087 の報告者はこれをコピーして復旧している。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1087
- docs/decisions/20260829/1730-installer-refuses-programs-that-cannot-run.md
- docs/knowledges/20260822/1906-tray-row-ab-capture-on-real-hardware.md
