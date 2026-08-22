---
status: accepted
---

# installtest は root/非root の両方の起動形をすべての OS で走らせ、Windows は昇格の拒否側と Phase 2 を検証する (20260822 19:24)

## Status
Accepted

## Context

インストーラには、権限をどう得るかで 2 つの起動形がある。

- **非特権で起動し、インストーラ自身が権限を上げる。** ドキュメントが案内する
  `curl -fsSL … | sh` / `iwr … | iex` の形。Linux/macOS では
  `common_elevate` が `SUDO=sudo` を選び (`install.sh:299-301`)、以降の約 50
  か所の `$SUDO` が実際の sudo 実行 (env_reset つき) になる。Windows では
  `Invoke-SelfElevate` (`install.ps1:1411`) が `Start-Process -Verb RunAs`
  (`:1460`) を呼び、AppInfo サービスが `CreateEnvironmentBlock` で**新しい環境
  ブロック**を作って昇格した子を起動する。
- **すでに root/Administrator のシェルから起動する。** `sudo sh install.sh`、
  プロビジョニングスクリプト、昇格済み PowerShell。`install.sh` は `id -u` が
  0 の枝に入って `$SUDO` は空語、`install.ps1` は `:3465` の already-admin 枝
  でインラインに Phase 2 を実行する。

`install.sh:1848-1851` は「両方の形が動く」と明記している。しかし installtest
が実際に走らせていたのは、**macOS が前者だけ、Linux と Windows が後者だけ**
だった (waired-agent#990 / #991)。結果として:

- Windows では `Export`/`Import-InstallState`、新しい環境ブロック、
  `.progress`/`.status` サイドカー、`Watch-ElevatedConsole`、終了コードの解読、
  拒否された UAC の catch (`install.ps1:1462-1480`) が**一度も実行されて
  いなかった**。ハーネスが持っていたのは「親が渡すはずの argv」の検査と、
  `WAIRED_*` を消した子で環境喪失を**模擬**したものだけで、模擬は実際には
  失われていない環境を観測していた。
- Linux では非 root 起動でしか通らない枝が未実行で、その中に実物の欠陥が
  あった: `linux_enable_tray_host_extension` が `SUDO_USER` を必須にしていた
  ため、`curl … | sh` の正道ではトレイ拡張の有効化が丸ごとスキップされていた
  (waired-agent#993)。ハーネスが `sudo env … sh install.sh` で起動していた
  ことが、そのまま見落としの原因だった。

## Decision

**installtest は 3 OS すべてで両方の起動形を走らせる。**

| OS | 主脚 (全アサート) | 第二脚 (Tier-1 相当) |
|---|---|---|
| Linux | 非 root (`--local` の既定) | root シェル (`IT_INSTALL_AS_ROOT=1`)。LXD 脚は真の root ログインで第三の形 |
| macOS | 非 root (従来どおり) | root シェル (`sudo -E env … bash install.sh`) |
| Windows | 昇格済みセッション (従来どおり) | 非昇格からの UAC 受け渡し: 標準ユーザの拒否アームと、遷移に依存しない Phase 2 アーム (許可される昇格は #997) |

順序は共通で、主脚 → `uninstall.sh --clean --yes` → 第二脚。第二脚は fresh
install でなければ `confirm_proceed` が要約を出さず、どちらの枝を通ったかを
観測できないため。

**どちらの枝を通ったかは、インストール後の状態からは復元できない。** 唯一の
観測点は `show_install_summary` が `id -u` 非ゼロのときだけ出す
"Ask for administrator rights" の行 (`install.sh:1086-1087`) なので、Linux と
macOS の両脚はこの行の有無を対で assert する。この 1 行が製品出力である以上、
文言を変えるときは同じ変更でアサートも動かす。

**Windows で CI が検証するのは、昇格の「拒否側」と、昇格を経ない Phase 2 で
ある。** 許可される昇格は自動化できないことが実測で分かったので、そこは
#997 に切り出した (根拠は後述)。同意 UI そのものは人間が要るので実機
チェックのまま残る。

ランナーの UAC 実値はレグが毎回ログに出す — 前提を記憶ではなく実測に置く
ため。実測 (run 32567682964) の `windows-latest` は `EnableLUA=1`,
**`ConsentPromptBehaviorAdmin=0`**(既に「確認なしで昇格」),
`ConsentPromptBehaviorUser=3`, `PromptOnSecureDesktop=1`,
`FilterAdministratorToken` 不在=既定。同意ダイアログを消す設定は**元から
そうなっていた**ので、この面で歪めているものは無い。

拒否アームの `ConsentPromptBehaviorUser=0` ("Automatically deny elevation
requests") は性質が違う。これは企業の標準ユーザ端末の**実在の出荷構成**で
あり、標準ユーザの昇格要求が人間なしに決着する唯一の設定でもある。歪めて
いるのではなく、出荷しているのに一度も実行していなかった枝
(`install.ps1:1462-1480`) に到達するための構成である。

### UAC フィルタ済みトークンをどこから取るか (実測で1案が否定された)

当初案は「ランナーとは別のローカル管理者を作り、`schtasks` を既定の実行
レベル (`/RL HIGHEST` なし) で起動すれば UAC フィルタ済みトークンになる」
だった。**実測で誤りだった。** 保存資格情報のスケジュールタスクは**バッチ
ログオン**であり、UAC のトークン分割は**対話ログオン**のときに LSA が
linked token を作るところでしか起きない。したがってタスクの管理者は**完全な
トークン**を受け取り、`Test-Admin` は真を返し、`install.ps1` は何も越えずに
already-admin 枝に入る。アサートはこれを正しく捕まえた
("the token was not filtered after all")。

同じ run でもう一つ分かったこと: **バッチログオンには対話ウィンドウ
ステーションが無い。** 標準ユーザのアームで Windows が返した文言は
`This operation requires an interactive window station.` である。これは
AppInfo がそのセッションでは誰に対しても昇格できないことを意味する。

残る候補は `runas /trustlevel:0x20000` (`Invoke-AsBasicToken`) — **今の
セッションの** SAFER 制限トークンで、新規ログオンではないのでウィンドウ
ステーションを持つ。`Test-Admin` が偽を返すことは #195 のアサートが既に
実測している。**これも実測で潰れた** (run 32568318138): SAFER 制限
トークンでは `Get-FileHash` が使えず、`install.ps1` は昇格に到達する
はるか手前、SHA-256 照合のところで
`The term 'Get-FileHash' is not recognized` で死ぬ。

**結論: 昇格が「許可される」経路は GitHub-hosted ランナーでは自動化
できない。** 「もっと良いランナーを待てばよい」ではなく、2つの経路の
どちらも形が違う。したがって永続的に skip し続けるアームは置かず、
機構と実測をコメントに残して**別 issue (#997) で追跡する**。

### 昇格を経ずに Phase 2 を実行する

Phase 2 側は**遷移に依存しないアーム**で常に実行する。Phase 1
に自分の書き手で状態文書を書かせ (`WAIRED_ARGTEST_STATEFILE`,
`install.ps1:3339-3341` — ダウンロードもインストールもしない)、`WAIRED_*` を
全部消した別プロセスで Phase 2 を起動する。これは AppInfo の
`CreateEnvironmentBlock` が本物の昇格子に残す環境と同じ形である。

ここでしか走らないもの: 環境に一度も存在しなかったパラメータを
`Import-InstallState` が再水和する経路 (`:387-435`)、`$ElevatedConsole` が
自分のトランスクリプトを張る経路 (`:755-761`)、そして
`.progress` / `.status` サイドカー — `Write-InstallProgress` は `$StateFile`
が無ければ即座に返る (`:288`) ので、**完全な breadcrumb 列はこの経路でしか
生成されない**。既存の #192 プローブは同じ形を `WAIRED_ARGTEST` の裏で
走らせており、シームで返るのでそのどれにも到達していなかった。

なお **`Get-ConsoleUser` / `HKEY_USERS\<sid>` の書き分け**
(`:2086-2093`, `:2212`) は、インストールした主体とコンソールユーザが別人の
ときにしか証明できない。それは昇格が許可される経路でしか成り立たないので、
AppInfo 自身の環境ブロックと合わせて **#997** の担当とする。

## 対象外

**SmartScreen / Smart App Control の評判判定は CI では観測できない。** SAC は
コンシューマ Windows 11 の機能で、クリーンインストールと登録を要し、ランナー
イメージは登録されていない。実機で観測されている挙動 (同じ zip の同じ
ディレクトリの 2 バイナリで判定が割れる、3 日前に入れた版が後からブロックに
反転する、`sudo.exe` 経由でも SCM 経由でも直接実行でも同じ) は**ファイル単位
の評判判定**であって、昇格の有無では変わらない。したがって昇格遷移を CI で
走らせても、この判定は代表できない。実機チェックとして残す。

このクラスの構造的な解決は署名であり、waired#759 ("installer: Windows
installer convergence to signed Inno .exe — phased tracker") の Phase 0 が
Windows の署名調達を追跡している。

## Consequences

- Linux の `--local` は非 root 起動になるため、**パスワードレス sudo が前提に
  なる**。`installtest-macos.sh:1267` に倣って `sudo -n true` の事前チェックを
  置く。これが無いと最初の `$SUDO` 呼び出しがパスワード待ちで刺さり、
  `common_daemon_owns_log_level` が 30 回それを繰り返す。
- `run_install` は per-PR 脚だけでなく `installtest-inference.yml` の 4 脚も
  通る。それらは `--no-init` を外すので、非 root 化によって初めて
  `waired init` の `SUDO_USER` ホップ (`cmd/waired/init_integration.go:187-199`)
  が実行される。狙った被覆増だが、nightly が新しく赤くなる可能性がある。
- Windows の拒否アームは**タイムアウトを FAIL として扱う**。タイムアウトは
  遅いマシンではなく「誰も答えられないダイアログが出ている」の署名であり、
  skip にすると、このアームが排除するために存在する条件そのものを隠す。
  抑制し損ねた MsgBox が run を 28 分刺した前例がある。
- アサート数の下限 (`installtest-run.sh` / `installtest-macos.sh` /
  `installtest-windows.ps1`) は、アサートを足した同じコミットで引き上げる。
  Windows のアームはすべて必ず走るので、条件付きの下限は要らない (条件付きに
  したくなった遷移アームは #997 として外に出した)。
- **前提を記憶で持たない。** この決定に至るまでに、UAC の挙動についての推測が
  2つ実測で覆っている (バッチログオン = フィルタ済みトークン、は誤り。SAFER
  制限トークンでインストーラが走る、も誤り)。
  ランナーの UAC 実値はレグが毎回ログに出すので、次に触る人は run を1つ開けば
  現在の姿勢が読める。

## Related Records

- waired-agent#990 (Linux/macOS の起動形)、#991 (Windows の自己昇格経路)、
  #993 (非 root 起動で死んでいたトレイ拡張の有効化)、#44 (state-dir ACL)、
  #997 (許可される昇格 — 実デスクトップセッションが要る残りの被覆)
- waired#759 (署名付き .exe への収束 — Phase 0 が署名調達)、waired#760
  (3-OS installtest を毎 PR・非昇格コンテキストを含む)
- #192 / #177 (`-StateFile` が存在する理由)、#315 / #653 (SAC が未署名
  バイナリをブロックしたときの診断)
- Microsoft, "User Account Control settings and configuration"
  (Local Policies\Security Options) — `ConsentPromptBehaviorAdmin` /
  `ConsentPromptBehaviorUser` / `EnableLUA` の値と意味
