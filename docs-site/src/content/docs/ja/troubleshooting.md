---
title: うまくいかないとき
description: いま実際に起きている症状を自分の言葉で探して、直すための手順を 1 つだけ見つけます。
meta:
  audience: Waired の様子がおかしい人
  needs: 対象のパソコンのターミナル
  time: 症状を探す。各対処は 1〜2 分
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
- [サインインのリンクが途中で期限切れになった](#the-sign-in-link-expired)
- [常駐サービスが応答せずサインインが止まる](#sign-in-stops-because-the-background-service-is-not-responding)
- [セットアップが途中で止まった](#setup-stopped-partway)
- [セットアップで「推論エンジンが起動しなかった」と出た](#setup-says-the-inference-engine-failed-to-start)
- [セットアップで「選んだモデルをダウンロードできない」と出た](#setup-says-it-cannot-download-the-model-you-chose)
- [デバイス数の上限に達したと言われた](#it-says-i-have-reached-the-device-limit)
- [「enrolled system-wide」と表示される](#it-says-the-device-is-enrolled-system-wide)
- [非常に小さいモデルが選ばれた](#waired-chose-a-very-small-model-for-my-machine)
- [選んでいないのにローカル推論がオフで始まった](#local-inference-started-off-and-i-did-not-choose-that)
- [ローカル推論が「まだ設定されていない」と出る](#it-says-local-inference-is-not-set-up-yet)
- [セットアップで「テスト生成を完了できませんでした」と出た](#setup-said-it-could-not-complete-a-test-generation)

**応答がない**

- [サインインしたのに「サインインしていない」と出る](#i-signed-in-but-waired-says-i-am-signed-out)
- [応答が返ってこない / Engine が not ready のまま](#no-answer-comes-back)
- [Claude Code がクラウドを使い続ける](#claude-code-is-still-using-the-cloud)
- [Waired に「Claude Code は組織が管理している」と言われる](#waired-says-claude-code-is-managed-by-your-organisation)
- [Claude Code に「Waired cannot answer」と出る](#claude-code-says-waired-cannot-answer)
- [Waired のアイコンに「エージェントが起動していません」と出る](#the-waired-icon-says-the-agent-is-not-running)
- [「waired-agent is not running」と出る](#a-command-says-waired-agent-is-not-running)
- [macOS で常駐サービスが一度も起動しない](#macos-the-background-service-never-starts)
- [Windows で 502 エラーになる](#windows-i-get-a-502-error)

**遅い・おかしい**

- [応答がとても遅い](#answers-are-very-slow)
- [GPUが使われていない](#my-gpu-is-not-being-used)
- [ハードウェアより大きいモデルを選んでしまった](#i-chose-a-model-bigger-than-my-hardware)
- [長いプロンプトで GPU のメモリが足りないと言われる](#it-says-the-gpu-ran-out-of-memory-on-a-long-prompt)
- [Windows: グラフィックス側にメモリを多く割り当てたら悪化した](#windows-giving-the-graphics-chip-more-memory-made-things-worse)
- [モデルの行に `needs inference engine …` と出る](#a-model-says-it-needs-a-newer-inference-engine)
- [このパソコンに推論エンジンがない](#this-computer-has-no-inference-engine)
- [/model に Waired の項目が出ない](#the-waired-entries-are-missing-from-model)
- [長い Claude Code のセッションが要約される](#long-claude-code-sessions-get-summarized)

**ほかのパソコン**

- [ほかのパソコンから AI に届かない](#my-other-computer-cannot-reach-the-ai)
- [パソコンを固定したらリクエストが通らなくなった](#requests-stopped-working-after-i-pinned-a-computer)

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

<a id="the-sign-in-link-expired"></a>

## サインインのリンクが途中で期限切れになった

`waired init` が表示するリンクには有効期限があります。どれだけの長さかは
Waired のサーバー側が決めます。別の部屋にある携帯で二段階認証に応えたり、
タブを開いたまま別の作業をしたりすると、それだけで使い切ることがあります。
期限が過ぎると、ターミナルは次の行で止まります。

```
waired: login expired. Run `waired init` again
```

書いてあるとおりに実行してください。

```sh
sudo waired init
```

壊れたものはなく、片付けも要りません。新しいリンクが表示されるので、そちらで
サインインし直すだけです（Windows は管理者のプロンプトから `waired init`）。

ターミナルが止まったあとにブラウザでサインインを済ませても、もう「成功した」
とは言いません。リンクの期限が切れた旨を表示して、ターミナルに戻り
`waired init` を実行し直すよう案内します。すでに Web コンソールのデバイス一覧
まで進んでいた場合は、そこのバナーが「このパソコンの登録が終わる前にサインインが
期限切れになった」と伝え、そのパソコンで `waired init` を実行し直すよう案内
します。どの画面も言っていることは同じです。パソコンを登録するのはターミナルで
待っているコマンドで、そのコマンドはもう終わっているため、ブラウザだけでは先に
進めません。

以前のバージョンは、リンクとは別の自前の待ち時間で諦めていました。出るのは
要求の期限切れを告げるだけの、何をすればいいのか分からない 1 行で、その間も
ブラウザは「セットアップはこのまま続きます」と表示し続けていました。静かに
止まったパソコンが、動いているパソコンとまったく同じに見えていたわけです。
いまはターミナルがリンクの有効期限いっぱいまで待ち、何が起きたかを 1 文で伝え、
ブラウザも来ないパソコンを約束しなくなりました。

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

### サインインはできたのに、セットアップの手順が実行されない

常駐サービスへの読み取りと書き込みは別の経路を通ります。そのため、片方だけ届いて
もう片方が届かないことがあります。セットアップがまったく届かなかったときは、黙って
続行せずに `waired init` がその旨を表示します。

```text
warn: could not ask the background service about setup (…); its setup steps will be skipped. Run "waired doctor" to see why.
```

この実行では、常駐サービスを必要とする手順 — 推論エンジンのインストール、
コーディングツールの接続、ブラウザへの進捗報告 — が飛ばされます。サインイン自体には
影響しません。パソコンはサインインしたままです。

もう一方の、より軽い形は「問い合わせは届いたが、最初の更新だけが届かなかった」場合です。

```text
warn: could not tell the background service that setup is running (…); retrying in the background. If the browser shows no progress, run "waired doctor".
```

こちらは 10 秒ほどで自動的に復旧します。それでもブラウザに進捗が出ないときは
`waired doctor` を実行してください。

これらの行が出るようになる前は、どちらの失敗も「常駐サービスが古くてこの機能を
持っていないパソコン」とまったく同じに見えていました。その最後のケースだけは今も
意図的に何も表示しません — そのパソコンで直すべきものは更新以外に無く、セットアップは
自動的に以前の動作に切り替わるためです。

<a id="setup-stopped-partway"></a>

## セットアップが途中で止まった

セットアップ画面に、何が起きたかが表示されます。メッセージごとに意味が決まっています。

| 表示 | 意味 | 対処 |
|---|---|---|
| The setup command on … was closed before this finished. Your progress was saved. | セットアップを実行していたターミナルが閉じられた。管理者権限が必要な工程は、そのウィンドウだけが担当している。 | `sudo waired init`（Windows は管理者プロンプトで `waired init`）をもう一度実行。続きから再開し、進捗は失われません。 |
| Setup has not been run on … yet, so its coding tools are not connected. | そのパソコンでセットアップコマンドがまだ実行されていない。コーディングツールをつなげられるのはこのコマンドだけです（Web ページからホームフォルダに書き込んだり、マシン全体の設定を変えたりはできません）。 | そのパソコンで `sudo waired init`（Windows は管理者プロンプトで `waired init`）を実行します。ほかの工程はブラウザから設定できますが、ここだけはできません。 |
| Setup has not been run on … yet, so its inference engine is not installed. | 同じことが 1 つ手前の工程で起きている。セットアップコマンドがまだ実行されておらず、推論エンジンのインストールにはこのコマンドだけが持つ管理者権限が必要。 | そのパソコンで `sudo waired init`（Windows は管理者プロンプトで `waired init`）を実行します。中断されたわけではなく、初回の実行がまだ行われていない状態です。 |
| Setup on … needs administrator access to continue. | 管理者権限なしで開始された。 | 管理者のターミナルから開始し直してください（[サインインとセットアップ](/ja/getting-started/first-run/)）。 |
| … has run out of disk space. | モデルが入りきらなかった。 | 空き容量を作るか、[カタログ](/ja/reference/model-catalog/)から小さいモデルを選びます。 |
| … could not finish downloading. Check its internet connection. | ネットワーク起因でダウンロードが失敗した（名前解決できない、接続が切れた、証明書を検証できないなど）。 | 再試行してください。最初からではなく途中から再開します。 |
| The inference engine on … is an older version than this model needs. | そのモデルには、このパソコンに入っている推論エンジンより新しいバージョンが必要。 | そのパソコンで `waired update` を実行するか、[カタログ](/ja/reference/model-catalog/)から別のモデルを選びます。 |
| This took too long on … and was stopped. | ある工程が制限時間を超えた。 | 再試行してください。同じ工程で 2 回起きる場合、そのモデルにはこのマシンが遅すぎる可能性が高いです。 |
| Something went wrong on …. | 何が起きたのかを Waired が特定できなかった。ダウンロードが中断された、あるいはダウンロード先の推論エンジンを起動できなかった、といったケース。 | 再試行してください。繰り返す場合は、そのパソコンで `waired doctor` を実行するか、ログを見てください（[さらに詳しく](#going-deeper-logs)）。 |

コーディングツールの工程が失敗した場合は、そのパソコンで `waired link --force all`
を実行すれば修復と同時にこの行も解消されます。ページに追随させるためだけに
セットアップをやり直す必要はありません。

「インターネット接続を確認してください」と出るのは、**本当にネットワークらしい
失敗のときだけ**です。判別できなかったものは、当て推量をせずそのまま「特定できな
かった」と表示します。中断されたダウンロードと、つながらないレジストリは別の問題
であり、ルータを見て直るのは片方だけだからです。

なお**モデルのダウンロードだけは例外**で、ブラウザのタブを閉じても続きます。
[app.waired.ai](https://app.waired.ai) でそのデバイスを開けば途中経過を確認できます。

ターミナルで見ている場合は、`waired init` と `waired models pull` が失敗を報告する
行に理由を出すようになりました。

```text
qwen3-8b-instruct: failed — no space left on device
```

古いバージョンの常駐サービスでは、理由が付かず `failed` だけになることがあります。
その場合は `waired doctor` とログに残っています。

<a id="setup-says-the-ai-engine-failed-to-start"></a>

## セットアップで「推論エンジンが起動しなかった」と出た

ターミナル側で、モデルの待機を打ち切って「悪いのはエンジンだ」と表示されます。

```
The inference engine failed to start, so qwen3.5-4b can't download.
ollama: process exited during startup: signal: killed
Run `waired doctor` for details; `waired status` shows the current state.
```

2 行目はエンジン自身が記録した理由をそのまま出したものです。多くの場合、
そのうしろにエンジンのログの末尾が続きます。まず読むべきはこの部分です。

**サインインはこの時点で完了しています。** デバイスはネットワークに参加していて、
ローカル推論以外はすべて動きます。最後のまとめも成功ではなくその旨を表示します。
Waired は背後で再試行を続けるので、エンジンが動き出せばダウンロードも自然に始まります。

ワンライナーのインストーラから来た場合も、最後のメッセージの下に同じことが出ます。
インストール自体は成功しているので、その旨はそのまま表示されます。

```
🎉 Waired is installed.
✅ Enrolled — the agent service is running.

⚠️  Local inference is not running on this device.
    Sign-in is finished; only local inference is missing.
    Details:      waired doctor
```

`waired init` はこのとき **終了コード 3** で終わります。「サインイン自体が失敗した」
場合とスクリプトから区別できます。

| 終了コード | 意味 |
| --- | --- |
| `0` | サインイン済みで、ローカル推論も動いている(または最初から使わない設定)。 |
| `3` | サインイン済みだが、この端末でローカル推論が動いていない。 |
| `1` | セットアップが完了しなかった(サインイン自体が失敗)。 |
| `130` | Ctrl-C で中断した。 |

`3` を `1` にしていないのは意図的です。デバイスは実際にサインイン済みで使える状態であり、
サインインをやり直してもエンジンの状況は何も変わらないためです。

エンジンのインストールを自分で切った場合 — `--skip-ollama` / `-SkipOllama`、または
環境変数 `WAIRED_NO_OLLAMA` — は、この項目には当てはまりません。そのデバイスは
意図的にエンジンを持たず、`waired init` は `0` で終了します。

モデルがまだダウンロード中の場合も、この項目ではありません。セットアップは
**`Waired is signed in — local inference is still setting up here`** で終わり、終了コードは
`0` です。異常ではなく、転送がセットアップの待ち時間の上限を超えただけで、続きは
バックグラウンドのサービスが完了させます。`waired status` で進捗を確認できます。

Waired がモデルを一つも選ばなかったパソコンも、この項目ではありません。セットアップは
**`Waired is signed in — no model chosen for this computer`** で終わり、終了コードは
`0` です。エンジンは導入されて動いており、欠けているのはモデルです。カタログの
どれもこのハードウェアにうまく収まらなかったためです。ダウンロード中のものは無く、
`waired status` に表示する進捗もありません。自分で選ぶときは
`waired models pull <model>`、またはブラウザのダッシュボードから指定します。
`waired models ls --detail` が、どのモデルが収まるかとその理由を表示します。

よくある原因:

- **別の Ollama がポートを使っている。** `waired runtimes status` が見つけたバージョンを
  表示します。そのプロセスを終了するか、`agent.json` の `inference.ollama_port` を
  空いているポートに変更します。
- **Ollama 以外の何かがそのポートを使っている。** Waired はそれを引き継げないので、
  エンジンはそのまま起動に失敗します。`waired status` がそのアドレスを表示します:

  ```
  ⚠ ollama: another program is already listening on 127.0.0.1:9475, the port the
    inference engine was told to use — set inference.ollama_port in agent.json to
    a free port
  ```

  そのプロセスを終了するか、`inference.ollama_port` を空いているポートに変更して
  サービスを再起動します。
- **vLLM の推論エンジンのポートを別のプログラムが使っている。** ポートが塞がっていると
  エンジンはそもそも起動できません。`waired status` がそのアドレスを表示します:

  ```
  ⚠ vllm: another program is already listening on 127.0.0.1:9479, the port the
    inference engine was told to use — set inference.vllm_port in agent.json to
    a free port
  ```

  そのプロセスを終了するか、`agent.json` の `inference.vllm_port` を空いている
  ポートに変更してサービスを再起動します。
- **エンジンがクラッシュを繰り返している。** 数回続くと Waired は自動再起動をやめ、
  その旨を表示します。原因に対処してから `waired inference engine start` で再開できます。
  `waired status` と `waired runtimes ls` は、エンジンの状態の代わりに **gave up**
  と表示します。自分で停止したエンジンと見分けるための表示です:

  ```
  runtimes:       ollama 0.33.3 (gave up, ctx 32k q8_0)
  ⚠ ollama: engine repeatedly crashed; not retrying — …
  ```
- **エンジンがそもそも起動していない。** vLLM エンジンは、動かす前に自分の準備を
  終えている必要があります —— Python 環境が構築済みであること
  (`waired runtimes install vllm`)、そしてそのエンジンが配信できる版を持つモデルが
  選ばれていること。どちらかが欠けているとクラッシュするエンジン自体が存在しない
  ので、Waired は始まらないダウンロードを待つのではなく、エンジンの失敗として
  どの部分が欠けているかを名指しします。

`waired doctor` はこれらをまとめて確認します。また
`sudo waired doctor --fix` は常駐サービスにエンジンの起動を頼み、起動していない
理由を表示します。

<a id="setup-says-it-cannot-download-the-model-you-chose"></a>

## セットアップで「選んだモデルをダウンロードできない」と出た

モデルによっては動かせないパソコンがあります。常駐サービスがその選択を断った場合、
ダウンロードが始まるかどうかを待たずに、ターミナルがその場でその旨を表示します。

```
Waired can't download qwen3.6-35b-a3b on this computer.
the engine on this device is too old for this model
Update Waired here (`waired update`), or pick a different model in your browser.
```

2 行目は常駐サービスが記録した理由をそのまま出したものです。3 行目はその理由によって
変わり、2 通りあります。

- **推論エンジンがモデルの要求より古い。** そのパソコンで `waired update` を実行すると、
  ダウンロードはそのあと自動的に始まります。更新で直るのはこの理由だけです。
- **それ以外** — そのパソコンではそのモデルを実行できないか、ダウンロードが無効に
  なっています。ブラウザで別のモデルを選ぶか、`waired models ls --detail` で
  このマシンに合うモデルを確認してください。

いずれの場合もサインインは完了しています。セットアップ画面のモデルの行にも同じ理由が
出るので、ターミナルに戻らずそちらで選び直せます。

よく似た行に、意味の違うものがあります。

```
Waired hasn't started downloading qwen3.6-35b-a3b yet; it keeps trying in the background.
```

こちらは拒否ではありません。Waired が把握している異常は何もなく、ターミナルが監視を
やめた時点でダウンロードがまだ始まっていなかっただけです。処理は背後で続きます。
`waired status` で現在地を確認できます。

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

`waired doctor` も同じマシンでは **state directory** の行で同じことを伝え、失敗では
なく「実行できなかった検査」として扱います
（→ [診断自体が全体を見られない場合](/ja/getting-started/doctor/#when-the-check-itself-cannot-see-everything)）。

代わりに `Not enrolled. Run 'waired init' to connect this device.` と出た場合は、
本当にまだセットアップされていません
（→ [サインインとセットアップ](/ja/getting-started/first-run/)）。

<a id="waired-chose-a-very-small-model-for-my-machine"></a>

## 非常に小さいモデルが選ばれた

それがこのパソコンで、コーディング 1 セッション分をメモリに保持したまま
動かせる最大のモデルです。Waired はそれを実行します。収まりはするが長い
会話の大半を捨てることになるモデルは、より良い選択ではありません。

以前は拒否していました。載せられる最良のモデルがコーディングに使える品質に
届かないと判断した場合、ローカル推論を**オフ**の状態から始め、何も動きません
でした。現在はそうしません。ローカル推論がオフで始まる理由として残っているのは
**速度**だけです。
→ [選んでいないのにローカル推論がオフで始まった](#local-inference-started-off-and-i-did-not-choose-that)

何が選ばれ、なぜそうなったかは次で確認できます。

```sh
waired models ls --detail
```

**SIZE** 列はそのモデルがどのクラスのGPU向けかを、**FIT** 列は
このパソコンに収まるかどうかを示します。一覧にあるモデルはどれも選択できます。

```sh
waired models use <model>
```

より大きいモデルも動きます。ただし会話の間じゅうシステムメモリから自分自身を
読み直すことになり、長いコーディングセッションほどその遅さを感じます。
`waired inference off` はこのパソコンでモデルを動かすこと自体をやめる設定です。
その場合もネットワークには残り、ほかのパソコンの AI を使えます。

<a id="local-ai-started-off-and-i-did-not-choose-that"></a>

## 選んでいないのにローカル推論がオフで始まった

理由はパソコン自身に聞けます。

```sh
waired inference status
```

Waired が判断した場合は、その旨が表示されます。

```
Local inference: off
  This computer is below the recommended spec for local inference.
  per request           210.4 s or more
  target                45 s or less
  It can still use the models running on your other computers.
  Turn it on with `waired inference on`.
```

**この数値の出どころ。** 推論エンジンを導入した直後——数十 GB のフルサイズの
モデルを何かがダウンロードするより前——に、Waired は 1 GB 程度の小さいモデルを
取得し、現実的なリクエスト——長い質問と、それに対する通常の長さの
回答——を計測します。計測は 3 回行い、中央の結果を採用するため、たまたま 1 回
だけ混雑していたことで結論が決まることはありません。速いパソコンなら数秒、
遅いパソコンでも数分で終わります。ターミナルからセットアップした場合も、
ブラウザからセットアップした場合も同じです。

基準を大きく下回るパソコンでは、Waired はそこまで測りません。最初の計測の
冒頭部分だけで「答えが出るまで長くかかりすぎる」と分かるため、正確な数値では
なく **210.4 s or more**（210.4 秒以上）と表示し、完全な計測にかかる数分を
使いません。その数分は、モデルのダウンロードが待たされる時間でもあります。

計測はインストールごとに 1 回です。サービスを再起動しただけなら前回の結果を
再利用し、Waired または推論エンジンを更新した場合は計測し直します。新しい
ビルドでの速度は、そのパソコンについての新しい事実だからです。

**小さいモデルにしても解決しない理由。** グラフィックスカードの無いパソコンでは、
カタログ内で最小のコーディングモデルも、載る範囲で最大のモデルも、速さは大きく
変わりません——どちらも 1 往復に数分かかります。これはモデルではなくパソコン側の
話であり、Waired がより小さいモデルを選び直すのではなく止める理由です。

**これは出発点であって、判定ではありません。** そのパソコンはネットワークには
参加し、他のパソコンで動いている AI を使えます。ローカル推論はいつでも
オンにできます。

```sh
waired inference on
```

Waired アプリでは **Run models on this computer** が同じ操作です。
一度選んだあとは、Waired はその選択を保持します。計測は出発点を決めるために
走るものであり、あとから選択を覆すことはありません。

`waired inference status` が理由なしで**オフ**と表示する場合、判断したのは
このパソコンではありません。セットアップでの選択、インストーラの
`--inference-enabled false`、`waired inference off` のいずれかです。

<a id="it-says-local-ai-is-not-set-up-yet"></a>

## ローカル推論が「まだ設定されていない」と出る

```sh
waired inference status
```

```
Local inference: not set up yet — this device is not signed in. Run `waired init`.
```

Waired をインストールしてからサインインするまでの状態です。異常ではなく、
変更すべき設定もありません。このパソコンにはまだモデルを動かす対象のアカウントが
ない、というだけです。[サインイン](/ja/getting-started/first-run/)すれば、
**オン**か**オフ**が表示されるようになります。

以前のバージョンはこの状態を *「unknown (this daemon does not report it —
`waired update`)」* と表示していました。案内された `waired update` を実行しても
「すでに最新です」と返るだけです。この表示が出る場合、そのパソコンは古い
ビルドで動いています。`waired update` を実行しても害はありませんが、
状況を進めるのはサインインのほうです。

<a id="setup-said-it-could-not-complete-a-test-generation"></a>

## セットアップで「テスト生成を完了できませんでした」と出た

セットアップの最後に、Waired はこのパソコンの速さを測るため AI に短い質問を
します。このメッセージは、質問はしたものの答えが返ってこなかった、という意味
です。測定できなかったので、Waired は「動いています」とは表示しません。

ほとんどの場合、推論エンジン自体が停止しています。確認してください。

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

<a id="i-signed-in-but-waired-says-i-am-signed-out"></a>

## サインインしたのに「サインインしていない」と出る

Waired のアイコンが「Not signed in」になる、あるいはアカウント上からこの
パソコンが消える。サインアウトした覚えがないのに、再起動直後に起きがちです。

見た目は同じでも原因は 2 通りあります。`waired doctor` で切り分けられます。

```sh
waired doctor
```

**`network connection` が ⚠ の場合** — サインインは有効で、このパソコンがまだ
接続できていないだけです。いつも使うネットワークポートが再起動時に別のものに
使われていた場合も含め、Waired が自動で再試行し続けるので、1 分ほど待って
もう一度確認してください。いつまでも解消しない場合は常駐サービスを再起動します。

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows（管理者）
```

**`device sign-in` が ✗ の場合** — このパソコンのサインインが実際に無効に
なっており、サインインし直す以外に戻す方法はありません。

```sh
sudo waired init      # Linux / macOS
waired init           # Windows（管理者）
```

モデル・設定・コーディングツールの構成はそのまま残ります。このパソコンの
アカウント上の位置づけを復旧するだけです。ローカルの AI はこの間も応答し
続けますが、アカウントを必要とする機能は止まります。サインインし直すまで、
Web コンソールにこのパソコンは表示されず、他の端末からも届きません。

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
  → [使うモデルを選ぶ](/ja/guides/choose-a-model/)。
- **`engine failed`** — 推論エンジンが自分で停止しました。Waired が自動で再起動する
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

- AI は答える前に**メモリに載せる**必要があり、エンジンを起動してから最初の
  リクエストがその待ち時間を負担します。所要時間はモデルとパソコン次第なので、
  Waired は推測しません — 今どうなっているかをそのまま示します（下記）。
- **503** が返る場合は、ルーティングが一時停止中（`waired resume`）か、共有が
  オフ（`waired share on`）です。

### 動いているのか、止まっているのか

`waired status` が両方に答えます。

```
  model loaded:   ollama: no (the next request reloads it)
  serving now:    0 requests
```

- **`model loaded:`** — AI がメモリに載っているかどうか。`no` なら次の
  リクエストがまず載せることになり、そのリクエストが遅いものです。モデル名の
  あとに `not the model this computer serves` と付く場合は、別のものがメモリを
  取っており、あなたのモデルは載せ直しになります。
- **`serving now:`** — このパソコンが今処理しているリクエストの数。混同されがちな
  2 つの状況を分ける行です。コーディングツールが応答を返さない状態で
  **`0 requests`** なら、待ちの原因はこのパソコンではありません — AI ではなく
  ルーティングを見てください。
- **`last turn:`** — 直近の回答が話し始めるまでにかかった時間。このパソコンが
  一度でも回答した後に表示されます。

Claude Code もフッターに同じことを表示します:
`⚡ waired: on Waired (qwen3-8b-instruct) · model not loaded`

読み込み中の Waired は黙っていません。このパソコンが答えるときは、読み込みに
どれだけかかっても回答が始まるまで接続を保ち、その間も接続を維持し続けます。
一定時間でターンが別の場所へ回るタイムアウトはありません。上の行の代わりに
フッターが `⚠ waired: Waired cannot answer (…)` と出ているなら、自分のどのパソコンも
そのターンを受けられない状態です →
[Claude Code に「Waired cannot answer」と出る](#claude-code-says-waired-cannot-answer)

それでも解決しない場合、`waired runtimes status` がエンジン自体の状態を、
[ログを見る](#going-deeper-logs)がより詳しい情報を提供します。

<a id="claude-code-is-still-using-the-cloud"></a>

## Claude Code がクラウドを使い続ける

```sh
waired doctor          # f キーで見つかった問題を修復
waired claude status
```

まずフッターを見ます。`→ waired: Anthropic` は**このセッション**が Anthropic の
モデルにあるという意味です。何も触っていないセッションはそうなります — Claude Code
自身の既定が Anthropic のモデルで、Waired はそれを変えないからです。これはセットアップ
直後の通常の状態で、不具合ではありません。`/model` で **Waired** の項目を選ぶと、
次のターンから自分のパソコンで動き、フッターは `⚡ waired: on Waired` に変わります。

`/model` に Waired の項目が無い場合は
[/model に Waired の項目が出ない](#the-waired-entries-are-missing-from-model)へ。
`waired claude status` が「連携が無効」と表示する場合は有効化し、Claude Code の
セッションを再起動してください。

```sh
sudo waired claude enable     # Windows は管理者プロンプトから
```

代わりに「Claude Code on this computer is managed by your organisation」と表示されて
終わる場合は、[次の項](#waired-says-claude-code-is-managed-by-your-organisation)へ。

`waired doctor` は、Claude Code と Waired の接続が壊れている場合に再構築します。
`waired claude status` は、新しいセッションがどのモデルで始まるか（`default model:`）と、
直近のターンが何をしたかを表示します。

```
last request:       claude-opus-5 → the real Anthropic API   (2 minutes ago)
```

ターンがクラウドへ行くのは、そのモデルが Anthropic のモデルのときだけです。Waired が
勝手に送ることはないので、`last request:` が本来の Anthropic API を指していれば、
それはそのセッションのモデルがそう指定したということです。

<a id="waired-says-claude-code-is-managed-by-your-organisation"></a>

## Waired に「Claude Code は組織が管理している」と言われる

`sudo waired claude enable` がルーティングを有効にせず、次の表示で止まります。

```
Claude Code on this computer is managed by your organisation, so Waired did not
change its settings. Found in /etc/claude-code/managed-settings.json:
  availableModels
  forceLoginMethod = console

Pointing ANTHROPIC_BASE_URL at Waired would also switch off the settings your
organisation delivers to every session on this computer, which is not Waired's
call to make. Ask whoever manages this computer, or use Waired from another
coding tool — `waired link` sets those up per user and touches nothing
machine-wide.
```

たいていは職場のパソコンです。Claude Code はマシン全体の設定ファイル（表示にパスが
出ます）を読みます。そのパソコンを管理している人が Claude Code の組織向けの設定を
そこに置いていると、Waired はそれを読んで止まります。`Found in` の下の行が
見つかったもので、どれか 1 つあれば十分です。ログインの強制
（`forceLoginOrgUUID`・`forceLoginMethod`・`forceLoginGatewayUrl`）、使えるモデルの
一覧（`availableModels`）、`/model` の一覧（`modelPicker`）、または Waired 自身の
ループバックアドレス以外をすでに指している `ANTHROPIC_BASE_URL` です。

止まる理由は、Claude Code を Waired 経由にするとは同じファイルに
`ANTHROPIC_BASE_URL` を書くことで、それは組織がそのパソコンのすべてのセッションに
配っている設定を — 自分のアカウントだけでなく — 切ってしまうからです。それを受け入れる
かどうかはパソコンを管理している人の判断なので、Waired は判断せず、構わず書き込む
オプションもありません。

できること:

- **パソコンを管理している人に相談する。** 表示にファイルと関係する設定が出ています。
- **同じパソコンで、別のコーディングツールから Waired を使う。** Claude Code の
  マシン全体の向き先の変更以外はすべて使えます —
  [OpenCode から使う](/ja/guides/opencode/)と
  [OpenClaw から使う](/ja/guides/openclaw/)を参照してください。

`waired init` のルーティングの手順も同じ所で止まり、同じ表示が出ます。セット
アップの残りはそのまま進み、Claude Code は Anthropic API に直接つながったままです。

<a id="claude-code-says-waired-cannot-answer"></a>

## Claude Code に「Waired cannot answer」と出る

Waired の項目にあるターンを自分のどのパソコンも処理できないとき、そのターンは
Claude Code の中で `API Error: 400` と、何が答えられなかったかを名指しする
メッセージを出してすぐに失敗します。Anthropic API には送られません。メッセージの
末尾はどれも同じ — ``Pick an Anthropic model in /model to send this turn to the cloud, or run `waired doctor` to see what is missing.`` — で、これが 2 つの出口です。
そのセッションをクラウドに切り替えるか、足りないものを直して送り直すか。どちらの
対処かはメッセージの先頭で分かります。

| メッセージの先頭 | 意味 | すること |
|---|---|---|
| `Waired is not set up to answer on this computer, so this turn has nowhere to run.` | このパソコンにエンジンが無く、自分のほかのパソコンにも届かない。 | このパソコンで `waired doctor`。ここでエンジンを動かすか、エンジンのあるパソコンの電源を入れる。 |
| `The computer this turn is pinned to, <名前>, is not answering.` | `waired worker` で固定したそのパソコンが、電源オフ・スリープ・共有オフのいずれか。 | → [パソコンを固定したらリクエストが通らなくなった](#requests-stopped-working-after-i-pinned-a-computer) |
| `The peer <名前> stopped answering after <時間>.` / `The peer <名前> stopped working on this request after <時間>.` | 前者は、答えていたパソコンが応答しなくなった。後者は、止まったと報告したか、エンジンは動いているのに答えなくなった — 固まっているだけで、パソコンが消えたわけではない。 | `waired peers list` と、そのパソコンでの `waired doctor` で確認する。 |
| ``No computer on Waired runs a medium model or larger. Change the floor with `waired worker set --min-model-size`.`` | 自分で設定したルーティングの下限が、このパソコンを含む全パソコンを除外した。 | 下限を下げるか外す → [`--min-model-size`](/ja/reference/cli/#setting-a-minimum-model-size) |

たいていはフッターが先に伝えます。赤い `⚠ waired: Waired cannot answer (local
disabled, no peer)` は、自分のどのパソコンも次のターンを受けられないと Waired が
すでに分かっている状態です。括弧内はこのパソコンのエンジンの状態（`local disabled`、
`local no_engine` など）と、ほかのパソコンに届かないときの `no peer` です。

<a id="the-waired-icon-says-the-agent-is-not-running"></a>

## Waired のアイコンに「エージェントが起動していません」と出る

Waired のメニューを開いて **Start the Waired agent…** を選んでください。パソコンが
管理者の確認を求めます。これは OS 自身の確認画面で、常駐サービスが特定のユーザーでは
なくパソコン全体のものであるために必要です。自分でコマンドを打ちたい場合は
**Copy start command** を選ぶと、このパソコン向けのコマンドがクリップボードに入ります。

メニューは次の 2 つを区別して表示します。

- **「Waired agent is starting…」** — 正常です。Windows では常駐サービスがサインインの
  数分後に起動する設定になっているため、Waired のアイコンのほうが先に出ます。異常では
  ないので、待っても、その場で起動しても構いません。
- **「Waired agent is not running」** — 起動しているはずなのに起動していない状態です。
  メニューから起動し、それでも戻らない場合は `waired doctor` を実行してください。

手動でサービスを停止しても、その状態は残りません（パソコンの起動時にまた立ち上がります）。

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

### Windows: 起動時に立ち上がらなくなった

Windows が起動時に Waired の常駐サービスをブロックすることがあります。Waired の
プログラムには、まだ Windows が認識する証明書による署名が付いていないためです。
スマート アプリ コントロールが有効だと、Windows は起動時に（ネットワーク確認に頼れない
状態で）これを判断し、サービスが起動しません。毎回ではなく、同じパソコンでも次の起動では
正常に立ち上がることがあります。

署名付きの配布を始めるまでは、この状態になったら Waired のメニューから起動してください。
壊れているわけではなく、同じサービスを手動で起動すれば正常に動作します。

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

このパソコンに推論エンジンが入っていません（多くは `-SkipOllama` または
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

- **GPUが使われているか**
  → [GPUが使われていない](#my-gpu-is-not-being-used)
- **モデルがメモリに対して大きすぎないか** — はみ出した分は CPU で処理されるため
  劇的に遅くなります。`waired models ls --detail` で収まり具合を確認できます。
- **AMD Ryzen AI Max のマシンなら、グラフィックスにどれだけ予約しているか**
  — 多く予約するとよくなるどころか悪化します。
  → [Windows: グラフィックス側にメモリを多く割り当てたら悪化した](#windows-giving-the-graphics-chip-more-memory-made-things-worse)
- **ほかのパソコンが答えていないか** — `waired infer --explain "hi"` が、
  応答したマシンと推定遅延を表示します。
- **Claude Code セッションの最初のターンではないか** — そこがいちばん高くつきます。
  会話全体・指示・ファイルの中身を読み終えるまで 1 文字も返らないので、ノート PC や
  古いグラフィックスカードでは数分かかることがあります。Waired は作業中のパソコンを
  見捨てずに待ちます。同じセッションの 2 ターン目以降はずっと速くなります
  （→ [どの AI が答えたか](/ja/guides/claude-code/#どの-ai-が答えた)）。

<a id="my-graphics-card-is-not-being-used"></a>

## GPUが使われていない

まず、Waired が何を見つけているかを確認します。

```sh
waired models ls --detail
```

1 行目にカード名と搭載メモリが出ます。カードがあるのに `no GPU` と表示される場合、
カードは検出できていません。渡されたモデルを含め、その後の判断はすべて CPU 前提で
見積もられています。

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

モデルが本当に収まっているかも確認してください
（要件は[モデルカタログ](/ja/reference/model-catalog/)）。

<a id="i-chose-a-model-bigger-than-my-hardware"></a>

## ハードウェアより大きいモデルを選んでしまった

Waired は警告しますが、禁止はしません。超過分（`needs 32 GB RAM (have 31 GB)` など）を
表示して確認を求めます。

- **少し超えている程度** — たいてい動きます。単に遅くなります。
- **本当に大きすぎる** — エンジンが読み込みに失敗し、明確なエラーを返します。
  小さいモデルに戻してください → [使うモデルを選ぶ](/ja/guides/choose-a-model/)。

推奨値には安全マージンが含まれています。Apple Silicon と AMD Strix Halo では、
GPU 側が実際に扱えるメモリ量で判定します。単体のGPUを搭載した
パソコンでは、Waired が**自動で選ぶ**対象はカード自身のメモリを基準に判定されます
— システム RAM にはみ出して初めて収まるモデルは、自分で意識して選ぶものです。
`waired models ls --detail` で、このマシンにおける全モデルの判定を確認できます。

<a id="it-says-the-gpu-ran-out-of-memory-on-a-long-prompt"></a>

## 長いプロンプトで GPU のメモリが足りないと言われる

これに気づくのはセットアップ中ではなく、パソコンを使っている最中です。長い会話の
途中でターンが失敗し、その後 `waired models ls --detail` のそのモデルの行が
`! running here with a warning` になり、表の下にエンジン自身の一文が出ます。
その一文は
`this computer's GPU ran out of memory serving a request at this model and window`
で始まり、丸括弧の中にエンジンの言葉が続きます。

これは「遅い」とは別の状態です。短いプロンプトなら動きますが、会話が長くなると
VRAM が足りなくなります。コーディングのセッションはすぐ長くなります。

Waired が勝手に何かを変えることはありません。エンジンは配信を続け（次の短い
リクエストは通ります）、Waired は警告を目に付く場所に置いておきます。

- `waired models ls --detail` がそのモデルに `! running here with a warning` を付け、
  エンジン自身の一文をその下に表示します。
- `waired status` が同じ内容を繰り返します。
- `waired doctor` が、このパソコンの他の状態と一緒に同じ内容を繰り返します。

必要な長さに対して、モデルがこのパソコンには大きすぎます。軽いモデルに
切り替えてください → [使うモデルを選ぶ](/ja/guides/choose-a-model/)。

この場合、Waired は**軽いモデルへの切り替えを自動では提案しません**。これは意図的
です。その提案は、計測で遅かったパソコンのためのものです。メモリが足りないのは
別の問題で、同じ会話の長さのまま小さいモデルにしても直るとは限らず、動くモデルを
「手放せ」と案内するのは誤った助言になるからです。

<a id="windows-giving-the-graphics-chip-more-memory-made-things-worse"></a>

## Windows: グラフィックス側にメモリを多く割り当てたら悪化した

AMD Ryzen AI Max（「Strix Halo」）のマシンでは、グラフィックス側と CPU が 1 つの
メモリを共有していて、そのうちどれだけをあらかじめグラフィックス側に渡すかを設定で
決めます。大きなモデルを動かすには増やせばよさそうに見えますが、逆です。

Windows は、VRAMの確保ごとに同じ量をシステム RAM側にも予約します。
必要になったときに退避できるようにするためです。つまりモデルは、グラフィックス側の
空きに加えて、**Windows から見えているメモリにも同じ量**が要ります。128 GB のマシン
から 96 GB をグラフィックス側に渡すと Windows に残るのは約 31 GB で、モデルの大きさの
実際の上限はその 31 GB になります。それを超えるモデルはロードを始めて足りなくなり、
何十分もディスクにページングしたまま、いつまでも答えません。

128 GB の Ryzen AI Max+ 395 で 76 GB のモデルを使い、この設定だけを変えた実測:

| グラフィックスへの予約 | 結果 |
| --- | --- |
| 96 GB | ロードが終わらない — 28 分待って応答なし |
| 512 MB | 15 秒でロードし、そのまま全速で動作 |

予約を減らして失うものはありません。グラフィックス側は残りのメモリにも届きますし、
この種のマシンではどちらも同じ物理メモリを同じ速度で読むだけだからです。

**なので小さくします。** BIOS ではVRAMのサイズを `Auto` のままに
します（*UMA Frame Buffer Size* という名前のことが多いです）。そのうえで
AMD Software: Adrenalin Edition の **Performance → Tuning → System → Variable
Graphics Memory** を開き、いちばん小さい選択肢を選びます。再起動したら、Waired から
どう見えているかを確認します。

```sh
waired models ls --detail
```

1 行目の数字が以前よりずっと大きくなっているはずです。まだ小さい残りのままなら、
ドライバに任せず BIOS 側で分割を固定しています。BIOS を `Auto` に戻してください。

<a id="a-model-says-it-needs-a-newer-ai-engine"></a>

## モデルの行に `needs inference engine …` と出る

新しい推論エンジンでしか動かないモデルがあり、その場合は行がそう告げます。

```
qwen3.8-27b   27B   medium   ✗ needs Ollama 0.32.13 (this computer has 0.31.1)
```

これは**メモリの話ではありません** — そのモデルはこのパソコンに収まるかもしれません。
このパソコンの推論エンジンがモデルの要求より古いだけで、更新されるまでは取得も
読み込みもできません。

推論エンジンは Waired が管理しているので、対処は通常の更新です。

```sh
waired update
```

更新すると、この Waired のビルドが同梱するバージョンまで推論エンジンが上がり、
行はひとりでに消えます。それまでの間、Waired は現在のエンジンで動かせるモデルを
選び続けるので、ローカル推論は止まりません。

行の末尾は他に 2 通りあり、それぞれが自分の状況を名乗ります。

- **`(this computer's version could not be read)`** — 推論エンジンは入っているが
  一度も起動していないので、聞く相手がいなかった状態です。起動してから、
  もう一度見てください。

  ```sh
  waired inference engine start
  ```

- **`(no inference engine on this computer)`** — このパソコンには推論エンジンがそもそも
  ありません。下の
  [このパソコンに推論エンジンがない](#this-computer-has-no-inference-engine) を見てください。

<a id="this-computer-has-no-ai-engine"></a>

## このパソコンに推論エンジンがない

推論エンジンが入らないパソコンもあります。Waired は「このパソコンでモデルを動かす」と
答えたときにだけ推論エンジンを入れるからです。`waired models ls --detail` は表の上で
そう告げます。

```
Host: Intel Arc 8 GB VRAM / 63 GB RAM · no inference engine installed

! No inference engine is installed on this computer, so it cannot run a model itself.
  Requests go to your other computers instead.
  Install one with `sudo waired runtimes install ollama`.
  The verdicts below are what this computer would run once an engine is installed.
```

**これは異常ではなく、正常な状態です。** このパソコンはサインインしたまま動き続け、
リクエストは Waired ネットワーク内の他のパソコンが答えます。その下のモデル行にも
意味があります — ここで**動くとしたら**何が動くかを示しています。

知っておくとよいことが 2 つあります。

- **トレイでモデルを選ぶと、推論エンジンの導入を尋ねます。** 推論エンジンのない
  パソコンでモデルを選んでも単独では何も起きないので、Waired は先に尋ね、
  推論エンジンを入れてから選択を記録します。
- **いつでも入れられます。** 推論エンジンは一般ユーザーが書き込めない場所に入るため、
  管理者権限が要ります。

  ```sh
  sudo waired runtimes install ollama
  ```

  Windows では同じ行が ``Install one with `waired runtimes install ollama`, from
  an elevated prompt.`` となります。コマンドに入れられる `sudo` が無いので、
  昇格の指示はコマンドの外に置かれます。

ここに推論エンジンが**あるはず**だった場合、最も可能性が高いのは、サインイン時に
「このパソコンではモデルを動かさない」と答えていたことです。`sudo waired init` を
実行し直すと、もう一度答えられます。

<a id="the-waired-entries-are-missing-from-model"></a>

## /model に Waired の項目が出ない

`/model` には Anthropic のモデル名の下に **Waired** / **Waired local** /
**Waired peer** が出るはずです（Public Share を有効にしていれば
**Waired public share** も）。出ない原因は 4 つで、確認する価値のある順に:

1. **Claude Code を再起動していない。** この行は Claude Code の起動時に読まれます。
   動いているセッションで `/model` を開き直しても読み直されず、Waired が行を
   変えても、次に起動した `claude` から出てきます。Claude Code を終了して起動し直して
   ください。
2. **このパソコンでルーティングが有効になっていない。** `waired claude status`
   で確認できます。Claude Code が Waired に向いて初めてこの項目が出ます。

   ```sh
   sudo waired claude enable    # Windows は管理者プロンプトから
   ```

3. **別のユーザー向けに書かれている。** この行は**自分の** `~/.claude/settings.json`
   （Windows は `%USERPROFILE%\.claude\settings.json`）の `modelPicker` に置かれる
   ので、`root` として Waired をセットアップしたインストールでは、自分の Claude Code
   が決して見ない場所に書かれます。どのファイルを見て何が分かったかは
   `waired claude status` が出します:

   ```
   /model rows:        not written — /home/you/.claude/settings.json
                       run `waired claude enable` as the user who runs `claude`
   ```

   行がある場合は、同じ行に件数とファイルが出ます:

   ```
   /model rows:        6 rows
                       /home/you/.claude/settings.json
   ```

4. **そのファイルに `/model` の行がすでにある。** Claude Code は `modelPicker` の
   一覧を 1 か所からまとめて読み、2 か所を合成しません。そのため自分の
   `~/.claude/settings.json` に自分や別のツールが置いた行があると、Waired はそれに
   触らず何も書きません。ステータス行がその旨を出します:

   ```
   /model rows:        LEFT ALONE — /home/you/.claude/settings.json already lists its own rows
   ```

   組織が管理する設定ファイルに `modelPicker` の一覧があるのは、そこの Claude Code が
   組織の管理下にある目印の 1 つで、その場合 `waired claude enable` は行も含めて
   何も書かず、その旨を表示します。
   [Waired に「Claude Code は組織が管理している」と言われる](#waired-says-claude-code-is-managed-by-your-organisation)を
   参照してください。同じ行の `UNREADABLE` は、ファイルが Waired の
   読める JSON ではないという意味です。直してから `waired claude enable` を
   実行し直してください。

WSL2 の中で Claude Code を動かし、Waired は Windows 側に入れている場合は別の話です。
別々のシステムなので、Windows 側の Claude Code を使ってください。

項目が戻るまでは選ぶものが無いので、Claude Code 自身の既定のままのセッションは本来の
Anthropic API に留まります。ステータス行が `→ waired: Anthropic` と出るので、それと
分かります。

<a id="long-claude-code-sessions-get-summarized"></a>

## 長い Claude Code のセッションが要約される

正常な動作です。ローカルのモデルはクラウドのモデルより一度に保持できる会話量が
少ないため、Waired が実際の上限を Claude Code に伝え、Claude Code が古いターンを
要約して収めます。冒頭を黙って失うのではなく、セッションが途切れずに継続しているということです。

一瞬「Prompt is too long」と表示されても、Claude Code が自動で復帰します。

**思ったより早く（または遅く）要約される場合。** 上限は Claude Code を接続した
時点で伝えられるため、モデルを切り替えたあとは古いままになることがあります。

```sh
waired claude status
```

**local window** の行に、今のモデルが扱える上限と、Claude Code が起動時に渡された
上限が並びます。食い違っていると表示されたら `sudo waired claude enable` を実行し
直し（Windows は管理者プロンプトから）、Claude Code を再起動してください。

しばらく大きなウィンドウを使いたい場合は、`/model` で使いたいモデルを選んで
ください。Anthropic のモデルを選べばセッションは本来の Anthropic API に送られ、
次のメッセージからそのモデル本来のウィンドウが適用されます。

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
3. **共有はオンですか。** ほかの端末に応答するには、パソコン自身の共有
   スイッチがオン（`waired share status` で確認、`waired share on` でオン）で、
   *かつ*ウェブコンソールの **「Sharing」** カードで **Your other computers**
   に提供している必要があります。

届いてはいるが `ready` にならない場合は、そのマシンにモデルが読み込まれていません。
そちらで[応答が返ってこない](#no-answer-comes-back)を順に確認してください。

**すべて届いているように見えるのにリクエストが通らない場合**は、`waired doctor` を
実行してください。**mesh peers** の行はネットワークの申告を鵜呑みにせず、各パソコンへ
実際にリクエストを送って、返ってきたかどうかを報告します。

```
⚠ mesh peers — 2/3 reported reachable, but only 0 answered an overlay ping —
  no reply from mac-mini, work-laptop. Inference cannot route to a peer that
  does not answer; check NAT traversal and relay connectivity
```

この行は「2 台は接続済みと申告されているが、実際には何も届いていない」という意味です。
名前の挙がったマシンについて、上の 3 点を確認してください。`(measured)` が付いた
件数は、同じ方法で実際に確認できたものです。

ポート開放や VPN の設定は不要です。ネットワークが許せば直接つながり、ファイアウォールが
邪魔する場合は暗号化された中継に自動で切り替わります。

<a id="requests-stopped-working-after-i-pinned-a-computer"></a>

## パソコンを固定したらリクエストが通らなくなった

```sh
waired worker get
```

固定は「このパソコンを使い、ほかは使わない」という強い指示です。固定した
パソコンがスリープ中・オフライン・共有オフのとき、Waired はこっそり別の場所で
処理したりせず、エラーを返します。これは意図した動作です。黙って別のマシンが
答えてしまうと、大きな GPU マシンに送ったつもりのリクエストが、実は目の前の
ノート PC で処理されていた、ということが起きてしまうからです。

Waired のアイコンにも同じことが出ます。**Worker: `<名前>` (pinned) —
unavailable, requests are not served here**。

Claude Code でも同じです。ターンはすぐに失敗してそのパソコンの名前を挙げます。
Anthropic API には送られません。

```
API Error: 400 The computer this turn is pinned to, sv-mag, is not answering. Pick an Anthropic model in /model to send this turn to the cloud, or run `waired doctor` to see what is missing.
```

それでもそのターンをクラウドに送りたければ、`/model` で Anthropic のモデルを
選んでください（→ [Claude Code](/ja/guides/claude-code/)）。

直すには、固定したパソコンを起こす（`waired peers list` と、そのマシンでの
`waired doctor` で確認 →
[ほかのパソコンから AI に届かない](#my-other-computer-cannot-reach-the-ai)）か、
固定をやめます。

```sh
waired worker set --mode=auto
```

固定したパソコンが戻ったのに、ターンがまだ同じメッセージで失敗するなら、1 分ほど
待ちます。そのパソコンの Waired の常駐サービスが再起動した直後は、自分のアカウントに
改めて自分を知らせるまで、ほかのパソコンはそこへ仕事を送りません。それまでは、
固定したターンは同じように失敗します。故障ではなく、そのパソコンで直すものも
ありません。待つか、上の手順で固定をやめます。

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
| macOS | `/Library/Logs/waired-agent.err.log`、または `sudo log show --predicate 'process == "waired-agent"' --last 10m`。このファイルは Waired が 32 MB で上限を掛け、直前の 10 世代を `waired-agent.err.log.0.gz`、`.1.gz` … として隣に残します。それより古いものはそちらを（`gzcat` で）確認してください。`debug` の間は上限が 128 MB に上がるので、詳細を上げてもさかのぼれる範囲は短くなりません。 |
| Windows | Waired の状態ディレクトリ配下の `logs\waired-agent.log`。通常のサービス導入では `C:\ProgramData\waired\logs\…` で、読むには管理者権限の PowerShell が要ります。上限の扱いは macOS と同じで、32 MB・`.0.gz`、`.1.gz` … 10 世代、`debug` の間は 128 MB です。`Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50` は要約版で、警告とエラーだけが載り詳細は載りません。 |
| 推論エンジン | Waired の状態ディレクトリ配下の `…/runtimes/ollama/logs/engine.log`（Linux は `/var/lib/waired/…`、macOS は `/Library/Application Support/waired/…`、Windows は `C:\ProgramData\waired\…`）。 |

## 不具合を報告する

[不具合を報告する](/ja/getting-started/report-a-problem/)の手順に従ってください。
再現する**前に**詳細ログを有効化し、1 つのファイルにまとめて添付します。この順番が
重要です — 原因を説明するだけの詳細は、先に頼まないと記録されません。

`waired init --mask-pii`（ほかのコマンドでは環境変数 `WAIRED_PII_MASK=1`）を使うと、
ホームディレクトリ・ユーザー名・ホスト名・アカウントのメールアドレスが伏せられるので、
出力やスクリーンショットをそのまま
[Issue](https://github.com/waired-ai/waired-agent/issues) に添付できます。
