---
title: モデルとエンジンのコマンド
description: waired infer、models、runtimes、inferenceについて、各動詞の動作、スクリプトが答える必要のある確認、出力の意味を説明します。
meta:
  audience: ターミナルで作業する人、画面のないパソコンを使う人
  needs: インストール済みのWaired
  time: 必要なコマンドを読むだけ
---

## <a id="waired-infer"></a>`waired infer`

自分のモデルにプロンプトを1つ送り、答えを表示します。経路全体が動くことを確かめる
いちばん速い方法です。

```sh
waired infer "say hi"
waired infer "say hi" --explain    # 送らずに、どのパソコンとモデルが答えるかを表示する
waired infer "say hi" --model <model-id>
```

`--explain`は、答えるパソコンを[`waired peers list`](/ja/reference/cli/routing/#waired-peers)と
同じ形で、`DEVICE-ID`とともに表示します。どちらも`waired worker set --pin`に渡せ
ます。除外されたパソコンとその理由も表示し、判断の元になった情報の古さを
`map_age_ms`として報告します。公開のパソコンは、Wairedが表示する仮名だけで
示されます。

## <a id="waired-models"></a>`waired models`

```sh
waired models ls                  # ダウンロード済みのモデルと、動作中のモデル
waired models ls --detail         # カタログ全体と、このパソコンに収まるかどうか
waired models pull <model-id>     # ダウンロードする
waired models use <model-id>      # このパソコンが動かすモデルにする
waired models cancel <model-id>   # 実行中のダウンロードを止める
waired models rm <model-id>       # 削除して数GBを空ける
waired models refresh             # このパソコンにより良い選択があるか
waired models check-agent         # このモデルはコーディングエージェントで使えるか
```

**`ls`**は、各モデルがディスクで占める容量を**SIZE**に表示します。`rm`で戻る
容量を知る方法です。この値は推論エンジンから来るので、ダウンロード済みでも推論
エンジンが止まっているモデルは`-`と表示されます。`--detail`を付けると、カタログの
すべてのモデルについて、必要なメモリ、このパソコンに収まるか、Wairedがどれを選ぶかが
表示されます。表には凡例が付きます。記号を書けない環境では、ASCIIで表示されます
（`●`は`*`、`→`は`->`、`◦`は`o`、`↓`は`v`）。

**`pull`**は、モデルの準備ができるまで待ちます。ここで動くがWairedが選ぶものでは
ないモデルは確認されます。`--yes`でこの確認を省略できます。このパソコンのメモリに
収まらないモデルは、不足分を示してもう一度確認され、既定の答えはNoです。`--yes`
だけではこの確認は省略されません。本当に進めるスクリプトは`--yes --force`を渡し
ます。モデルIDは[モデルカタログ](/ja/reference/model-catalog/)にあります。

**`use`**は、このパソコンが動かすモデルを設定します。`pull`は重みを取得するだけ
です。切り替えに再起動は不要です。動作中のモデルは新しいモデルの準備ができるまで
答え続け、重みがまだディスクにない場合は`use`がダウンロードを始めてそのことを
表示します。

```
waired models use qwen3.5-4b
qwen3.5-4b will run on this computer once it finishes downloading.
The current model keeps answering until then.
```

サービスが選択を受け付けた時点で戻ります。`--wait`を付けると、新しいモデルが
応答を始めるまで待ちます。確認は`pull`と同じように動きます。

**`cancel`**は、実行中のダウンロードを確認なしに止めます。止めたジョブを表示するか、
`no download in progress for <model>`と表示します。そのダウンロードを待っていた
`pull`も止まり、0以外で終了します。ダウンロード済みの部分はディスクに残るので、
同じモデルをもう一度取得すると途中から再開します。`cancel`は`use`を取り消し
ません。そのモデルを選んでいた場合は選択のままで、重みが届いたときに適用され
ます。

**`rm`**は、モデルのファイルを削除します。先に確認されるか、`--yes`を受け付け
ます。そのモデルのダウンロードが実行中なら先に止めます。このパソコンのほかの
モデルが同じファイルを共有している場合、ファイルは残り、項目だけが消えます。

**`refresh`**は、このパソコンが動かしているモデルより良い選択があるかを表示します。

**`check-agent`**は、ほかのコマンドが答えない問いに答えます。コーディング
エージェントがこのモデルを動かせるか、です。コーディングエージェントは、モデルに
ツールを呼ばせることで動きます。チャットでは上手に答えるモデルでも、実際のツール
一覧を渡すと、ツール呼び出しを行う代わりに散文で説明してしまうことがあります。
この検査は、このパソコンを通じて実際のリクエストをいくつか送り、返ってきた結果を
報告します。

```sh
waired models check-agent                  # このパソコンが提供しているモデル
waired models check-agent <model-id>       # 特定のモデル
waired models check-agent --json out.json  # 不具合報告用の完全な結果
```

約1分かかり、先にモデルのダウンロードが必要です。モデルが信頼できない場合は
0以外で終了するので、スクリプトの判定に使えます。

## <a id="waired-runtimes"></a>`waired runtimes`

モデルそのものではなく、モデルを読み込んで動かす推論エンジンを扱います。

```sh
waired runtimes ls
waired runtimes status
waired runtimes install [engine]    # ollamaまたはvllm。ハードウェアから自動で選ぶ
waired runtimes upgrade <engine>    # インストール済みの推論エンジンをこのビルドのバージョンにする
waired runtimes uninstall <engine>
waired runtimes refresh             # 推論エンジンとモデルの選択を評価し直す
waired runtimes benchmark           # このパソコンの実際の速度を計測する
```

**`benchmark`**は、このパソコンが動かしているモデルでスループットを計測します。
別のモデルのほうが適していれば切り替えを提案し、両方のモデルの名前と、どちらの
方向への提案かを表示します。[モデルを変更する](/ja/guides/choose-a-model/#switch-models)を
参照してください。

**`upgrade`**は、`waired update`が代わりに実行するものです。このパソコンにすでに
ある推論エンジンを変更し、推論エンジンのないパソコンでは何もしません。vLLMでは、
`upgrade`は入れ替えではなく再構築です。新しい環境は使用中の環境のとなりに構築
され、準備ができてから引き継ぐので、実行中も応答は止まりません。vLLMのバージョンを
動かすアップデートは約4GBをダウンロードし、5〜15分かかり、両方がディスクにある間は
約8GBの空きが必要です。

## <a id="waired-inference"></a>`waired inference`

```sh
waired inference on               # このパソコンでモデルを動かす
waired inference off
waired inference status

waired inference engine start     # 推論エンジンを始める
waired inference engine stop      # 止めて、保持しているメモリを解放する
waired inference engine status

waired inference memory status    # モデルの選択の根拠になるメモリの値
waired inference memory remeasure # その値を計測し直す

waired inference unload           # モデルのメモリを解放し、答え続ける
waired inference residency        # keep-alive：モデルを載せたままにする時間
waired inference residency 30m    # 変更する。"always"は載せたままにする
```

**`on`と`off`**は、このパソコンでモデルを動かすかどうかを決めます。オンにすると、
選んだモデルがまだなければダウンロードするので、最初の`on`には時間がかかることが
あります。オフにするとすべてディスクに残したまま、ローカルでの応答をやめます。
推論エンジンのないパソコンでは、`on`はそのことを表示し、推論エンジンをインストール
する`waired init`の実行を提案します。この設定は再起動後も保持され、バックグラウンド
サービスが応答していなくても使えます。Wairedが決めた場合は、`status`に理由が
表示されます。
[選んでいないのにローカル推論がオフで始まった](/ja/troubleshooting/setup/#local-inference-started-off-and-i-did-not-choose-that)を
参照してください。

**`unload`と`engine stop`**はどちらもメモリを戻しますが、同じではありません。
`unload`はモデルを解放して推論エンジンは動かしたままにするので、このパソコンは
答え続け、次のリクエストでモデルを読み込み直します。`engine stop`は推論エンジン
自体を止めるので、再び始めるまでここでは何も答えません。
[Wairedを一時停止する](/ja/guides/pause/)を参照してください。

**`residency`**はkeep-aliveです。最後のリクエストのあと、モデルをメモリに保持する
時間です。既定は`always`です。モデルを読み込み直すと、次のリクエストに約17秒から
約1分の待ち時間がかかるからです。引数なしで実行すると、現在の設定を表示します。

```text
Keep-alive: always (the model stays loaded).
```

`30m`や`8h`のような時間を渡すと設定します。`always`または`0`で、載せたままに
戻ります。設定を変えたときにモデルがメモリにあれば、変更はすぐに反映されます。
同じ設定は`agent.json`の`idle_timeout`、`WAIRED_INFERENCE_IDLE_TIMEOUT`、
`--inference-idle-timeout`、Wairedアプリの［Keep-alive］です。

推論エンジンが動いている間だけモデルを保持するパソコンでは、設定するタイマーが
なく、`residency`と`unload`はそのことを表示します。

```text
The inference engine on this computer holds the model for as long as the engine runs,
so there is no idle timeout to set here.
To free the memory, stop the engine: `waired inference engine stop`
```

**`memory status`**は、Wairedが最後に確認したときの空きメモリと、その時刻を表示
します。この値が、このパソコンでの「このモデルは収まるか」というすべての判断の
根拠です。Wairedはバックグラウンドサービスの起動のたびに、何かを読み込む前に
確認し、見た中でいちばん大きい値を保持します。**`memory remeasure`**は計測を
やり直し、大きくても小さくてもその値を採用します。推論エンジンが読み込まれて
いる間は拒否されます。その推論エンジンのメモリが差し引かれてしまうからです。先に
`waired inference engine stop`で止めてください。
