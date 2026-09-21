# 製品の変更で踏む罠 (20260921 21:40)

## Issue

製品(CLI、インストーラ、daemon、tray、リリースの経路)を変える PR で、ローカルの
検査が全部緑のまま CI か実機で初めて落ちた罠を、変更の種類ごとにまとめる。
どれも一度は実際に踏んだもので、着手の前に該当する節だけ読めばよい。
正本が既にこのリポジトリの docs に在るものは、写さずにパスを指す。

出自: セッションのメモリに在った知見を、オーナーの指示
(2026-09-21、waired-ai/waired-ai#12)でリポジトリへ移した。

## Learnings

### 1. 製品の文字列は pin されている

いつ読むか: CLI の出力行、`install.sh` / `install.ps1` の文言、NAVI の文言を
変える前。

製品が出す行は、人だけでなくスクリプトとテストにも読まれている。文言を変えると、
ローカルの検査が全部緑でも CI の別のレグ(CI の 1 構成ぶんの実行)で落ちる。
CLI の文言変更(#1280 / #1281)で 3 回、インストーラの文言変更(#1289)で 1 回、
CI を赤くしたことがある。

読んでいるもの:

- `scripts/dev/lib/installtest-enroll.sh` の `IT_*_RE`。
- `scripts/dev/installtest-windows.ps1` / `installtest-macos.sh` /
  `installtest-run.sh`。**CI でしか走らない。** ローカルの `installtest-dash.sh`
  (当時 246/0)と `installtest-pwsh.ps1`(当時 67/0)が緑でも、Windows の
  install test はインストーラの出力へのインラインの照合
  (`ItSoft '832' ($script:InstallOut -match '...')`)で落ちた。
- 配布しているインストーラ自身。`packaging/install/install.sh` / `install.ps1` の
  daemon を待つループは `waired config log-level` の出力の形を読んでいる。
  こちらの失敗は文字列の不一致としては出ず、`install.ps1` の exit 1 や
  "did not become the persisted level" として出る。
- `scripts/ci/harness-failure-strings-guard.sh` が製品側と突き合わせるのは
  `IT_*_RE` の組だけ。ハーネスのインラインの grep と、インストーラが CLI の出力を
  読む箇所は、2026-09-21 の時点でどのガードも見ていない。

変える前にすること:

- `rg -F '<変える前の語句>' scripts/dev packaging/install`。push の前に、変えた行
  ごとに
  `grep -nE "<変える前の語句>" scripts/dev/installtest-{windows.ps1,macos.sh,run.sh}`。
  決まったキーワードの一覧で探すと、忘れた 1 行を見落とす。
- pin は、アポストロフィを含まない断片か、出力の形(例: `"Log level: "*"("*`)に
  張る。`IT_*_RE` は単一引用符の値なので、`'` を含む文字列を張れない。
- 引用符:
  - PowerShell の単一引用符の正規表現では `'` を `''` と書く。大文字小文字の
    違いを避けるなら先頭の 1 文字を落とす(`'he last install didn''t finish'`)。
  - sh は、リテラルに `$` `` ` `` `\` `"` が無ければ `"..."`、在れば `'\''`。
  - `installtest-dash.sh` の pin は複数行の単一引用符の文字列なので、開く側を
    `"` に変えたら閉じる側も変える(`bash -n` がすぐ見つける)。
  - Go の raw string にはバッククォートを書けない(Long help の本文)。
- 確認: `bash -n`、pwsh のパーサ、`bash scripts/ci/harness-failure-strings-guard.sh`。
- ガード自身のフィクスチャ(`scripts/ci/installer-copy-guard-test.sh` の形)に
  `https://example.invalid/...` と書くと、`invalid` を禁じる規則
  (`scripts/ci/installer-copy-rules.txt`)に当たる。`example.com` を使う。

次の文言は、CI の外に在る実機の検証手順が照合に使っている。変えると、その手順が
黙って外れる: `Ask for administrator rights`、`Waired is installed (macOS`、
`The Waired app is running in the menu bar`、
`Stopping the Waired app (waired-tray, PID <n>)`
(`packaging/debian/waired-tray/prerm` と install / uninstall の各スクリプトに
バイト単位で同じ文が在る)、`No terminal detected`、`Update available: X -> Y`、
`Control Plane URL:`、`State / identity:`。

NAVI(ブラウザの管理画面)の文言は private のリポジトリに在るが、このリポジトリの
`docs-site` がそれを引用している: `getting-started/set-up-in-the-browser`、
`getting-started/servers-and-auth-keys`、`troubleshooting/setup`、ja の
`quickstart` と `getting-started/sign-in`、`docs-site/TRANSLATION.md`。NAVI の
文言を決める前に両方のリポジトリを grep して pin の一覧(テストと docs)を作り、
docs の引用はこのリポジトリの PR で追う。辞書側のテストの pin は内部の記録を参照。
引用を製品の文字列と一緒に変える規則は CLAUDE.md §Vocabulary and provenance の
最後の項。

### 2. `waired init` のプロンプトを触る前に、全レグの argv を引く

いつ読むか: `waired init` のプロンプトの既定値、EOF の扱い、非 TTY の扱いを
変える前。

`cmd/waired/init_prompt.go` の `ynAsk` の doc コメントが制約を書いている:

> `waired init` does not force non-interactive mode off a terminal (it
> cannot: scripted installs pipe their answers in, and
> scripts/dev/installtest-windows.ps1 drives it that way)

origin/main で argv を引くと、パイプで答えを流しているレグは 1 本だけだった。
`scripts/dev/lib/installtest-enroll.sh` の `assert_engine_only_install`
(`printf '0\n' | waired init --inference-enabled=true …`)で、engine の問いは
フラグで先に答えられ、picker が `0` を読む。それ以外は全部 `--non-interactive` を
渡している(`installtest-windows.ps1` の `$initArgs`、`installtest-enroll.sh` の
authkey / interactive の本体、`assert_reinit_resumes`、
`assert_reinit_engine_optout`、`assert_reinit_default_unfit`)。

だから「非 TTY を非対話として扱う」はハーネスを壊し、「EOF(答えなし)に別の
意味を与える」は壊さない。プロンプトまわりを触るときは、argv の表を先に作る。

- **アサート数のフロア**: `scripts/dev/installtest-run.sh` の tier-2 のフロアは
  構成別(lean / `--engine-only` / INFER・DAEMON_ENGINE)。engine の無い構成の
  ブロック(`assert_reinit_engine_optout` など)は lean と `--engine-only` だけで
  走るので、そこに assert を足したら、その 2 つのフロアだけを上げる。
- **「出ないこと」を測る grep はガードに登録する**:
  `scripts/ci/harness-failure-strings-guard.sh` は 3 つのハーネスの文言の一致と、
  製品側にその文言が実在することを検査する。「出ないこと」を測る grep は、文言が
  変わると永久に緑になるので、「出ること」を測る grep 以上に登録が要る
  (`IT_ROLE_GUIDANCE_RE` はこの理由で足された)。

### 3. 単体テストでは EOF は Enter の代役にならない

いつ読むか: プロンプトを読む関数のテストを書く、または大量に落ちたとき。

`ynPrompt` / `readModelChoice` は EOF でも既定値を返していたので、テストは長く
「Enter を押した」を `eofLineReader()`(空の reader)で書いていた。#1048 で EOF を
「答えなし」として扱うようにした時点で 9 か所が落ち、すべて `linesOf("\n")` に
直すだけで、元の主張のまま緑に戻った。

- Enter は `enterLineReader()`、EOF は `eofLineReader()`
  (`cmd/waired/init_engine_ask_test.go`)。EOF の reader を残してよいのは、
  プロンプトに到達しない経路(フラグで先に答えている / 非対話の分岐 /
  早期 return)。そこでは「ここは読まない」の証明になる。
- 一般形: 製品が区別できない 2 つの入力を、テストが片方だけで代役していると、
  その差に意味を持たせた日に一斉に落ちる。落ち方が「大量だが機械的」なら、
  テストの反転を疑う前にフィクスチャを疑う。PR 本文には「反転ではなく
  フィクスチャの修正」と書く。「反転」と書くと、要件を変えたと読まれる
  (CLAUDE.md §Test discipline の Declare pins)。

### 4. インストーラ / init のフラグを撤去すると 4 つのハーネスが落ちる

いつ読むか: `install.sh` / `install.ps1` / `waired init` のフラグを消す前。

製品コードの側を全部直しても、CI でしか落ちないハーネスが 4 つ残る。
waired-ai/waired#1300 で `--share-with-mesh` を消したとき、install-scripts の
ジョブが 8 失敗で赤くなった。ローカルの `go test ./...` も `golangci-lint` も
緑のままだった。

消したフラグ名で `scripts/dev/` と `packaging/install/` を grep する。実際に
アサートしているのは:

- `scripts/dev/installtest-dash.sh` — dash / bash / busybox の 3 つのシェルで
  `run_case` にフラグを渡す。1 本のフラグで 3 ケースが落ちるうえ、同じ `out=` を
  共有する後続のケースが巻き添えで落ちる(8 失敗のうち 5 件が 1 本の原因だった)。
  「値が不正なら non-zero」型のケースは、フラグ自体が unknown argument になって
  **理由が変わったまま緑**になるので、消す。
- `scripts/dev/installtest-windows.ps1` / `installtest-pwsh.ps1` —
  `Invoke-Argtest` でフラグを渡し、`InitArgs=[...]` を正規表現で見る。
- `packaging/install/install.ps1` の `ARGTEST` の行 — **位置で一致を取る**。
  コメントが "Fields are append-only -- the harness matches on positions" と言う
  とおり、フィールドを削ると以降が全部ずれる。枠は残して空文字を出す。

`bash scripts/dev/installtest-dash.sh` はローカルで数秒で回る。フラグを消したら、
製品コードの grep のあとに上の 4 ファイルを名指しで確認し、これを 1 回回してから
push する。

### 5. 管理 API の読み取りは socket 経由

いつ読むか: `cmd/waired` に、daemon の管理 API を読む・書くコードを足すとき。

daemon は management socket が bind されている間、loopback の TCP では
`tcpReadRoutes`(`internal/management/socket.go`)の経路だけ GET を返し、ほかは
403 にする(`readGuard`、waired-ai/waired#836。daemon の `-mgmt-socket-reads-only` は
既定で true)。許可リストは `/waired/v1/` の下の
`status`、`inference/status`、`inference/runtimes`、`inference/catalog`、
`setup/state` の 5 本で、理由は同じファイルのコメントに在る。

- CLI の読み取りは `mgmtReadRoute`、書き込みは `mgmtWriteRoute`
  (`cmd/waired/main.go`)を通す。自前の `&http.Client{}` や `http.DefaultClient` を
  使うと、許可リストの外の経路が 403 になる。`waired peers list`、doctor の
  メッシュの行、Claude Code の statusline(毎ターン「agent down」)がこれで
  壊れたことがある(#785)。
- CI では赤くならない。CLI と daemon は、単体ではそれぞれ正しく動くため。
  止めているのは `scripts/ci/mgmtclientguard` の宣言表。`http.DefaultClient` の
  grep だけでは `&http.Client{}` の形を取りこぼす。
- `mgmtReadRoute` に bare の `host:port` を渡すと、`url.Parse` が
  「first path segment in URL cannot contain colon」を返す。`mgmtURL(mgmt, path)` で
  正規化する。`mgmtURL` は `doctor_*.go` / `init_resume.go` などで引数名に
  shadow されているので、新しい関数の引数は `mgmt` と命名する。
- `/waired/v1/ping` だけは TCP のまま書ける(`writeGuard` の `isPingProbe`)。
- socket に推測でパスを叩いて 404 が返っても、「その機能は提供されていない」とは
  限らない。パスが存在しないだけのことがある(`/waired/v1/peers` は元から無く、
  実体は `/waired/v1/inference/mesh`)。実在の経路は `internal/management/server.go` の
  `mux()` の登録を読む。

### 6. `agent.json` を daemon の外から読む・書く

いつ読むか: CLI やインストーラに、daemon の設定を読む・書く修正を入れる前。

**読む側。** CLI が `agent.json` を読む修正は、per-user のインストールでしか
成り立たない。system service の配備(Linux / Windows の既定の形)では:

- デスクトップユーザーの CLI は state dir を `~/.config/waired` と解決する。
  そこに `agent.json` は無い。
- daemon の設定は `/var/lib/waired/agent.json`(root:waired、読めない)。
  `/etc/waired/` に在るのは `agent.env` だけで、`/etc/waired/agent.json` を
  決め打ちすると「設定を変えたのに効かない」と誤診する。
- 結果として、CLI はコンパイル済みの既定値へ黙ってフォールバックする。

ユニットテストは通る(テストは同じ dir を両方に使う)。実機の service install で
しか出ない。#999 はこの形の修正がマージされ close されたあとで、実機で
`local_gateway_port=19473` を固定したら、daemon は 19473 で待ち受け、
`waired link` は 9473 を書き込んだ。

daemon 由来の値が要るときは、まず daemon に訊く口を探す。
`GET /waired/v1/integration/{opencode,openclaw}` が `expected_base_url` を返し、
`/run/waired/mgmt.sock` は `srw-rw-rw-` なのでデスクトップユーザーから届く。
CLI からは `mgmtReadRoute` 経由(この口は socket 限定で、素の TCP は 403 — §5)。
解決の順は daemon → 設定ファイル → 定数で、明示のフラグはすべてに優先する。
原則の記録は
`docs/decisions/20260822/1742-integration-rows-belong-to-the-desktop-user.md`。

**書く側。** `agent.json` が存在するだけで、初回ブートのモデル自動選択が走らなく
なる。`cmd/waired-agent/bundled_model_select.go` の
`shouldAutoSelectBundledModel(agentJSONExists, preferenceExists, intent)` は
`!agentJSONExists` を条件にしている(waired-ai/waired#756)。daemon の初回ブートの
前に `agent.json` が在ると、ハードウェアに合わせたモデル選択が恒久的に走らない。

CLI(`cmd/waired/config.go`)は、daemon が停止中だと `Defaults()` + `MergeJSON` +
`Save` で `agent.json` を**作る**フォールバックを持つ。インストーラから
`waired config ...` を呼ぶ案は、Windows の fresh install(登録のみで起動しない
区間)と macOS(RunAtLoad との競合)でこれを踏む。`Save` は全フィールドを書くので、
今日の既定値をディスクに固定する副作用もある(§7 の 2 つめ)。

- 設定ファイルに書く修正を入れる前に、`fileExists(<そのファイル>)` /
  `!...Exists` を grep して、ファイルの存在自体を条件にしている消費者を探す。
- 書き込みは稼働中の daemon 経由に限り、停止中のフォールバックには落とさない
  (Linux の daemon は `User=waired` なので、所有権も同時に解決する)。daemon が
  上がらなければ、書かずに警告する。待機の条件は `/waired/v1/status` ではなく、
  その書き込みが使う読み取り(例: `waired config log-level`)そのものにする。
  status は TCP の許可リストに在るので、IPC の socket がまだ無くても緑になる。
  決定の記録は
  `docs/decisions/20260821/0242-log-level-is-a-setting-not-a-service-flag.md`。

### 7. 死んでいたものを生かす修正

いつ読むか: 「常に nil / 常にエラー」だった経路を直す PR、消費者の居なかった
設定項目に消費者を付ける PR の前。

「意図した挙動が戻るだけ」は、戻った先が無変化であることを意味しない。

**死んでいたコードパスを生き返らせると、下流の前提が無効になる。**
実例(#803 → #815): `/waired/v1/identity` の 403 を直した結果、`reauthWanted` の
第 2 の分岐(daemon が `AuthStateReauthRequired` を報告)が、本番で初めて発火する
ようになった。#810 のゲートは `reauth` を「フラグで意思表明があった」と数えていて、
書かれた時点では正しかった。403 のせいで `Reauth` は実質 `--force-reauth` その
ものだったからだ。復活で、#782 が閉じた穴が再び開いた。どちらの PR も単体では
正しく、組み合わせで壊れた。

- 「常に nil / 常にエラー」を直す PR では、その述語が false に固定されていた期間に
  書かれた呼び出し元を全部辿る。grep で消費者を列挙し、1 つずつ「修正前は必ず
  どうだったか」「今は何が通るか」を表にする。見たが他に無かった、という結果も
  issue に残す。
- 逆側: 「今は必ず false」に依存する設計は、なぜ false なのかをコメントに書く。
- 特に見る先: 終了コードに効くか(`waired doctor` は `StatusFail` が 1 つでも
  あれば非ゼロ)、CI やインストーラのスクリプトがそのコマンドの終了コードを見て
  いるか。

**消費者ゼロだった設定項目に消費者を付けると、ディスクの旧既定が勝つ。**
`Defaults()` は、ファイルに無いキーのフォールバックでしかない。`Save()` が
`omitempty` 無しで書き出していた項目は、全ホストの `agent.json` に旧既定の実物が
残っている。後から消費者を付けると、新しい既定は新規インストールにしか届かない。

実例(#907): `Inference.IdleTimeout` は長く「宣言され、env と flag から読まれ、
既定 10m を持ちながら消費者ゼロ」で、実際の常駐は別の定数(60m)が決めていた。
#897 で配線した瞬間、`agent.json` に前から入っていた `"idle_timeout": "10m0s"` が
有効になり、既存ホストの常駐が 60 分から 10 分に短くなった。

- 設定項目に消費者を付ける前に、ディスクに何が書かれているかを実機で見る。
  更新前のホストを 1 台残しておくと、実物が証拠になる。旧挙動の実測は更新の前に
  しか採れない。
- 既定を変えるだけでは既存ホストに届かない。届かせるなら (a) JSON のキーを改名し、
  旧キーを意図的に読み込まない(消費者が無かった = どの値も運用者の選択では
  あり得ない、が根拠)か、(b) マーカー付きの一度きりの移行。ファイル単体では
  「いつ書かれたか」を答えられないので、値だけを見て書き換える案にはマーカーが
  要る。

### 8. tray のラベルのエスケープ

いつ読むか: tray のメニューに、`&` や `_` を含み得る文字列(モデル ID、
variant(量子化ビルド)の名前、デバイス名)を出すとき。

機構と裁定はこのリポジトリに在る(#1096 / #1100、実装済み)。写さない:

- `docs/knowledges/20260828/0510-menu-labels-are-markup-on-two-of-three-os.md` —
  Windows は `&` を、Linux(dbusmenu の `label`)は `_` を食う。エスケープは
  描画の直前(`internal/gui/tray/rows.go` の `setTitle`)で行う。
- `docs/decisions/20260828/2140-a-menu-label-is-written-for-its-renderer.md` と
  `docs/knowledges/20260828/2145-writing-a-menu-label-per-renderer.md` — `_` の
  エスケープは Plasma / GTK と gnome-shell で形が違い、描いている相手は D-Bus で
  特定する。Linux の `GetLayout` の `label` はエスケープ後なので、照合の正本は
  `WAIRED_TRAY_DEBUG=1` の JSON、という注意も 2145 に在る。
- グレーの行の規則は
  `docs/decisions/20260828/0320-grey-means-unavailable-in-the-tray.md` と
  `scripts/ci/tray-grey-row-guard.sh`。グレーの実測は MSAA の `accState & 0x1` /
  dbusmenu の `enabled`。
- `fyne.io/systray` の `Quit()` が一回限りである件は
  `docs/decisions/20260827/2307-uninstall-stops-the-running-tray.md` と
  `internal/gui/tray/shutdown.go` / `tray.go` のコメント。

上の記録に無い、検証の手順:

- 実機のメニューに、検証したい文字が 1 つも無いことがある(検証に使った Linux の
  ホストでは、メニュー 153 行に下線が 0 個だった)。`scripts/dev/mock-mgmt` の前段に、
  値を書き換える 40 行ほどの TCP プロキシを置き、
  `waired-tray -mgmt http://127.0.0.1:9477` で接続すると、実コード・実 dbusmenu・
  実 GNOME のまま、狙った文字列を出せる(`internal/gui/tray/mgmt.go` の `NewClient`
  は、既定の authority 以外なら socket でなく素の TCP を使う)。
- 判定は `GetLayout` を python / Gio で歩いて `label` を採り、インストール済みの
  拡張の正規表現を gjs にそのまま流す。
- ガードを書くときの注意: `find | xargs grep` は、ファイルが 1 個だとファイル名を
  出さない。`file:line:text` を前提にした判定が読み違えるので、`grep -H` を付ける。

### 9. `waired update` はリリースのインストーラを走らせる

いつ読むか: インストーラの修正が「届いたか」を判断するとき、`waired update` の
直後の挙動を診断するとき。

`waired update` が走らせるインストーラは、常に「そのチャンネルの現在のリリース」
からダウンロードしたもの。ホストに入っている `install.ps1` / `install.sh` は
使わない。

```go
// cmd/waired/update_client.go
scriptPath, err := downloadInstaller(goos, update.ScriptURLForChannel(goos, hostChannel))
// internal/update/script.go — edge: releases/download/edge、ほか: releases/latest/download
```

temp に落として `-File` / `sh` で実行する。昇格した側も同じ temp のファイルを
走らせる(`Invoke-SelfElevate` は `$PSCommandPath` を再起動する)。
`%ProgramFiles%\Waired` は読まない。

- インストーラの欠陥は、修正を含むリリースが公開された後の最初の更新で、自動的に
  直る。
- **`edge` は main より遅れる。** マージの直後、`edge.yml` の再ビルドが終わるまでは、
  修正済みの main を見ながら、修正前のインストーラが配られている。「main にマージ
  した」は「ホストが安全になった」ではない。境目はリリース資産の再ビルド。
- stable の経路にこの遅れは無い(タグを切れば、`releases/latest/download` はその
  タグの資産)。インストーラの修正の実機確認は、stable の経路のほうが安全。

配られている実物を確かめる手順(run 番号やタグではなく、後ろほど強い):

```sh
gh release view edge --repo waired-ai/waired-agent --json body --jq .body | grep 'Built from'
git merge-base --is-ancestor <fix-sha> <その sha>
curl -fsSL https://github.com/waired-ai/waired-agent/releases/download/edge/install.ps1 \
    -o /tmp/x.ps1                                   # 実物を落として
grep -c '<新しい関数名>' /tmp/x.ps1                  #   中身を見る
powershell.exe -File scripts/dev/installtest-swap.ps1 -InstallPs1 /tmp/x.ps1  # 回帰テストを掛ける
```

落とした資産そのものを Windows PowerShell 5.1 でテストに掛けるのが、実機に触る前に
取れる最も強い証拠。「main が直っている」ではなく、「ホストが実際に取ってくる
バイト列が正しく振る舞う」まで見える。

更新の直後の、欠陥と誤診しやすい挙動:

- エンジンの pin が動いた後の初回の手動 `waired update`(Linux)では、root の
  インストーラと daemon(waired ユーザー)のバックグラウンドの収束処理が、同じ
  staging ディレクトリを取り合い、daemon 側に `permission denied` の WARN が 1 本
  出る。root 側が勝って正しく入り、`.stage` も掃除される。
- Windows では、pin が動いた後の update の直後に `waired runtimes ls` が
  `(no runtimes detected)` を、`doctor` が network の finding を 1 件返す。
  数分後にはエンジンが ready、doctor が 0 件に落ち着く。判定は数分置いてから。

### 10. `v*` タグはチャンネルを持たない

いつ読むか: プレビュー用のビルドを配りたくなったとき、`v*` タグの名前に意味を
持たせたくなったとき。

`v*` タグは、パイプラインがタグ名の中身を見ていない。
`reusable-build-artifacts.yml`(`scripts/ci/resolve-build-version.sh`)が `v` を
剥がして版番号にするだけで、`-dev` などの接尾辞による分岐は workflow / Makefile /
build tag のどこにも無い(2026-09-21 に main で再確認)。

- `release.yml` の `gh release create` には `--prerelease` も `--latest=false` も無い。
  どの `v*` タグも GitHub の Latest になり、公開のワンライナー
  `/releases/latest/download/install.sh` の配信先が入れ替わる(`edge.yml` は
  prerelease を明示している)。
- `.deb` は無条件に、公開 APT リポジトリの stable suite へ上がる(`release.yml` の
  apt upload のステップ)。
- 接続先の CP はビルドに焼かれない。`internal/controlurl` の `Default` は
  `https://app.waired.ai` の定数で、開発用の CP は install 時の `--dev` / `-Dev` が
  `agent.env` に `WAIRED_CONTROL_URL` を書いて選ぶ。タグ名では開発用にならない。
- server 側(private のリポジトリ)のタグの扱いはこれと同じではない。内部の
  記録を参照。

プレビューを配る経路は `edge`(main のマージごとに再ビルドされる prerelease)。
タグ名に意味を持たせるなら、先に `release.yml` へ条件付きの `--prerelease` を足す
変更が要る。

deb の版の並び(SemVer の `-` を Debian の `~` に書き換える、0.0.3 系列へ繰り上げた
理由)は #780 で修正済み。記録は
`docs/decisions/20260815/0245-deb-version-tilde-and-0-0-3-series.md`、実装は
`scripts/ci/resolve-build-version.sh`。

### 11. 端末を読んで止まる詰まりは、制御端末が無いと再現しない

いつ読むか: インストーラや `waired update` の「固まった」という報告を、実機で
再現しようとするとき。

`ssh host cmd`(`-t` 無し)、cron、systemd のユニット、`setsid` には制御端末が無い。
制御端末が無ければ「バックグラウンドのプロセスグループ」が成立せず、カーネルは
SIGTTIN を生成しない(端末の読み出しは、停止ではなく EIO / EOF になる)。
したがって、端末を読んで止まる型の詰まりは、実機に ssh でコマンドを流す既定の
形では再現できない。「実機で試したら起きなかった」は、条件を満たしていないだけの
ことがある。CI も同じ理由で緑のままになる。

実例は #1097(修正済み): `timeout` 配下の `apt-get` が needrestart のプロンプトで
SIGTTIN 停止し、同じプロセスグループの `timeout` も止まって、上限が発火しなかった。
機構と修正(`NEEDRESTART_SUSPEND=1` と `</dev/null`)は
`packaging/install/install.sh` の `apt_bounded` のコメントに在る。手で
`sudo waired update` を叩いた人だけが踏み、自動更新は無傷だった。

制御端末つきで再現する手順(sudo のパスワードを pty にエコーさせない形):

```sh
printf '%s\n' "$PASS" | ssh host 'sudo -S -p "" setsid --fork script -qec "<コマンド>" /path/log'
```

sudo がパイプからパスワードを 1 行読み、そのあと `script` が新しい pty を割り当てる
ので、子は制御端末を持つ。`ssh -tt` でパスワードを流す形と違い、typescript に
パスワードが残らない。

診断: `ps -eo pid,pgid,tpgid,stat,args`。`TPGID` は制御端末のフォアグラウンドの
プロセスグループなので、`STAT=T` かつ `TPGID != PGID` なら「端末に対して背面にいて、
読んで止められた」。制御端末が無いプロセスは `TPGID = -1`。止まったグループは
`sudo kill -9 -<pgid>` でグループごと殺す。

### 12. `security(1)` のクエリの形と終了コード

いつ読むか: macOS の Keychain に古い版が残した項目が在るかを調べるとき。

機構の正本は `docs/knowledges/20260821/2230-security-cli-reports-twice.md`(#799)。
終了コードは OSStatus を 256 で割った余りで、文面と一致しないことがある。
すぐ要る換算だけ再掲する: **36** = `errSecInteractionNotAllowed`(セッションが無く、
プロンプトを出せない)/ **44** = `errSecItemNotFound` / **45** =
`errSecDuplicateItem` / **195** = `wrPermErr`(本物の失敗。stderr は成功時と同じ
`password has been deleted.` だけ)。終了コードを先に、文面は後に読む。

上の記録に無い点:

- **在庫確認はクエリの形を間違えやすい。** `-s waired/gateway-token` のように
  「account/service」を service に渡すと、全部 44 が返り、「項目はゼロ」という
  誤った結論になる。正しい形は `-a waired -s gateway-token`。この形の間違いで、
  項目が在るのに「無い」と報告したことがある。「無い」と言う前に、書き込む側の
  コードが account と service に何を渡しているかを読む。
- Keychain の層は撤去済み
  (`docs/decisions/20260822/0357-secrets-are-files-on-every-os.md`)。いま
  `security` を叩く場面は、古い版が残した項目の在庫確認と、CA 証明書の導入
  (`internal/proxy/trust/install_darwin.go`)だけ。

## Refs

- CLAUDE.md §Test discipline、§Vocabulary and provenance、§Tags / releases
- docs/decisions/20260821/0242-log-level-is-a-setting-not-a-service-flag.md
- docs/decisions/20260822/1742-integration-rows-belong-to-the-desktop-user.md
- docs/decisions/20260822/0357-secrets-are-files-on-every-os.md
- docs/decisions/20260815/0245-deb-version-tilde-and-0-0-3-series.md
- docs/decisions/20260828/2140-a-menu-label-is-written-for-its-renderer.md
- docs/decisions/20260828/0320-grey-means-unavailable-in-the-tray.md
- docs/decisions/20260827/2307-uninstall-stops-the-running-tray.md
- docs/knowledges/20260828/0510-menu-labels-are-markup-on-two-of-three-os.md
- docs/knowledges/20260828/2145-writing-a-menu-label-per-renderer.md
- docs/knowledges/20260821/2230-security-cli-reports-twice.md
- scripts/ci/harness-failure-strings-guard.sh, scripts/ci/mgmtclientguard/main.go
- scripts/dev/lib/installtest-enroll.sh, scripts/dev/installtest-run.sh,
  scripts/dev/installtest-dash.sh, scripts/dev/installtest-swap.ps1
- internal/management/socket.go, cmd/waired/main.go (`mgmtReadRoute` / `mgmtWriteRoute`)
- cmd/waired/update_client.go, internal/update/script.go
- packaging/install/install.sh (`apt_bounded`)
- waired-agent の issue / PR: #785, #780, #782, #799, #803, #810, #815, #897, #907,
  #999, #1048, #1096, #1097, #1100, #1280, #1281, #1289
- waired-ai/waired#756, waired-ai/waired#836, waired-ai/waired#1300
