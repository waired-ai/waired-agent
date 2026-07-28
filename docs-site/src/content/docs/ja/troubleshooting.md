---
title: うまくいかないとき
description: いま実際に起きている症状を自分の言葉で探して、直すための手順を 1 つだけ見つけます。
meta:
  audience: Waired の様子がおかしい人
  needs: 対象のパソコンのターミナル
  time: 症状を探す。各対処は 1〜2 分
sourceHash: b94fa07ad51e7fbe
---

<!-- 症状ファースト。読者が分かるのは「何が見えているか」であって、どの機能の
     問題かではない。そのため索引は読者の言葉で書き、各項目は 1 つの対処へ導く。

     各見出しの直前に英語の id を置いてあるのは、他ページ（EN / JA 双方）からの
     アンカーリンクを言語間で同じ形に保つため。見出しを日本語にすると自動生成の
     id が変わってしまい、リンクが切れる。 -->

## まずこれ

```sh
waired doctor
```

セットアップの各部分を点検し、✓ / ⚠ / ✗ で表示します。**f** キーを押すと、
直せるものは自動で修復します。このページの他の項目より先に実行してください。
たいていはこれだけで解決します。

## 症状から探す

**セットアップ中**

- [`waired` と入力したら「コマンドが見つかりません」と出た](#i-typed-waired-and-got-command-not-found)
- [サインインでブラウザが開かない／別のブラウザが開く](#no-browser-opened-at-sign-in)
- [常駐サービスが応答せずサインインが止まる](#sign-in-stops-because-the-background-service-is-not-responding)
- [セットアップが途中で止まった](#setup-stopped-partway)
- [デバイス数の上限に達したと言われた](#it-says-i-have-reached-the-device-limit)
- [「enrolled system-wide」と表示される](#it-says-the-device-is-enrolled-system-wide)
- [「非常に小さいモデルしか動かせない」と言われた](#it-said-my-machine-can-only-run-a-very-small-model)
- [セットアップで「テスト生成を完了できませんでした」と出た](#setup-said-it-could-not-complete-a-test-generation)

**応答がない**

- [応答が返ってこない / Engine が not ready のまま](#no-answer-comes-back)
- [Claude Code がクラウドを使い続ける](#claude-code-is-still-using-the-cloud)
- [「waired-agent is not running」と出る](#a-command-says-waired-agent-is-not-running)
- [macOS で常駐サービスが一度も起動しない](#macos-the-background-service-never-starts)
- [Windows で 502 エラーになる](#windows-i-get-a-502-error)

**遅い・おかしい**

- [応答がとても遅い](#answers-are-very-slow)
- [グラフィックボードが使われていない](#my-graphics-card-is-not-being-used)
- [ハードウェアより大きいモデルを選んでしまった](#i-chose-a-model-bigger-than-my-hardware)
- [長い Claude Code のセッションが要約される](#long-claude-code-sessions-get-summarized)

**ほかのパソコン**

- [ほかのパソコンから AI に届かない](#my-other-computer-cannot-reach-the-ai)

**アプリ本体**

- [Waired のアイコンが出ない（Linux）](#the-waired-icon-is-missing-linux)
- [Claude Code にステータス行が出ない](#the-status-line-does-not-show-up-in-claude-code)

---

<a id="i-typed-waired-and-got-command-not-found"></a>

## `waired` と入力したら「コマンドが見つかりません」と出た

インストールが完了していないか、インストール前から開いていたターミナルが
新しいコマンドをまだ認識していないかのどちらかです。

1. **ターミナルを閉じて開き直してください。** 起動中のシェルはコマンドの場所を
   記憶しているため、多くはこれだけで解決します。
2. それでも出ない場合は、インストールコマンドをもう一度実行してください
   （[インストール](/ja/getting-started/install/)）。2 回実行しても安全です。

Windows ではコマンドの実体は `C:\Program Files\Waired\waired.exe` です。
`waired` だけで動かない場合も、このフルパスなら必ず動きます。

<a id="no-browser-opened-at-sign-in"></a>

## サインインでブラウザが開かない／別のブラウザが開く

サインイン用のリンクは、ブラウザが開くより先に必ずターミナルに表示されます。
そのため、いつでも手動で進められます。表示されたリンクをコピーして、普段使って
いるブラウザに貼り付けてください。サインインはそのブラウザで行い、以降の
セットアップも同じブラウザのまま進めてください。セットアップ画面は、
サインインしたブラウザでしか開けません。

普段使っていないブラウザが開いてしまった場合も同じです。サインインせずに閉じて、
表示されたリンクを使いたいブラウザで開いてください。

どちらも管理者権限でセットアップを実行したときに起きていたもので、最新版では
修正済みです。

```sh
waired update
```

<a id="sign-in-stops-because-the-background-service-is-not-responding"></a>

## 常駐サービスが応答せずサインインが止まる

サインインは常駐サービス**を通して**行われます。Waired と通信するのも、その後
このパソコンを接続し続けるのも常駐サービスです。応答がないときは、サービス抜きで
続行せずにサインインを中止します。

```
Waired's background service is installed but isn't responding, so sign-in can't continue.
  Check what's wrong:  waired doctor
  Start it:            sudo systemctl start waired-agent
  Then run again:      sudo waired init
```

表示された 3 行を上から順に実行してください。実際の原因は `waired doctor` が
教えてくれます。macOS でよくあるのは[常駐サービスが一度も起動していない
場合](#macos-the-background-service-never-starts)です。

以前のバージョンは、この状況でもそのままサインインしていました。一見うまく
いったように見えますが、実際にはサインイン済みなのにブラウザでセットアップを
完了できない状態になり、しかもその理由がパソコン側には何も出ませんでした。
対処できるメッセージを出して止まるようにしたのは、そのためです。

**「Waired isn't running in the background」**と表示された場合は、このパソコンに
常駐サービスがそもそも登録されていません。通常はインストーラを使わずプログラムを
直接実行しているときに出ます。先に `waired-agent` を起動してから、`waired init`
をもう一度実行してください。

<a id="setup-stopped-partway"></a>

## セットアップが途中で止まった

セットアップ画面に、何が起きたかが表示されます。メッセージごとに意味が決まっています。

| 表示 | 意味 | 対処 |
|---|---|---|
| The setup command on … was closed before this finished. Your progress was saved. | セットアップを実行していたターミナルが閉じられた。管理者権限が必要な工程は、そのウィンドウだけが担当している。 | `sudo waired init`（Windows は管理者プロンプトで `waired init`）をもう一度実行。続きから再開し、進捗は失われません。 |
| Setup on … needs administrator access to continue. | 管理者権限なしで開始された。 | 管理者のターミナルから開始し直してください（[サインインとセットアップ](/ja/getting-started/first-run/)）。 |
| … has run out of disk space. | モデルが入りきらなかった。 | 空き容量を作るか、[カタログ](/ja/reference/model-catalog/)から小さいモデルを選びます。 |
| … could not finish downloading. Check its internet connection. | ダウンロードが中断された。 | 再試行してください。最初からではなく途中から再開します。 |
| The AI software on … has not started yet. | エンジンがまだ起動中。 | 1 分ほど待って再試行。続く場合は[応答が返ってこない](#no-answer-comes-back)へ。 |
| This took too long on … and was stopped. | ある工程が制限時間を超えた。 | 再試行してください。同じ工程で 2 回起きる場合、そのモデルにはこのマシンが遅すぎる可能性が高いです。 |

なお**モデルのダウンロードだけは例外**で、ブラウザのタブを閉じても続きます。
[app.waired.ai](https://app.waired.ai) でそのデバイスを開けば途中経過を確認できます。

<a id="it-says-i-have-reached-the-device-limit"></a>

## デバイス数の上限に達したと言われた

1 アカウントで十分な台数を登録できますが、たいていは使わなくなった古いマシンが
残ったままになっているのが原因です。

[app.waired.ai](https://app.waired.ai) を開き、不要なデバイスを削除してから
もう一度セットアップしてください。

**すでにサインイン済み**のマシンでセットアップをやり直す分には、上限に数えられません。

<a id="it-says-the-device-is-enrolled-system-wide"></a>

## 「enrolled system-wide」と表示される

エラーではありません。デバイスの識別情報は管理者しか読めないシステム領域に保存されて
いるため、一般ユーザーとして実行した `waired status` からは見えません。推測で答える
代わりに「このデバイスは登録済みです」と伝えて正常終了しています。

すべて表示するには管理者権限で実行してください。

```sh
sudo waired status          # Windows は管理者プロンプトから
```

代わりに `Not enrolled. Run 'waired init' to connect this device.` と出た場合は、
本当にまだセットアップされていません
（→ [サインインとセットアップ](/ja/getting-started/first-run/)）。

<a id="it-said-my-machine-can-only-run-a-very-small-model"></a>

## 「非常に小さいモデルしか動かせない」と言われた

その判断は信頼してください。そのサイズのコーディングモデルは、役に立つ出力より
壊れた出力のほうが多くなります。既定が「いいえ」なのはそのためです。

それでもこのマシンをネットワークに入れる価値はあります — ほかのパソコンの AI を
使えるからです。どうしても入れたい場合は `--inference-enabled=true` を付けて
セットアップし直してください。

<a id="setup-said-it-could-not-complete-a-test-generation"></a>

## セットアップで「テスト生成を完了できませんでした」と出た

セットアップの最後に、Waired はこのパソコンの速さを測るため AI に短い質問を
します。このメッセージは、質問はしたものの答えが返ってこなかった、という意味
です。測定できなかったので、Waired は「動いています」とは表示しません。

ほとんどの場合、AI エンジン自体が停止しています。確認してください。

```sh
waired status
waired doctor
```

`waired status` のエンジンの行に、エンジン自身が報告した理由が出ます。
クラッシュしていた場合、詳細はエンジンのログにあります
（[さらに詳しく調べる（ログ）](#going-deeper-logs) を参照）。

Waired のほかの機能には影響ありません。デバイスはサインインしたままで、
ほかのパソコンで動いている AI は引き続き使えます。エンジンが正常に戻ったら、
次のコマンドで測定し直せます。

```sh
waired runtimes benchmark
```

<a id="no-answer-comes-back"></a>

## 応答が返ってこない

エンジンの状態を確認します。

```sh
waired status --observability
```

見るべきは **Engine** の行です。

- **`ready`** — モデルは読み込まれています。それでも失敗するなら、原因は経路側です
  → [Claude Code がクラウドを使い続ける](#claude-code-is-still-using-the-cloud)。
- **`not ready`** — 多くはまだダウンロード中です。`waired models ls` で進捗を確認して
  ください。最初のモデルは数 GB あります。
- **ダウンロード完了後も `not ready`** — そのモデルがメモリに収まっていない可能性が
  高いです。小さいものに変更してください
  → [使う AI モデルを選ぶ](/ja/guides/choose-a-model/)。
- **`engine failed`** — AI エンジンが自分で停止しました。Waired が自動で再起動する
  ため（最大 3 回）、通常は 1 分以内に復帰します。停止した理由は同じ行に表示されます。
  繰り返す場合は自動再起動を止めてそのことを表示します。理由が指している問題を直して
  から、次のコマンドで起動してください。

  ```sh
  waired inference engine start
  ```

  このパソコンに対してモデルが大きすぎるのが典型的な原因です。詳細はエンジン自身の
  ログにあります（[さらに詳しく調べる（ログ）](#going-deeper-logs)）。エンジンが
  停止している間、このパソコンはほかのマシンへの AI 提供を停止するため、ほかの
  マシンは待たされずに別の経路へ切り替わります。

知っておくとよい原因が 2 つあります。

- モデルの**初回**読み込みは遅く（GPU でも 1 分前後）、固まったように見えます。
  自動で復帰します。
- **503** が返る場合は、ルーティングが一時停止中（`waired resume`）か、共有が
  オフ（`waired inference share on`）です。

それでも解決しない場合、`waired runtimes status` がエンジン自体の状態を、
[ログを見る](#going-deeper-logs)がより詳しい情報を提供します。

<a id="claude-code-is-still-using-the-cloud"></a>

## Claude Code がクラウドを使い続ける

```sh
waired doctor          # f キーで見つかった問題を修復
waired claude status
```

`waired doctor` は、Claude Code と Waired の接続が壊れている場合に再構築します。
`waired claude status` が「連携が無効」と表示する場合は有効化し、Claude Code の
セッションを再起動してください。

```sh
sudo waired claude enable     # Windows は管理者プロンプトから
```

`waired claude status` は、**直近のフォールバック**とその理由も表示します。

- `local_no_model` — このデバイスでまだモデルが動いていない
  → [応答が返ってこない](#no-answer-comes-back)
- `local_status_<コード>` — フォールバック直前にローカル側がそのエラーを返した。
  詳細は `waired status --observability`

クラウドへのフォールバックは意図的な設計です。失敗させるより作業を続けられることを
優先し、**起きたことは必ず知らせます**。

<a id="a-command-says-waired-agent-is-not-running"></a>

## 「waired-agent is not running」と出る

常駐サービスが停止しています。

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows（管理者）
```

macOS ではシステムが自動的に再起動します。戻らない場合は `waired doctor` を実行するか、
パソコンを再起動してください。一度も起動していない場合は
[次の項目](#macos-the-background-service-never-starts)を参照してください。

再起動は一時的な不整合の多くを解消するので、込み入った対処の前に試す価値があります。

<a id="macos-the-background-service-never-starts"></a>

## macOS で常駐サービスが一度も起動しない

インストールは完了したのに常駐サービスが立ち上がらず、`--clean` を付けて入れ直しても
変わらない — この組み合わせはたいてい、macOS 側でサービスが**無効（disabled）**として
記録されていることを意味します。2026 年 7 月 15 日から今回のリリースまでの間に
インストールして削除した Waired は、この記録を残していました。アンインストールや
再インストール、パソコンの再起動でも消えません。

確認方法:

```sh
sudo launchctl print-disabled system | grep waired
```

`"com.waired.agent" => true` と出れば無効化されています。記録を消してサービスを
起動します:

```sh
sudo launchctl enable system/com.waired.agent
sudo launchctl bootstrap system /Library/LaunchDaemons/com.waired.agent.plist
```

現在は Waired のインストール／アップデート時に自動で解除されるので、これらのコマンドが
必要なのはインストーラー自体を実行できないパソコンだけです。

<a id="windows-i-get-a-502-error"></a>

## Windows で 502 エラーになる

このパソコンに AI ソフトが入っていません（多くは `-SkipOllama` または
`WAIRED_NO_OLLAMA=1` でインストールしたためです）。

管理者プロンプトから:

```powershell
waired runtimes install ollama
```

<a id="answers-are-very-slow"></a>

## 応答がとても遅い

```sh
waired runtimes benchmark
```

このパソコンの実際の速度を測ります。コーディング用途に必要な水準を下回る場合は、
より軽いモデルが提案されます。受け入れるのが妥当なことがほとんどです。

ほかに確認する点:

- **グラフィックボードが使われているか**
  → [グラフィックボードが使われていない](#my-graphics-card-is-not-being-used)
- **モデルがメモリに対して大きすぎないか** — はみ出した分は CPU で処理されるため
  劇的に遅くなります。`waired models ls --detail` で収まり具合を確認できます。
- **ほかのパソコンが答えていないか** — `waired infer --explain "hi"` が、
  応答したマシンと推定遅延を表示します。

<a id="my-graphics-card-is-not-being-used"></a>

## グラフィックボードが使われていない

まず、Waired が何を見つけているかを確認します。

```sh
waired models ls --detail
```

1 行目にカード名と搭載メモリが出ます。カードがあるのに `no GPU` と表示される場合、
カードは検出できていません。渡されたモデルを含め、その後の判断はすべて CPU 前提で
サイジングされています。

よくあるケースは Waired が自動処理します。統合 GPU（AMD / Intel）は Vulkan 経由で
有効化し（最近の Ollama は既定で無効にし、黙って CPU にフォールバックします）、
単体の AMD カードは対応していれば ROCm、うまく動かない場合は Vulkan に切り替えます。

NVIDIA のカードはドライバ自身に問い合わせて検出します。`PATH` 上に `nvidia-smi` が
あるかは見ません — 常駐サービスは端末の `PATH` を引き継がないため、`PATH` に無くても
カードは見つかります。それでも検出されない場合は、ツールの場所を直接指定して
サービスを再起動してください。

Linux では `sudo systemctl edit waired-agent` に次を追加します。

```ini
[Service]
Environment=WAIRED_NVIDIA_SMI=/usr/bin/nvidia-smi
```

Windows では管理者権限の PowerShell で次を実行します。

```powershell
[Environment]::SetEnvironmentVariable(
  'WAIRED_NVIDIA_SMI', 'C:\Windows\System32\nvidia-smi.exe', 'Machine')
```

そのうえでサービスを再起動し（[「waired-agent is not running」と出る](#a-command-says-waired-agent-is-not-running)）、
もう一度 `waired models ls --detail` を実行します。Windows では、マシン全体の環境変数を
サービスに確実に反映させるには再起動が最も確実です。

Waired の外で自前の Ollama を動かしている場合は、自分で `OLLAMA_IGPU_ENABLE=1` を
設定して再起動してください。

モデルが本当に収まっているかも確認してください
（要件は[モデルカタログ](/ja/reference/model-catalog/)）。

<a id="i-chose-a-model-bigger-than-my-hardware"></a>

## ハードウェアより大きいモデルを選んでしまった

Waired は警告しますが、禁止はしません。超過分（`needs 32 GB RAM (have 31 GB)` など）を
表示して確認を求めます。

- **少し超えている程度** — たいてい動きます。単に遅くなります。
- **本当に大きすぎる** — エンジンが読み込みに失敗し、明確なエラーを返します。
  小さいモデルに戻してください → [使う AI モデルを選ぶ](/ja/guides/choose-a-model/)。

推奨値には安全マージンが含まれています。Apple Silicon と AMD Strix Halo では、
GPU 側が実際に扱えるメモリ量で判定します。`waired models ls --detail` で、
このマシンにおける全モデルの判定を確認できます。

<a id="long-claude-code-sessions-get-summarized"></a>

## 長い Claude Code のセッションが要約される

正常な動作です。ローカルのモデルはクラウドのモデルより一度に保持できる会話量が
少ないため、Waired が実際の上限を Claude Code に伝え、Claude Code が古いターンを
要約して収めます。冒頭を黙って失うのではなく、セッションが生き延びているということです。

一瞬「prompt is too long」と表示されても、Claude Code が自動で再試行します。

しばらく大きなウィンドウを使いたい場合は `/waired-route anthropic` で本来の
Anthropic API に送れば、次のメッセージから本来のウィンドウが適用されます。

<a id="my-other-computer-cannot-reach-the-ai"></a>

## ほかのパソコンから AI に届かない

```sh
waired status --observability
```

**Mesh** の行が `enrolled / reachable / ready` です。`reachable` が 0 の場合:

1. **両方のパソコンが同じ Google アカウントでサインインしていますか。** これが
   圧倒的に多い原因です。各マシンで `waired status` のアカウント行を見比べてください。
2. **相手のパソコンは起動していて、Waired が動いていますか。** そちらで
   `waired doctor` を実行します。
3. **共有はオンですか。** ほかの端末に応答するには共有が必要です:
   `waired inference share on`

届いてはいるが `ready` にならない場合は、そのマシンにモデルが読み込まれていません。
そちらで[応答が返ってこない](#no-answer-comes-back)を順に確認してください。

ポート開放や VPN の設定は不要です。ネットワークが許せば直接つながり、ファイアウォールが
邪魔する場合は暗号化された中継に自動で切り替わります。

<a id="the-waired-icon-is-missing-linux"></a>

## Waired のアイコンが出ない（Linux）

GNOME は拡張機能なしでは時計のとなりにアイコンを表示しません。Waired アイコンには AppIndicator 拡張が必要です。
セットアップは、そのパソコンに GNOME が入っていれば導入します。サインインのたびに確認し直すので、
拡張が入っているのに無効になっている場合は自動で有効に戻ります。

それでもアイコンが出ない場合は、これで直ります:

```sh
waired doctor --fix
```

何が問題かを表示し、変更する前に確認を取ったうえで、必要に応じて拡張の導入・有効化を行います。

同じことを手動で行う場合:

```sh
sudo apt install gnome-shell-extension-appindicator
gnome-extensions enable appindicatorsupport@rgcjonas.gmail.com
```

そのあと**ログアウトして入り直してください**（Wayland では必須です）。

KDE Plasma では何も必要ありません。MATE では表示できません。

<a id="the-status-line-does-not-show-up-in-claude-code"></a>

## Claude Code にステータス行が出ない

**プロジェクトのディレクトリ内で** `waired claude status` を実行してください。

Claude Code はステータス行を 1 つしか使わず、プロジェクト直下の設定
（`.claude/settings.json` や `.claude/settings.local.json`）が、Waired がユーザー単位で
入れた設定より優先されます。その場合、コマンドが優先されているファイル名と、
自分のステータス行スクリプトに追加できる 1 行を表示します。

連携を有効にしたあとに Claude Code のセッションを再起動したかも確認してください。

---

<a id="going-deeper-logs"></a>

## さらに詳しく（ログ）

`waired doctor` を実行したあとで:

| | |
|---|---|
| Linux | `journalctl -u waired-agent -e` |
| macOS | `/Library/Logs/waired-agent.err.log`、または `sudo log show --predicate 'process == "waired-agent"' --last 10m` |
| Windows | `Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50` |
| AI エンジン | Waired の状態ディレクトリ配下の `…/runtimes/ollama/logs/engine.log`（Linux は `/var/lib/waired/…`、macOS は `/Library/Application Support/waired/…`）。自前の Ollama を使っている場合は `~/.ollama/logs/server.log`。 |

## 不具合を報告する

[不具合を報告する](/ja/getting-started/report-a-problem/)の手順に従ってください。
再現する**前に**詳細ログを有効化し、1 つのファイルにまとめて添付します。この順番が
重要です — 原因を説明するだけの詳細は、先に頼まないと記録されません。

`waired init --mask-pii`（ほかのコマンドでは環境変数 `WAIRED_PII_MASK=1`）を使うと、
ホームディレクトリ・ユーザー名・ホスト名・アカウントのメールアドレスが伏せられるので、
出力やスクリーンショットをそのまま
[Issue](https://github.com/waired-ai/waired-agent/issues) に添付できます。
