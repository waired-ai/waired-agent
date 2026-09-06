---
title: セットアップの問題
description: セットアップが止まった、推論エンジンが起動しない、モデルをダウンロードできない、小さいモデルやモデルなしで終わった、といった症状の対処です。
meta:
  audience: セットアップが想定どおりに終わらなかった人
  needs: そのパソコンのターミナル
  time: 各対処は1〜2分
---

まず`waired doctor`を実行してください。準備ができていない部分が表示されます。そのあと、下から症状を探します。

## <a id="setup-stopped-partway"></a>セットアップが途中で止まった

セットアップページに何が起きたかが表示されます。各メッセージの意味は次のとおりです。

| ページの表示 | 意味 | 対処 |
|---|---|---|
| 「… のセットアップコマンドが完了前に終了しました。進捗は保存されています。」 | セットアップを実行していたターミナルが閉じられました。一部の手順には管理者権限が必要で、そのターミナルだけがそれを持っています。 | `sudo waired init`をもう一度実行します（Windowsでは管理者のターミナルで`waired init`）。続きから再開し、失われるものはありません。 |
| 「… ではセットアップが実行されていないため、コーディングツールは接続されていません。」 | そのパソコンで誰もセットアップコマンドを実行していません。ウェブページはホームフォルダに書き込むことも、パソコン全体の設定を変えることもできません。 | そのパソコンで`sudo waired init`を実行します。そのパソコンのほかの設定はブラウザからできますが、この手順だけはできません。 |
| 「… ではセットアップが実行されていないため、推論エンジンがインストールされていません。」 | 同じことが1つ前の手順で起きています。推論エンジンのインストールには、セットアップコマンドだけが持つ管理者権限が必要です。 | そのパソコンで`sudo waired init`を実行します。中断されたのではなく、まだ初回の実行が行われていません。 |
| 「… でのセットアップの続行には管理者権限が必要です。」 | セットアップが管理者権限なしで始められました。 | 管理者のターミナルからもう一度始めます。[サインインする](/ja/getting-started/sign-in/)を参照してください。 |
| 「… のディスク空き容量が不足しています。」 | モデルがディスクに収まりませんでした。 | 空き容量を確保するか、[カタログ](/ja/reference/model-catalog/)から小さいモデルを選びます。 |
| 「… でダウンロードを完了できませんでした。インターネット接続を確認してください。」 | 名前解決の失敗、接続の切断、証明書の検証失敗など、ネットワークの理由でダウンロードが失敗しました。 | 再試行します。ダウンロードは最初からではなく途中から再開します。 |
| 「… の推論エンジンのバージョンが古いため、このモデルを利用できません。」 | モデルが、このパソコンにあるものより新しい推論エンジンを必要としています。 | そのパソコンで`waired update`を実行するか、別のモデルを選びます。 |
| 「… での処理に時間がかかりすぎたため、停止しました。」 | 手順が制限時間を超えました。 | 再試行します。同じ手順で2回起きる場合は、そのモデルにはこのパソコンが遅すぎることが普通です。 |
| 「… で問題が発生しました。」 | Wairedは何が起きたかを特定できませんでした。 | 再試行します。繰り返す場合は、そのパソコンで`waired doctor`を実行するか、ログを読みます。[ログを読む](/ja/troubleshooting/other-computers/#reading-the-logs)を参照してください。 |

失敗したのがコーディングツールの手順なら、そのパソコンで`waired link --force all`を実行すると、修復と表示の更新の両方が行われます。セットアップをやり直す必要はありません。

モデルのダウンロードだけは例外で、ブラウザのタブを閉じても続きます。進み具合は[app.waired.ai](https://app.waired.ai)でそのパソコンのページを開くと分かります。

ターミナルで見ている場合、`waired init`と`waired models pull`は失敗を報告する行に理由を表示します。

```text
qwen3-8b-instruct: failed — no space left on device
```

## <a id="setup-says-the-inference-engine-failed-to-start"></a>セットアップが推論エンジンを起動できなかったと言う

ターミナルでは、セットアップはモデルを待つのをやめて、推論エンジンが原因だと伝えます。

```
The inference engine failed to start, so qwen3.5-4b can't download.
ollama: process exited during startup: signal: killed
Run `waired doctor` for details; `waired status` shows the current state.
```

2行目は、推論エンジン自身が記録した内容をそのまま表示したもので、そのあとに推論エンジンのログの末尾が続くことがよくあります。まずここを読みます。

この時点でサインインは完了しています。パソコンはネットワークに加わっていて、ローカル推論以外はすべて動きます。Wairedはバックグラウンドで試行を続けるので、推論エンジンが動けばダウンロードは自動で始まります。`waired init`はこの場合、終了コード`3`で終わります。スクリプトからは、サインインが行われなかった場合と区別できます。

| 終了コード | 意味 |
|---|---|
| `0` | サインイン済みで、ローカル推論が動いているか、最初から求められていません。 |
| `3` | サインイン済みですが、このパソコンでローカル推論が動いていません。 |
| `1` | セットアップが完了していません。サインイン自体が失敗しました。 |
| `130` | Ctrl-Cで中断しました。 |

よくある原因は次のとおりです。

- **別のOllamaがすでにポートを使っている。** `waired runtimes status`が見つけたバージョンを表示します。それを終了するか、`agent.json`の`inference.ollama_port`を空いているポートに設定します。
- **Ollama以外のものがそのポートを使っている。** Wairedはそれを引き継げず、`waired status`がアドレスを表示します。

  ```
  ⚠ ollama: another program is already listening on 127.0.0.1:9475, the port the
    inference engine was told to use — set inference.ollama_port in agent.json to
    a free port
  ```

  それを終了するか、`inference.ollama_port`を空いているポートに設定してサービスを再起動します。vLLMも自身のポート`inference.vllm_port`について同じことを表示します。
- **推論エンジンがクラッシュを繰り返す。** 数回クラッシュすると、Wairedは再起動をやめてそのことを表示します。`waired status`と`waired runtimes ls`は、推論エンジンの状態の代わりに**gave up**と表示します。

  ```
  runtimes:       ollama 0.33.3 (gave up, ctx 32k q8_0)
  ⚠ ollama: engine repeatedly crashed; not retrying — …
  ```

  原因に対処してから、`waired inference engine start`を実行します。
- **vLLMが一度も起動していない。** vLLMは自身のセットアップが必要です。`waired runtimes install vllm`でPython環境を構築し、vLLMで動かせる版のあるモデルを選ぶことです。どちらかが欠けていると、Wairedは何が欠けているかを表示します。

`waired doctor`はこれらを一度に検査し、`sudo waired doctor --fix`はバックグラウンドサービスに推論エンジンの起動を依頼して、動いていない理由を表示します。

## <a id="setup-says-it-cannot-download-the-model-you-chose"></a>セットアップが選んだモデルをダウンロードできないと言う

パソコンによっては動かないモデルがあります。バックグラウンドサービスが選択を断ると、ターミナルはすぐにそのことを表示します。

```
Waired can't download qwen3.6-35b-a3b on this computer.
the engine on this device is too old for this model
Update Waired here (`waired update`), or pick a different model in your browser.
```

中央の行は、バックグラウンドサービスが記録した理由です。最後の行はその理由で変わります。

- **推論エンジンがモデルの要件より古い。** そのパソコンで`waired update`を実行します。そのあとダウンロードは自動で始まります。アップデートで直るのはこの理由だけです。
- **それ以外。** そのパソコンではそのモデルをまったく動かせないか、ダウンロードがオフになっています。ブラウザで別のモデルを選ぶか、`waired models ls --detail`で収まるモデルを確認します。

どちらの場合もサインインは完了していて、セットアップページのモデルの行にも同じ理由が表示されるので、そこで選び直せます。

似た表示で意味の違うものがあります。

```
Waired hasn't started downloading qwen3.6-35b-a3b yet; it keeps trying in the background.
```

こちらは拒否ではありません。ターミナルが見るのをやめた時点でダウンロードがまだ始まっていなかっただけで、バックグラウンドで続いています。進み具合は`waired status`で分かります。

## <a id="setup-said-it-could-not-complete-a-test-generation"></a>セットアップがテストの生成を完了できなかったと言う

セットアップの最後に、Wairedはこのパソコンの速度を確認するためにモデルに短い質問をします。このメッセージは、答えが返ってこなかったために何も計測できなかったことを意味します。

ほぼ常に、推論エンジン自体が止まっています。確認します。

```sh
waired status
waired doctor
```

`waired status`は推論エンジンの行に推論エンジン自身の理由を表示します。クラッシュした場合の詳細はログにあります。[ログを読む](/ja/troubleshooting/other-computers/#reading-the-logs)を参照してください。

Wairedのほかの部分に影響はありません。パソコンはサインインしたままで、自分のほかのパソコンのモデルを使えます。推論エンジンが正常になったら、もう一度計測します。

```sh
waired runtimes benchmark
```

## <a id="waired-chose-a-very-small-model-for-my-computer"></a>Wairedがとても小さいモデルを選んだ

それは、コーディングセッション全体をメモリに保持したまま、このパソコンが載せられるいちばん大きいモデルで、Wairedはそれを動かします。収まっても長い会話の大半を捨てなければならないモデルは、良い選択ではありません。

何が選ばれ、なぜかを見るには次のように実行します。

```sh
waired models ls --detail
```

**SIZE**の列はそのモデルがどの階級のGPU向けか、**FIT**の列はこのパソコンに載せられるかを示します。一覧にあるものはどれでも選べます。

```sh
waired models use <model>
```

大きいモデルも動きますが、会話の間ずっとシステムRAMから読み込み直すことになり、長いコーディングのセッションでいちばん遅さを感じます。`waired inference off`は、このパソコンでモデルを動かすのを完全にやめます。パソコンはネットワークに残り、自分のほかのパソコンのモデルを使えます。

## <a id="local-inference-started-off-and-i-did-not-choose-that"></a>選んでいないのにローカル推論がオフで始まった

パソコンに理由を聞きます。

```sh
waired inference status
```

Wairedが決めた場合は、答えにそのことが示されます。

```
Local inference: off
  This computer is below the recommended spec for local inference.
  per request           210.4 s or more
  target                45 s or less
  It can still use the models running on your other computers.
  Turn it on with `waired inference on`.
```

この数値の出どころは次のとおりです。推論エンジンのインストール直後、フルサイズのモデルをダウンロードする前に、Wairedは小さいモデルをダウンロードし、現実的なリクエストを3回計測して中央の値を採ります。目標を大きく下回るパソコンでは、計測を早めに切り上げ、正確な数値の代わりに`210.4 s or more`と表示します。詳しくは[Wairedがモデルを選ぶ仕組み](/ja/guides/how-a-model-is-chosen/#the-timing-that-can-leave-local-inference-off)を参照してください。

これは出発点であって、最終的な判定ではありません。パソコンはネットワークに加わっていて、自分のほかのパソコンのモデルを使えます。ローカル推論はいつでもオンにできます。

```sh
waired inference on
```

Wairedアプリでは、同じ選択肢が［Run models on this computer］です。一度選ぶと、Wairedはその選択を保ちます。

`waired inference status`が**off**と表示して理由を示さない場合、このパソコン側で決めたことではありません。セットアップ中、インストーラの`--inference-enabled false`、または`waired inference off`で選ばれたものです。

## <a id="it-says-local-inference-is-not-set-up-yet"></a>ローカル推論がまだセットアップされていないと表示される

```sh
waired inference status
```

```
Local inference: not set up yet — this device is not signed in. Run `waired init`.
```

Wairedのインストールとサインインの間の状態です。何も問題はなく、変える設定もありません。パソコンには、モデルを動かす対象のアカウントがまだありません。[サインイン](/ja/getting-started/sign-in/)すると、答えは**on**か**off**になります。

## <a id="this-computer-has-no-inference-engine"></a>このパソコンに推論エンジンがない

推論エンジンが入らないパソコンもあります。Wairedは、このパソコンでモデルを動かすと答えた場合にだけ推論エンジンをインストールします。`waired models ls --detail`は表の上にそのことを表示します。

```
Host: Intel Arc 8 GB VRAM / 63 GB RAM · no inference engine installed

! No inference engine is installed on this computer, so it cannot run a model itself.
  Requests go to your other computers instead.
  Install one with `sudo waired runtimes install ollama`.
  The verdicts below are what this computer would run once an engine is installed.
```

これは正常で、不具合ではありません。パソコンはサインインしたまま動き続けます。リクエストにはネットワークのほかのパソコンが答えます。推論エンジンはいつでもインストールできます。管理者のターミナルで次のように実行します。

```sh
sudo waired runtimes install ollama
```

推論エンジンのないパソコンでWairedアプリからモデルを選ぶと、先に推論エンジンのインストールが提案されます。推論エンジンがあるはずだと考えている場合、いちばんありそうな理由は、セットアップで「このパソコンでモデルを動かさない」と答えたことです。[セットアップをやり直す](/ja/getting-started/set-up-again/)を参照してください。

## <a id="a-model-says-it-needs-a-newer-inference-engine"></a>モデルが新しい推論エンジンを必要としていると表示される

最近のビルドの推論エンジンでしか動かないモデルがあり、行にそのことが表示されます。

```
qwen3.8-27b   27B   medium   ✗ needs Ollama 0.32.13 (this computer has 0.31.1)
```

メモリの問題ではありません。このパソコンの推論エンジンが、モデルの要件より古いだけです。推論エンジンはWairedが管理しているので、通常のアップデートで直ります。

```sh
waired update
```

アップデートで、推論エンジンはこのビルドのWairedに同梱されたバージョンになり、そのあと行の表示は消えます。それまでの間、Wairedは現在の推論エンジンで動かせるモデルを選び続けます。

この行の末尾には、ほかに2つの形があります。

- **`(this computer's version could not be read)`**：推論エンジンはあるが一度も起動していません。`waired inference engine start`を実行してから見直します。
- **`(no inference engine on this computer)`**：推論エンジンがありません。[このパソコンに推論エンジンがない](#this-computer-has-no-inference-engine)を参照してください。
