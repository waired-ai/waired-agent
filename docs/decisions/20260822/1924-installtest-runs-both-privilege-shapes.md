---
status: accepted
---

# installtest は root/非root の両方の起動形をすべての OS で走らせ、UAC は遷移だけを検証する (20260822 19:24)

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
| Windows | 昇格済みセッション (従来どおり) | 非昇格からの UAC 受け渡し: 標準ユーザの拒否アームと、UAC フィルタ済み管理者の成功アーム |

順序は共通で、主脚 → `uninstall.sh --clean --yes` → 第二脚。第二脚は fresh
install でなければ `confirm_proceed` が要約を出さず、どちらの枝を通ったかを
観測できないため。

**どちらの枝を通ったかは、インストール後の状態からは復元できない。** 唯一の
観測点は `show_install_summary` が `id -u` 非ゼロのときだけ出す
"Ask for administrator rights" の行 (`install.sh:1086-1087`) なので、Linux と
macOS の両脚はこの行の有無を対で assert する。この 1 行が製品出力である以上、
文言を変えるときは同じ変更でアサートも動かす。

**Windows の UAC については、CI が検証するのは「遷移」であって「同意 UI」では
ない。** 無人で成功アームを通すには `ConsentPromptBehaviorAdmin=0`
("Elevate without prompting") が要り、それは同意ダイアログを消す。値はアーム
の間だけ設定して `finally` で戻す。ゲストは使い捨てなのでジョブ後に何も残ら
ないが、**測っている対象が変わることを暗黙にはしない** — だからこの決定に
書く。同意 UI そのものは人間が要るので実機チェックのまま残す。

拒否アームの `ConsentPromptBehaviorUser=0` ("Automatically deny elevation
requests") は性質が違う。これは企業の標準ユーザ端末の**実在の出荷構成**で
あり、標準ユーザの昇格要求が人間なしに決着する唯一の設定でもある。歪めて
いるのではなく、出荷しているのに一度も実行していなかった枝
(`install.ps1:1462-1480`) に到達するための構成である。

成功アームには**ランナー自身とは別の**ローカル管理者を作り、`schtasks` を
既定の実行レベル (`/RL HIGHEST` を付けない = limited) で起動する。管理者
グループのメンバーが UAC フィルタ済みトークンで走る唯一の自動化可能な形で
あり、同時に**インストールした管理者とコンソールユーザが別人**という状態を
作る — `install.ps1` の `Get-ConsoleUser` と `HKEY_USERS\<sid>` 書き込み
(`:2086-2093`, `:2212`) が存在する理由そのもので、already-admin 実行では
両者が同一人物なので永久に証明できない。

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
- Windows の UAC アームは**タイムアウトを FAIL として扱う**。タイムアウトは
  遅いマシンではなく「誰も答えられないダイアログが出ている」の署名であり、
  skip にすると、このアームが排除するために存在する条件そのものを隠す。
  抑制し損ねた MsgBox が run を 28 分刺した前例がある。
- アサート数の下限 (`installtest-run.sh` / `installtest-macos.sh` /
  `installtest-windows.ps1`) は、アサートを足した同じコミットで引き上げる。
  Windows の成功アーム分だけは `EnableLUA` の実測で条件付きにする — Admin
  Approval Mode が切られたランナーでは正当に skip されるので、固定値だと緑の
  run で下限が発火する。

## Related Records

- waired-agent#990 (Linux/macOS の起動形)、#991 (Windows の自己昇格経路)、
  #993 (非 root 起動で死んでいたトレイ拡張の有効化)、#44 (state-dir ACL)
- waired#759 (署名付き .exe への収束 — Phase 0 が署名調達)、waired#760
  (3-OS installtest を毎 PR・非昇格コンテキストを含む)
- #192 / #177 (`-StateFile` が存在する理由)、#315 / #653 (SAC が未署名
  バイナリをブロックしたときの診断)
- Microsoft, "User Account Control settings and configuration"
  (Local Policies\Security Options) — `ConsentPromptBehaviorAdmin` /
  `ConsentPromptBehaviorUser` / `EnableLUA` の値と意味
