---
title: セットアップとサインインのコマンド
description: waired init、status、doctor、auth、logoutについて、重要なフラグ、終了コード、それぞれの出力を説明します。
meta:
  audience: ターミナルで作業する人、画面のないパソコンを使う人
  needs: インストール済みのWaired
  time: 必要なコマンドを読むだけ
---

## <a id="waired-init"></a>`waired init`

このパソコンをサインインさせてセットアップします。通常はインストーラが実行するので、自分で入力するのは、中断したセットアップを再開するとき、`--no-init`でインストールしたパソコンをセットアップするとき、サインインし直すときです。

```sh
sudo waired init            # macOS、Linux
waired init                 # Windowsでは管理者のターミナルで
```

推論エンジンをインストールするため、管理者権限が必要です。実行中は、ブラウザのセットアップページが求める手順を実行する役割も担うので、セットアップが終わるまでウィンドウを開いたままにします。[サインインする](/ja/getting-started/sign-in/)と[ターミナルでセットアップする](/ja/getting-started/set-up-in-the-terminal/)を参照してください。

| フラグ | 用途 |
|---|---|
| `--mask-pii` | 出力中のホームフォルダ、ユーザー名、マシン名、アカウントのメールアドレスを伏せます。不具合報告に貼り付けるためのものです。最善の努力です。 |
| `--non-interactive` | 何も聞かず既定値を採用します。スクリプトでのインストール向けです。 |
| `--no-browser` | ブラウザを開かず、サインインのリンクとペアリングコードを表示します。SSH向けです。 |
| `--inference-enabled=true`または`=false` | 「run models on this computer?」に聞かれずに答えます。 |
| `--inference-bundled-model-id <id>` | 一覧から選ぶ代わりにモデルを固定します。 |
| `--skip-claude-route` | セットアップは終えますが、Claude CodeはAnthropic APIと通信したままにします。スキルとプラグインはインストールされます。ルーティングはあとから`waired claude enable`でオンにできます。 |
| `--skip-integration` | コーディングツールの設定を完全に省きます。 |
| `--device-name <name>` | このパソコンのホスト名の代わりに、指定した名前を報告します。最初に参加するときに使われます。あとから名前を変えるのは[Webコンソール](/ja/guides/web-console/)で行います。 |
| `--control <URL>` | 特定のコントロールプレーンに対してサインインします。[高度なインストールオプション](/ja/reference/install-options/)を参照してください。 |
| `--auth-key <key>` | ブラウザの代わりに認証キーでサインインします。サーバーとコンテナ向けです。`file:/path/to/key`の形も受け付け、フラグを省くと`$WAIRED_AUTH_KEY`を読みます。[認証キーでサーバーをセットアップする](/ja/getting-started/servers-and-auth-keys/)を参照してください。 |
| `--force-reauth` | サインイン済みのパソコンでサインインし直します。これがないと、`waired init`はセットアップを再開し、既存のサインインには触れません。`--auth-key`を渡した場合も同様です。 |

`waired init --help`が正式な一覧です。ここに載せていない開発者向けとCI向けのフラグも含まれます。

サインイン済みのパソコンでもう一度実行しても問題ありません。最初からサインインするのではなく、セットアップを再開します。[セットアップをやり直す](/ja/getting-started/set-up-again/)を参照してください。

**終了コード**（スクリプト向け）：

| コード | 意味 |
|---|---|
| `0` | サインイン済みで、ローカル推論が動いているか、最初から求められていません。 |
| `3` | サインイン済みですが、このパソコンでローカル推論が動いていません。推論エンジンをインストールできなかったか、起動し続けられませんでした。[セットアップが推論エンジンを起動できなかったと言う](/ja/troubleshooting/setup/#setup-says-the-inference-engine-failed-to-start)を参照してください。 |
| `1` | セットアップが完了していません。サインイン自体が失敗しました。 |
| `130` | Ctrl-Cで中断しました。 |

`3`を`1`と分けているのは意図的です。パソコンはサインイン済みでネットワークに加わっていて、サインインをやり直しても推論エンジンについては何も変わりません。エラーではない2つの状態は`0`で終わります。`WAIRED_NO_OLLAMA`で自分で推論エンジンのインストールをオフにした場合と、セットアップがターミナルを返した時点でモデルのダウンロードが終わっていない場合です。ダウンロードの進捗は`waired status`が報告します。

## <a id="waired-status"></a>`waired status`

「動いているか」を手早く確認します。

```sh
waired status
waired status --observability     # 推論エンジン、モデル、自分のほかのパソコン
waired status --observability -o json
```

通常のデスクトップのインストールでは状態はシステムのものなので、すべてを見るには`sudo`で、Windowsでは管理者のターミナルで実行します。管理者権限がないと、このパソコンはシステム全体でサインインしていると報告してそこで止まります。

モデルを動かすパソコンでは、`Inference:`のブロックが推論エンジンの現在の状態を報告します。

```
Inference:
  state:          ready
  runtimes:       ollama 0.33.3 (ready, ctx 200k q8_0)
  model loaded:   ollama: qwen3:8b-q4_K_M (kept until unloaded)
  first token:    35.4s, 12 minutes ago (fastest seen here: 2.6s)
  models ready:   qwen3-8b-instruct
```

`model loaded:`は重みがメモリにあるかどうかです。`first token:`は直前の答えが始まるまでにかかった時間で、Wairedの前回の再起動以降に同じモデルでこのパソコンが記録したいちばん速い値が並びます。モデルが載っていてもプロンプト全体を読み直すことがあり、それが2つの値の差です。計測した値がないときは、この行は表示されません。

その下の`Notices:`は、Wairedがこのパソコンについて伝えたいことです。

```
Notices:
  ⚠ Lighter model recommended — switch to qwen3-8b-instruct
    This computer answers at 42 tok/s with qwen3-30b-a3b, below the 60 tok/s floor.
  ⬆ Update available — install v0.9.3
    This computer runs v0.9.1.
```

伝えることがなければ、このブロックは表示されません。すべてのお知らせは[お知らせ](/ja/guides/notices/)を参照してください。

## <a id="waired-doctor"></a>`waired doctor`

セットアップの各項目を検査し、検査ごとに✓、⚠、✗を表示し、`f`キーを押すと直せるものの修復を提案します。詳しくは[診断を実行する](/ja/getting-started/doctor/)を参照してください。

```sh
waired doctor
waired doctor --fix              # 確認なしで修復する。スクリプトとSSH向け
```

Wairedがこのパソコンについて伝えたいことも、⚠としてこれらの行に含まれます。終了コードには影響しません。

## <a id="waired-auth-status"></a>`waired auth status`

サインインの状態と失効する時期を表示し、更新が必要なら`init`をもう一度実行するよう伝えます。サービスとしてインストールした環境では、`status`と同じく管理者権限が必要です。

更新は、最初に実行したのと同じ`waired init`です。このパソコンがサインイン済みであることを認識し、続行前に確認し、サインインだけを置き換えます。設定、推論エンジン、ネットワーク上のこのパソコンの位置はそのままです。サインインを保持しているのはバックグラウンドサービスなので、Wairedがバックグラウンドで動いている必要があります。

## <a id="waired-logout"></a>`waired logout`

このパソコンの識別情報と秘密情報を削除し、次の`waired init`で新しいデバイスとして登録されるようにします。一時的な手段ではありません。しばらくWairedを使わないだけなら、[Wairedを一時停止する](/ja/guides/pause/)を参照してください。

バックグラウンドサービスが動いているときは、サービス自身がサインアウトを実行します。アクセストークンの期限切れを待たずに、その場で以前のサインインの提供をやめます。何も動いていないとき、たとえばアンインストールの途中では、コマンドが同じ処理を自分で行います。
