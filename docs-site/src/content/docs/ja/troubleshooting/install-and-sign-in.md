---
title: インストールとサインインの問題
description: コマンドが見つからない、ブラウザが開かない、サインインのリンクが失効した、サービスが応答しない、サインアウトしていると表示される、といった症状の対処です。
meta:
  audience: インストールとサインインの間で止まっている人
  needs: そのパソコンのターミナル
  time: 各対処は1〜2分
---

まず`waired doctor`を実行してください。準備ができていない部分が表示されます。そのあと、下から症状を探します。

## <a id="i-typed-waired-and-got-command-not-found"></a>`waired`と入力すると「command not found」になる

インストールが終わっていないか、インストールの前から開いていたターミナルが新しいコマンドをまだ認識していないかのどちらかです。

1. ターミナルを閉じて、新しく開きます。動作中のシェルはコマンドの場所をキャッシュしているので、たいていはこれだけで直ります。
2. それでも見つからなければ、インストールコマンドをもう一度実行します。2回実行しても問題ありません。[Wairedをインストールする](/ja/getting-started/install/)を参照してください。

Windowsでは、コマンドは`C:\Program Files\Waired\waired.exe`にあります。`waired`だけで動かないときも、このフルパスなら動きます。

## <a id="no-browser-opened-at-sign-in-or-the-wrong-one-did"></a>サインイン時にブラウザが開かない、または別のブラウザが開く

サインインのリンクは、何かが開く前に必ずターミナルに表示されるので、いつでも手動で続けられます。リンクをコピーして、普段使うブラウザに貼り付けます。そこでサインインし、セットアップの残りもそのブラウザで進めます。セットアップページは、サインインしたブラウザでしか動きません。

普段使わないブラウザが開いた場合は、サインインせずに閉じて、表示されたリンクを使いたいブラウザで開きます。

どちらも、セットアップが管理者権限で動いていたことが原因で、現在のバージョンでは修正されています。

```sh
waired update
```

## <a id="the-sign-in-link-expired-before-i-finished"></a>サインインを終える前にリンクが失効した

`waired init`が表示するリンクの有効期間は限られています。別の部屋のスマートフォンでの2要素認証や、ほかの作業をしている間に開いたままのタブだけで、期限が尽きることがあります。期限が切れると、ターミナルは次のように止まります。

```
waired: login expired. Run `waired init` again
```

そのとおりに実行します。

```sh
sudo waired init        # Windowsでは管理者のターミナルで waired init
```

壊れているものも、片付けるものもありません。コマンドが新しいリンクを表示するので、それでサインインします。

ターミナルが止まったあとにブラウザでサインインを終えた場合、ブラウザはサインインのリンクが失効したと表示し、ターミナルに戻るよう案内します。Webコンソールのデバイス一覧まで進んでいた場合は、このパソコンの登録が終わる前にサインインが失効したとバナーに表示されます。3つの画面が同じことを言っています。パソコンを登録するのはターミナルで待っているコマンドで、そのコマンドが止まったということです。

## <a id="sign-in-stops-because-the-background-service-is-not-responding"></a>バックグラウンドサービスが応答しないためサインインが止まる

サインインはバックグラウンドサービスを通じて行われます。サービスがWairedと通信し、そのあともこのパソコンを接続し続けます。サービスが応答しないと、サービスなしで続けるのではなく、サインインが止まります。

```
Waired's background service is installed but isn't responding, so sign-in can't continue.
  Check what's wrong:  waired doctor
  Start it:            sudo systemctl start waired-agent
  Then run again:      sudo waired init
```

この3行を順に実行します。`waired doctor`が原因を表示します。macOSでよくある原因は[サービスが一度も起動しない](/ja/troubleshooting/no-answer/#macos-the-background-service-never-starts)ことです。

代わりに**「Waired isn't running in the background」**と表示される場合は、このパソコンにバックグラウンドサービスが登録されていません。通常は、インストールせずにプログラムを直接実行しているときです。先に`waired-agent`を起動してから、`waired init`をもう一度実行します。

### <a id="sign-in-worked-but-the-setup-steps-did-not-run"></a>サインインはできたが、セットアップの手順が実行されない

バックグラウンドサービスへの読み取りと書き込みは別の経路を通るので、片方だけ届くことがあります。セットアップがサービスに届かない場合、`waired init`はそのことを表示します。

```text
Warning: couldn't ask the background service about setup (…). Its setup steps will be skipped. Run `waired doctor` to see why.
```

この実行では、バックグラウンドサービスが必要な手順が飛ばされます。推論エンジンのインストール、コーディングツールの接続、ブラウザへの進捗の報告です。サインイン自体には影響ありません。

軽い形のものは、問い合わせは届いたが最初の更新だけが届かなかったことを示します。

```text
Warning: couldn't tell the background service that setup is running (…). Retrying in the background. If the browser shows no progress, run `waired doctor`.
```

こちらは約10秒で自動的に直ります。それでもブラウザに進捗が出ない場合は、`waired doctor`を実行します。

## <a id="i-signed-in-but-waired-says-i-am-signed-out"></a>サインインしたのに、Wairedはサインアウトしていると言う

Wairedアイコンに「Not signed in」と表示される、またはアカウントからパソコンが消えている。サインアウトしていないのに、再起動の直後に起きることが多い症状です。外から見ると同じに見える2つの状態があり、`waired doctor`が区別します。

**`network connection`が⚠の場合。** サインインはできていて、このパソコンがまだ接続していないだけです。Wairedは自動で再試行を続けます。普段使うネットワークのポートが再起動後にほかのものに取られていた場合も同様です。1分待ってからもう一度確認してください。いつまでも直らない場合は、バックグラウンドサービスを再起動します。

```sh
sudo systemctl restart waired-agent    # Linux
Restart-Service waired-agent           # Windows、管理者
```

**`device sign-in`が✗の場合。** このパソコンのサインインが本当に無効になっていて、サインインし直すことでだけ戻ります。

```sh
sudo waired init      # LinuxとmacOS
waired init           # Windows、管理者
```

モデル、設定、コーディングツールの設定はすべて残ります。このパソコンのアカウント上の位置を再確立するだけです。その間もローカル推論は答え続けます。止まるのはアカウントが必要なものだけで、Webコンソールからパソコンが消え、サインインし直すまでほかのパソコンから届かなくなります。

## <a id="it-says-i-have-reached-the-device-limit"></a>デバイスの上限に達したと表示される

アカウントごとに登録できるデバイスの数は十分にありますが、使わなくなった古いパソコンが数えられたままになっていることがよくある原因です。

[app.waired.ai](https://app.waired.ai)を開き、不要なデバイスを削除してから、もう一度セットアップします。サインイン済みのパソコンでセットアップをやり直しても、上限には数えられません。

## <a id="it-says-the-device-is-enrolled-system-wide"></a>デバイスが「enrolled system-wide」だと表示される

これはエラーではありません。デバイスの識別情報は管理者しか読めないシステムのフォルダにあるので、一般ユーザーで実行した`waired status`はそれを読めません。推測する代わりに、デバイスは登録済みだと伝えて正常終了します。完全な状態を見るには、管理者権限で実行します。

```sh
sudo waired status          # Windowsでは管理者のターミナルで
```

`waired doctor`もそのパソコンでは**state directory**の行で同じことを伝え、失敗ではなく実行できなかった検査として扱います。[診断自体が全体を見られない場合](/ja/getting-started/doctor/#when-the-check-itself-cannot-see-everything)を参照してください。

代わりに`Not enrolled. Run 'waired init' to connect this device.`と表示される場合は、このパソコンはまだセットアップされていません。[サインインする](/ja/getting-started/sign-in/)を参照してください。
