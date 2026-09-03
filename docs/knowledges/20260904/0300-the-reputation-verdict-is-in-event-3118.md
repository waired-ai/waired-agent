# 評判判定はイベント 3118 に既に書かれている (20260904 03:00)

## Issue

waired-agent#1191 — Smart App Control が Waired のプログラムを拒否する条件と、
拒否が解ける条件を調べる。同 issue の冒頭は、rc5 検証(waired#1309)で採れた
証拠が**インストーラ自身の文言だけ**だったと記している。CodeIntegrity の
イベントそのもの(3077 のハッシュ 4 種、3089 の署名詳細、3118 のブロック詳細)、
ファイルごとの拡張属性、Defender の状態はどれも採っていなかった。そこで ISG の
答えを直接読む手段として、`HKLM\SYSTEM\CurrentControlSet\Control\CI` に
`TestFlags=0x300` を置いて再起動し、`PassesSmartlocker` を持つ診断イベント
3090/3091/3092 を有効にする案が、オーナー判断待ちとして置かれていた。

調べてみると、**拒否側についてはその調査が要らなかった**。証拠は失われておらず、
今もホストのログにある。

## Learnings

### 1. ログはまだ残っていて、読むのに昇格は要らない

`Microsoft-Windows-CodeIntegrity/Operational` は一般ユーザーで読める(実測。
昇格なし)。しかも実機では数か月分が残っている。

| ホスト | 期間 | 3033 | 3077 | 3089 | 3118 | 備考 |
|---|---|---|---|---|---|---|
| dell(25H2) | 2026-08-07T14:18:59Z 〜 2026-09-02T09:52:37Z、計 598 件 | 177 | 81 | 259 | 81 | |
| xps15(#1181 の並行セッションが測定) | 最古 2026-05-10T17:22:39Z | 118 | 93 | 226 | 93 | 3076 / 3090 / 3092 は 0 |

xps15 では 3077 と 3118 が 93 対 93、つまり**拒否 1 件ごとにブロック詳細が
1 件付いていた**。

結果として、#1191 の rc5 タイムラインは、インストーラ出力ではなく
このイベントから、事後に、**新しいインストール試行を 1 回もせずに**再構成できた。

### 2. イベント 3118 に ISG の答えが入っている

3118 は "Smart App Control Block Details"。観測したフィールドは
`DefenderCalled`, `DefenderCallAttempted`, `DefenderCloudCallRequested`,
`DefenderMadeCloudCall`, `DefenderCloudHTTPCode`, `DefenderTrust`,
`CachedDefenderTrust`, `CachedDefenderTrustExpiryTime`,
`DefenderTrustExpiryTime`, `DefenderScanResultDetails`, `IsUnfriendlyFile`,
`DefenderStatusCode`, `DefenderClientStatusCode`, `DefenderCatDbFailure`,
`DefenderEngineReportGUID`, `EADefenderTrustCached`, `TTLValid`,
`DefenderDisabled`, `ExternalAuthorizationFlags`。

このリポジトリで 3118 を読んでいるものは、この作業の前には 1 つも無かった。
`internal/platform/servicediag` が読むのは 3033 と 3077
(`internal/platform/servicediag/servicediag.go` の定数
`winCodeIntegrityBlocked = 3033` / `winCodeIntegrityAudit = 3077`)、
`scripts/dev/installtest-windows.ps1 -SacAudit` が読むのは 3076。
**評判判定の側が観測不能に見えていたのは、誰も 3118 を読んでいなかったから**である。

ただし、**許可側には今も `TestFlags` + 再起動が要る。** 許可されたファイルは
イベントを 1 件も出さない。だからログは「反転の前の最後の拒否」を日付で示せるが、
**反転そのものは示せない**。許可を直接読める唯一の手段は 3090/3092 で、
それにはレジストリ変更と再起動が必要になる。

### 3. 3118 のフィールド集合は Windows ビルドで違う

25H2 の dell は、#1181 セッションが測ったホストより 3118 のフィールド数が
少なかった。**読む側は、存在する `<Data Name="...">` ノードを全部拾い、どの
フィールドも在ると仮定しない**。`scripts/dev/sac-verdict.ps1` はそう書いてある。

### 4. dell の拒否 81 件が実際に言っていること

| フィールド | 値 | 件数 |
|---|---|---|
| `IsUnfriendlyFile` | `false` | 81 / 81 |
| `DefenderTrust` | 定数 -16777216(0xFF000000) | 81 / 81 |
| `DefenderStatusCode` / `DefenderClientStatusCode` / `DefenderCatDbFailure` | すべて 0 | 81 / 81 |

つまり**問い合わせ自体は毎回成功していた**。採取時の Defender の状態は
`AMRunningMode=Normal`, `AMServiceEnabled=True`,
`RealTimeProtectionEnabled=True`, `IsTamperProtected=True`, `MAPSReporting=2`,
`SubmitSamplesConsent=1`。

**この拒否はマルウェア判定でも望ましくないソフトウェアの判定でもない。**
「肯定的な予測が存在しない」場合である。Microsoft の説明はこう分かれている:

> Microsoft's app intelligence services provide safety predictions for many
> popular apps. If the app intelligence service is unable to make a prediction,
> then Smart App Control will still allow an app to run if it is signed with a
> certificate issued by a certificate authority (CA) within the Trusted Root
> Program.
>
> Malware, Potentially Unwanted Apps (PUA), and unknown, unsigned code are
> blocked by default.
> — <https://learn.microsoft.com/en-us/windows/apps/develop/smart-app-control/overview>

> Smart App Control will block execution of unsigned files unless the file has
> a positive reputation.
> — <https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation>

よって結果は 2 通りではなく 3 通りある:

| 結果 | 何が起きるか |
|---|---|
| 肯定的な予測がある | 許可 |
| 確信のある予測が無い | 署名検査に落ち、未署名なら失敗 |
| 危険と判定 | ブロック |

dell で測れた 81 件はすべて真ん中である。Waired がマルウェアの基準に近いか
遠いかについては、この記録は何も言わない(測っていない)。

### 5. クラウドには 1 回問い合わせ、以後はキャッシュの答えが使われる

フィールドから**区別できること**を記す。キャッシュのアルゴリズムを証明した
わけではない。

- `DefenderCloudHTTPCode` は、連続した拒否の**最初の 1 件で 0xc8000000**、
  それ以降は 0x0。その間 `DefenderCalled` はずっと `true`。
- `CachedDefenderTrust` はファイル単位ではない。暦に沿って進む:
  08-17 に 122158、08-19 に 122159、08-21 に 122161、08-22 に 122162、
  08-27 に 122166、08-30 に 122168、08-31 に 122169、09-01 と 09-02 に 122170。
  おおむね 1 日 1 つ進む世代番号に見える。
- #1181 の並行セッションは xps15 で、初見のハッシュ 1 件
  (2026-09-03T16:19:16Z)が新規にクラウドへ問い合わせ
  (`DefenderMadeCloudCall=true`, `DefenderCloudHTTPCode=0xc8000000`,
  `EADefenderTrustCached=false`, `TTLValid=false`)、それでも拒否されたのを
  測っている。

### 6. 拡張属性はドキュメントにある名前ではない

Microsoft が文書化しているのは `$KERNEL.SMARTLOCKER.ORIGINCLAIM`
(<https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/operations/configure-appcontrol-managed-installer>)。
Windows 11 25H2 で、判定を受けた実行ファイルに実際に付いていたのは
`$KERNEL.PURGE.ESBCACHE` だった。

| ファイル | サイズ |
|---|---|
| `waired.exe` / `waired-tray.exe` / ビルドしたての対照バイナリ | 0x76 |
| `C:\Windows\System32\where.exe` | 0x11f |

**読む側は 1 つの名前を探すのではなく、付いている属性名を全部列挙する。**

もう 1 つ。`fsutil file queryEA` のラベルは OS の表示言語で刷られる
(日本語ホストは "EA 名"、英語ホストは "Ea Name")。ローカライズされない
**属性名**の側を解析し、散文は生の証拠としてだけ保存する。

### 7. この記録が狭めるもの

`docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md` の
Decision 5「(ii) は実機のまま。CI では試みない」が、この記録が狭める対象である。
拒否側の答えは 3118 に既にあり、読むのに実機固有の条件は要らない。
同記録の改訂は、waired-agent#1190 の CI 計測が済んでから行う(保留中)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1190
- https://github.com/waired-ai/waired-agent/issues/1191
- https://github.com/waired-ai/waired/issues/1312
- https://github.com/waired-ai/waired/issues/1309
- scripts/dev/sac-verdict.ps1
- docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md
- docs/knowledges/20260829/1740-sac-verdict-is-per-file-and-moves.md
