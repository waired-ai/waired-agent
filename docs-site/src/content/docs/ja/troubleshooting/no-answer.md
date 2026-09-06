---
title: 応答が返らない
description: 答えが返ってこない、推論エンジンがnot readyのまま、バックグラウンドサービスが動いていない、リクエストが502になる、といった症状の対処です。
meta:
  audience: モデルが答えなくなった人
  needs: そのパソコンのターミナル
  time: 各対処は1〜2分
---

まず`waired doctor`を実行してください。準備ができていない部分が表示されます。そのあと、下から症状を探します。

## <a id="no-answer-comes-back"></a>答えが返ってこない

推論エンジンの状態を確認します。

```sh
waired status --observability
```

見るのは**Engine**の行です。

- **`ready`**：モデルは読み込まれています。それでもリクエストが失敗するなら、問題はルーティングです。[Claude Codeがクラウドを使ったまま](/ja/troubleshooting/claude-code/#claude-code-is-still-using-the-cloud)を参照してください。
- **`not ready`**：多くの場合、モデルをまだダウンロード中です。`waired models ls`で進み具合が分かります。最初のモデルは数GBあります。
- **ダウンロードが終わっても`not ready`**：モデルがこのパソコンのメモリに収まっていない可能性が高いです。小さいモデルに切り替えます。[モデルを変更する](/ja/guides/choose-a-model/)を参照してください。
- **`engine failed`**：推論エンジンが自然に止まりました。Wairedは最大3回まで自動で再起動するので、通常は1分以内に直ります。止まった理由は同じ行に表示されます。繰り返す場合、Wairedは再起動をやめてそのことを表示します。理由が示すものを直してから、`waired inference engine start`を実行します。よくある原因は、このパソコンには大きすぎるモデルです。

ほかに知っておくとよい原因が2つあります。

- モデルは答える前にメモリに読み込まれる必要があり、推論エンジンの起動後の最初のリクエストがそれを待ちます。
- **503**は、ルーティングが一時停止している（`waired resume`）か、共有がオフになっている（`waired share on`）ことを意味します。

### <a id="is-it-working-or-is-it-stuck"></a>動いているのか、止まっているのか

`waired status`が両方に答えます。

```
  model loaded:   ollama: no (the next request reloads it)
  serving now:    0 requests
```

- **`model loaded:`**は、モデルがメモリにあるかどうかです。`no`なら次のリクエストが先に読み込みを行い、そのリクエストが遅くなります。
- **`serving now:`**は、このパソコンが処理中のリクエストの数です。コーディングツールがしばらく何も言わないのに`0 requests`なら、待たされているのはこのパソコンではありません。モデルではなくルーティングを確認します。
- **`last turn:`**は、直前の答えが始まるまでにかかった時間です。このパソコンが何かに答えたあとに表示されます。

Claude Codeは処理中、フッターに同じ内容を表示します。`⚡ waired: on Waired (qwen3-8b-instruct) · model not loaded`。このパソコンが答える場合、読み込みにどれだけかかっても、答えが始まるまで接続は保たれます。代わりに`⚠ waired: Waired cannot answer (…)`と表示される場合は、自分のパソコンのどれもそのターンを受けられません。[Claude CodeがWairedは答えられないと言う](/ja/troubleshooting/claude-code/#claude-code-says-waired-cannot-answer)を参照してください。

それでも止まったままなら、`waired runtimes status`が推論エンジン自体の状態を報告します。ログは[ログを読む](/ja/troubleshooting/other-computers/#reading-the-logs)を参照してください。

## <a id="the-waired-icon-says-the-background-service-is-not-running"></a>Wairedアイコンがバックグラウンドサービスが動いていないと言う

Wairedのメニューを開き、［Start the background service…］を選択します。パソコンが管理者権限を求めます。これはOS自身の確認画面で、バックグラウンドサービスがパソコン全体のものであるために必要です。コマンドを自分で実行したい場合は、［Copy start command］がこのパソコン用のコマンドをクリップボードに置きます。

このメニューは2つの状態を区別します。

- **Background service is starting…**は正常です。Windowsでは、サービスはログインの数分後に始まる設定なので、Wairedアイコンのほうが先に表示されます。待つか、いま始めるかを選べます。
- **Background service is not running**は、動いているはずなのに動いていない状態です。メニューから始め、戻らなければ`waired doctor`を実行します。

サービスを手動で止めても、その状態は続きません。パソコンの起動時に再び始まります。

## <a id="a-command-says-waired-agent-is-not-running"></a>コマンドが「waired-agent is not running」と言う

バックグラウンドサービスが止まっています。

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows、管理者
```

macOSではシステムが自動で再起動します。戻らない場合は、`waired doctor`を実行するか、パソコンを再起動します。一度も起動したことがない場合は、[次の項目](#macos-the-background-service-never-starts)を参照してください。

再起動は一時的な不整合の大半も解消するので、込み入った対処の前に試す価値があります。

### <a id="windows-it-stopped-starting-at-boot-on-its-own"></a>Windows：起動時に自動で始まらなくなった

Wairedのプログラムは、Windowsが認識する証明書でまだ署名されていないため、Windowsが起動時にバックグラウンドサービスを止めることがあります。Smart App Controlがオンの場合、ネットワークでの確認なしに起動時に判定し、サービスは始まりません。一貫性はなく、同じパソコンが次の起動では正常に始まることもあります。

Wairedが署名済みのプログラムを配布するまでは、この状態になったらWairedのメニューからサービスを始めてください。壊れているものはなく、同じサービスを手動で始めることはできます。[Windowsがプログラムの実行を拒否したとき](/ja/getting-started/install/windows/#if-windows-refuses-to-run-a-program)を参照してください。

## <a id="macos-the-background-service-never-starts"></a>macOS：バックグラウンドサービスが一度も起動しない

インストーラは完了したのにサービスが一度も起動せず、`--clean`を付けて再インストールしても変わらない。この組み合わせは、macOSがサービスを無効としてマークしていることを意味するのが普通です。2026年7月15日からこのリリースまでの間にインストールして削除したWairedがこのマークを残し、アンインストール、再インストール、パソコンの再起動でも消えません。

確認します。

```sh
sudo launchctl print-disabled system | grep waired
```

`"com.waired.agent" => true`なら無効になっています。マークを消してサービスを始めます。

```sh
sudo launchctl enable system/com.waired.agent
sudo launchctl bootstrap system /Library/LaunchDaemons/com.waired.agent.plist
```

現在のWairedはインストールとアップデートの際にこれを消すので、これらのコマンドが必要なのは、インストーラ自体を実行できないパソコンだけです。

## <a id="windows-i-get-a-502-error"></a>Windows：502エラーになる

このパソコンに推論エンジンがインストールされていません。`-SkipOllama`または`WAIRED_NO_OLLAMA=1`でインストールした場合によく起きます。管理者のターミナルで次のように実行します。

```powershell
waired runtimes install ollama
```
