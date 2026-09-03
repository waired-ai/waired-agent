---
title: CLI コマンド
description: すべての waired コマンドを、やりたいことごとにまとめたリファレンス。重要なフラグと、何が表示されるか。
meta:
  audience: ターミナルで作業する人、画面のないマシンを扱う人
  needs: Waired がインストール済みであること
  time: 索引を眺めて、必要な節だけ読む
sourceHash: b01f4e1cc0f75379
---

このページの内容は、注記のあるもの以外すべて
[Waired アプリ](/ja/guides/waired-app/)からも行えます。全フラグは
`waired <コマンド> --help` で確認できます。このページは、そのフラグが**何のためにあるか**を扱います。

## 索引

| コマンド | 内容 |
|---|---|
| [`waired init`](#waired-init) | このパソコンをサインインさせ、セットアップする |
| [`waired status`](#waired-status) | ちゃんと動いている？ |
| [`waired doctor`](#waired-doctor) | 全体を検査し、多くをその場で修復する |
| [`waired auth status`](#waired-auth-status) | このパソコンのサインインはいつ切れる？ |
| [`waired logout`](#waired-logout) | このパソコンの識別情報を削除する |
| [`waired infer`](#waired-infer) | いますぐ自分のモデルに尋ねる |
| [`waired models`](#waired-models) | 何が入っているか、追加、どれを動かすかの選択、ダウンロードの中止、削除 |
| [`waired runtimes`](#waired-runtimes) | 推論エンジン本体と、ベンチマーク |
| [`waired inference`](#waired-inference) | ここでモデルを動かすかどうか、エンジンの起動・停止 |
| [`waired share`](#waired-share) | このパソコンを貸し出すかどうか |
| [`waired worker`](#waired-worker) | どのパソコンが答えるか |
| [`waired peers`](#waired-peers) / [`ping`](#waired-ping) | 自分のほかのパソコン |
| [`waired public`](#waired-public) | ほかの Waired ユーザーと空きマシンを貸し借りする |
| [`waired link`](#waired-link--unlink) / [`unlink`](#waired-link--unlink) | コーディングツールをつなぐ |
| [`waired claude`](#waired-claude) | Claude Code の実行先と、その場での切り替え |
| [`waired pause`](#waired-pause--resume) / [`resume`](#waired-pause--resume) | ルーティングの停止と再開 |
| [`waired update`](#waired-update) | 新しい Waired を入れる |
| [`waired config`](#waired-config) | 詳細ログの ON / OFF |
| [`waired logs`](#waired-logs) | バグ報告用に最近のログをファイルへ保存 |
| [`waired version`](#waired-version) | どのビルド？ |
| [`waired keygen`](#waired-keygen) | 鍵ペアを手動で生成する |

---

## セットアップとサインイン

### `waired init`

このパソコンをサインインさせ、セットアップします。1 台につき 1 回です。
通常はインストーラが実行してくれるので、自分で打つのは中断したセットアップの再開や、
`--no-init` でインストールしたマシンを設定するときだけです。

```sh
sudo waired init            # macOS / Linux
waired init                 # Windows は管理者ターミナルから
```

推論エンジンをインストールするため管理者権限が必要です。
**実行中はこのコマンド自身が、ブラウザのセットアップ画面が要求する作業を行っています**。
セットアップが終わるまでウィンドウを閉じないでください。
→ [サインインとセットアップ](/ja/getting-started/first-run/)

| フラグ | 使いどころ |
|---|---|
| `--mask-pii` | 出力中のホームフォルダ・ユーザー名・マシン名・アカウントのメールアドレスを伏せます。バグ報告に貼るとき用。ベストエフォート。 |
| `--non-interactive` | 何も聞かず既定値で進めます。スクリプト用。 |
| `--no-browser` | ブラウザを開かず、サインイン用リンクを表示します。SSH 用。 |
| `--inference-enabled=true\|false` | 「このパソコンでモデルを動かすか」に、聞かれずに答えます。 |
| `--skip-claude-route` | セットアップは行いつつ、Claude Code は Anthropic API のままにします。スキルやプラグインは入ります。あとから `waired claude enable` で切り替えられます。 |
| `--skip-integration` | コーディングツールの設定を丸ごと省きます（Claude Code も OpenCode も OpenClaw も変更しません）。 |
| `--device-name <name>` | このパソコンのホスト名ではなく、指定した名前を申告します。使われるのは最初にネットワークへ参加するときで、あとから名前を変えるのは [Web コンソール](/ja/guides/web-console/)です。`waired init` をもう一度実行しても、その変更は上書きされません。 |
| `--control <URL>` | 既定ではなく指定したコントロールプレーンでサインインします。→ [インストールの詳細オプション](/ja/reference/install-options/) |
| `--auth-key <key>` | ブラウザでのサインインの代わりに認証キーで参加します（サーバーやコンテナ向け）。`file:/path/to/key` も指定でき、フラグを省略すると `$WAIRED_AUTH_KEY` を読みます。キーは[管理コンソール](/ja/guides/web-console/)の **設定 → 認証キー** で作成します。→ [サインインとセットアップ](/ja/getting-started/first-run/#servers-and-containers-auth-keys) |
| `--force-reauth` | すでにサインイン済みのパソコンで、あらためてサインインし直します。これを付けない場合、`waired init` はセットアップの続きから進み、既存のサインインはそのままにします（`--auth-key` を渡した場合も、そのキーは使われません）。コーディングツールの質問をやり直すことはありませんが、ブラウザーで答えたのにこのパソコンにまだ書かれていない設定があれば、それは最後まで行います。 |

正式な一覧は `waired init --help` です。ここに載せていない開発者向け・CI 専用の
フラグもそちらに含まれます。

すでにサインイン済みのパソコンで実行し直しても安全です。最初からサインインし直すのでは
なく、セットアップの続きから進むため、何度実行してもかまいません。Waired が自分から
サインインし直すのは、既存のサインインが修復不能なほど期限切れになっている場合だけです。

スクリプト向けの **終了コード**:

| コード | 意味 |
|---|---|
| `0` | サインイン済みで、ローカル推論も動いている(または最初から使わない設定)。 |
| `3` | サインイン済みだが、この端末でローカル推論が動いていない — 推論エンジンをインストールできなかったか、起動しても動き続けなかった。サインイン自体は完了している。[セットアップで「推論エンジンが起動しなかった」と出た](/ja/troubleshooting/#setup-says-the-inference-engine-failed-to-start)を参照。 |
| `1` | セットアップが完了しなかった(サインイン自体が失敗)。 |
| `130` | Ctrl-C で中断した。 |

`3` を `1` と分けているのは意図的です。パソコンは実際にサインイン済みでネットワークにも
参加しており、サインインをやり直してもエンジンの状況は何も変わらないためです。

自分でエンジンのインストールを切った場合は、これに**該当しません**。`WAIRED_NO_OLLAMA`
が設定されたパソコン(`--skip-ollama` / `-SkipOllama` の実体)では、`waired init` は
エンジンを飛ばし、その旨を表示して `0` で終了します。異常は起きていないため、
エラーとしては扱いません。

モデルのダウンロードが終わっていない場合も該当しません。セットアップが待つ時間には
上限があり、それを過ぎると端末を返し、その旨を表示して `0` で終了します。転送は
バックグラウンドのサービスが続けます。数分後には利用者が何もしなくてもローカル推論が
使えるようになるため、スクリプトがこれをインストール失敗として扱ってはいけません。
進捗を報告するのは `waired status` です。

### `waired status`

「動いているか」を手早く確認します。

```sh
waired status
waired status --observability     # エンジン、モデル、自分のほかのパソコン
waired status --observability -o json
```

通常のデスクトップ用インストールでは状態がシステムの所有物なので、
`sudo` を付けて（Windows は管理者ターミナルで）実行するとすべて見えます。
権限がない場合は「システム全体で登録済み」とだけ報告して終了します — 推測はしません。

モデルを動かすパソコンでは、`Inference:` のブロックがエンジンの現在の様子を報告します。

```
Inference:
  state:          ready
  runtimes:       ollama 0.33.2 (ready, ctx 200k q8_0)
  model loaded:   ollama: qwen3:8b-q4_K_M (kept until unloaded)
  first token:    35.4s, 12 minutes ago (fastest seen here: 2.6s)
  models ready:   qwen3-8b-instruct
```

`model loaded:` は重みがメモリに載っているかどうかを示します。`first token:` は
直前の回答が始まるまでにかかった時間と、Waired を最後に起動してから同じモデルで
記録した最短の時間を並べて示します。役に立つのはこの2つの組み合わせです。
モデルが載っていても、プロンプト全体を読み直すことはあります — 上の2つの数字の差が
まさにそれです。

どちらも測定値であって判定ではありません。最初のトークンまでどれくらいなら良いのかは
モデルとマシンによって変わるので、数字だけを示して判断はお任せしています。
示すものが何も測れていないときは、この行は出ません。インストール直後や、
リクエストをすべて別のパソコンに任せているパソコンでは、それが通常の状態です。

### `waired doctor`

セットアップの各部分を検査し、項目ごとに ✓ / ⚠ / ✗ を表示して、
**f** を押せば直せるものを直します。詳細:
[状態を診断する](/ja/getting-started/doctor/)

```sh
waired doctor
waired doctor --fix              # 確認なしで修復（スクリプト・SSH）
```

### `waired auth status`

サインインの状態と期限を表示し、更新が必要なら `init` の再実行を促します。
サービス用インストールでは `status` と同様に管理者権限が必要です。

更新は最初に実行したのと同じ `waired init` です。このパソコンがすでにサインイン
済みであることを認識し、確認を取ったうえで、サインインだけを入れ替えます。
設定も推論エンジンも、ネットワーク上でのこのパソコンの位置づけもそのままで、
端末一覧でも同じ端末のままです。サインインを保持しているのはバックグラウンドの
サービスなので、更新には Waired がバックグラウンドで動いている必要があります。

### `waired logout`

このパソコンの識別情報と秘密情報を削除し、次の `waired init` が
新しい端末としてきれいに登録できるようにします。一時的な措置ではありません。
しばらく使わないだけなら [`pause`](#waired-pause--resume) を見てください。

---

## モデルと推論

### `waired infer`

プロンプトを 1 つ送って応答を表示します。経路全体が通っていることを確かめる最短の方法です。

```sh
waired infer "say hi"
waired infer "say hi" --explain    # 実際には尋ねず、どのマシンとモデルが答えるかを表示
```

`--explain` は、表示する数値の元になったピア情報の古さも `map_age_ms` として出します。
数値がおかしいのか、単に古いだけなのかは、これで見分けます。

他のパソコンが答えた場合、`--explain` は
[`waired peers list`](#waired-peers) と同じ呼び方でそのマシンを示します。名前と
`DEVICE-ID` の両方が出るので、どちらをそのまま `waired worker set --pin` に渡しても
構いません。パブリックマシンについては、Waired が表示している仮名だけが出ます。

理由の行は、このパソコン自身のエンジンがなぜ答えなかったのかを述べます。これは
「手元のモデルが使える状態かどうか」とは別の問いです。他のノードに pin していれば、
手元のモデルが使える状態であっても参照されません。pin が指すのはモデルではなく
パソコンだからです。

### `waired models`

```sh
waired models ls                  # ダウンロード済みのモデルと、動作中のモデル
waired models ls --detail         # カタログ全体と、このパソコンで動くかどうか
waired models pull <モデルID>      # ダウンロードする
waired models use <モデルID>       # このパソコンが動かすモデルにする
waired models cancel <モデルID>    # 実行中のダウンロードを止める
waired models rm <モデルID>        # 削除して数 GB 空ける
waired models refresh             # このマシンにもっと合うモデルはあるか
waired models check-agent         # コーディングエージェントで使えるモデルか
```

`ls` は各モデルがディスク上で占める容量を **SIZE** 列に表示します。`rm` で
どれだけ空くかはここで分かります。値は推論エンジンから取得するので、ダウンロード
済みでもエンジンが停止している場合は `-` になります（ゼロではなく「不明」です）。

`pull` はモデルが使える状態になるまで待ちます。ここで動きはするが Waired が
選ばないモデルは確認を求めます（スクリプトでは `--yes` で省略）。このパソコンの
メモリに載らないモデルは、不足量を示したうえで**再確認**を求めます
（既定は No）— ダウンロード完了後の読み込みに失敗する見込みだからです。この
再確認は `--yes` だけでは省略できません。本当に実行したいスクリプトは
`--yes --force` を渡します。`rm` も実行前に確認します。
モデル ID は[モデルカタログ](/ja/reference/model-catalog/)にあります。

`use` は、このパソコンが実際に動かすモデル——応答を返すモデル——を設定します。
`pull` が行うのは重みの取得だけで、ダウンロード済みでも使用中とは限りません。
切り替えに再起動は要りません。新しいモデルが使えるようになるまで、いま動いている
モデルが応答を返し続けます。重みがまだディスクに無ければ `use` がそのダウンロード
を開始し、その旨を伝えます。

```
waired models use qwen3.5-4b
qwen3.5-4b will run on this computer once it finishes downloading.
The current model keeps answering until then.
```

コマンドはデーモンが選択を受け付けた時点で戻ります。`--wait` を付けると新しい
モデルが実際に応答できるようになるまで待つので、切り替えの完了を待ってから先へ
進むスクリプトで使えます。過剰スペックの確認とメモリに載らない場合の再確認は
`pull` とまったく同じに働きます（`--yes` と `--yes --force` を含む）。

`cancel` は実行中のダウンロードを止めます。数 GB の `pull` を誤って始めたときの
抜け道です。事前確認はしません — いま取得中のものを止めるだけで、それは直前に
「要らない」と言った当のものだからです。止めたジョブを表示します。

```
cancelled download: model=qwen3.5-9b job=job_a761d6a4ca1a
```

何もダウンロードしていなければ、そう伝えて終わります。

```
no download in progress for qwen3.5-9b
```

そのダウンロードを待っていた `pull` も終了し、2 つの結末のどちらだったかを表示
します。

```
qwen3.5-4b: downloading…
qwen3.5-4b: download stopped before it finished
```

終了コードは 0 以外になるので、ダウンロードを待っていたスクリプトが、完了したもの
として先へ進むことはありません。

途中まで取得したデータはディスクに残るので、同じモデルを再度 `pull` すると
最初からではなく途中から再開します。この分を空けるには、ダウンロードを完了させて
から `rm` してください — 完了しなかったダウンロードが残したデータは、まだ `rm`
が名前で指せません。

`cancel` は `use` を取り消しません。そのモデルをすでに選んでいた場合、選択はその
まま残り、重みが揃った時点で適用されます。`models ls --detail` では
`→ preferred (switching)` ではなく `◦ preferred (needs downloading)` と表示され
ます — 何も取得していないためです。`use` で別のモデルを選べば置き換わります。

この表は自分の下に凡例を出します。記号をそのまま書けない場所 — UTF-8 になっていない
Windows コンソール、Windows でファイルにリダイレクトした出力、ロケールが UTF-8 でない
端末 — では ASCII で出ます（`●` は `*`、`→` は `->`、`◦` は `o`、`↓` は `v`、`⋯` は
`...`）。凡例そのものも同じように置き換わります。

削除の前に `cancel` する必要はありません。`rm` はそのモデルのダウンロードを先に
止め、止めたことを表示します。

`check-agent` は他のコマンドとは別の問いに答えます。「このパソコンで動くか」でも
「速度は足りるか」でもなく、「コーディングエージェントがこのモデルを動かせるか」です。

コーディングエージェントは、モデルにツールを呼ばせて動きます（このファイルを読む、
これを検索する、など）。チャットでは見事に答えるのに、実際のツール一覧を渡すと
ツール呼び出しを「実行せずに文章で説明する」モデルや、渡していないツールを
要求するモデルがあります。そうなると、エージェントが生の JSON を延々と表示したり、
作業を宣言したまま何もしなかったりします。パソコン側は壊れていません。モデルが
形式に従えないだけです。

このチェックは実際のリクエストをこのパソコン経由で何度か送り、結果を報告します。

```sh
waired models check-agent                  # このパソコンが動かしているモデル
waired models check-agent <モデルID>        # 特定のモデル
waired models check-agent --json out.json  # 詳細な結果（不具合報告用）
```

所要時間は 1 分ほどで、対象モデルのダウンロードが先に必要です。モデルが信頼できない
場合は終了コードが 0 以外になるので、スクリプトの判定に使えます。チェック自体が
実行できなかった場合（モデル未ダウンロード、サービス停止など）は、モデルのせいに
せずその旨を区別して表示します。

### `waired runtimes`

モデルそのものではなく、モデルを読み込んで動かす **推論エンジン**の側です。

```sh
waired runtimes ls
waired runtimes status
waired runtimes install [エンジン]
waired runtimes upgrade <エンジン>   # 入っているエンジンをこの版が使うバージョンに揃える
waired runtimes uninstall <エンジン>
waired runtimes benchmark         # このパソコンの実際の速度を測る
```

注目すべきは `benchmark` です。実測のスループットを計測し、
別のモデルのほうが合っている場合は切り替えを提案し、
両方のモデル名とどちら向きの切り替えかを示すので、速さと質を見比べて選べます。

`upgrade` は `waired update` が自動で実行するものです。`install` との違いを
押さえておくと役に立ちます。`upgrade` はこのパソコンにすでにあるエンジンを
入れ替えるだけで、エンジンが無いパソコンでは何もしません。

vLLM の `upgrade` は入れ替えではなく作り直しです。使用中の環境の隣に新しい
環境を作り、完成してから切り替わるので、実行中も応答は止まりません。ただし
vLLM のバージョンが変わる更新では約 4 GB をダウンロードし、5〜15 分ほどかかり
ます。両方がディスク上にある間は 8 GB ほどの空きが必要で、古いほうは終わってから
削除されます。vLLM を入れていないパソコンには何も起きません。

### `waired inference`

```sh
waired inference on               # このパソコンでモデルを動かす
waired inference off
waired inference status

waired inference engine start     # 推論エンジンを起動する
waired inference engine stop      # 推論エンジンを止めて、確保しているメモリを解放する
waired inference engine status

waired inference memory status    # モデル選択の基準になっているメモリ計測値
waired inference memory remeasure # その計測をやり直す

waired inference unload           # モデルのメモリを解放し、応答は続ける
waired inference residency        # keep-alive（モデルをメモリに残す時間）を表示
waired inference residency 30m    # ...変更する（"always" で保持し続ける）
```

`on` / `off` は、このパソコンでモデルを動かすかどうかそのものです。**オン**に
すると、選ばれたモデルがまだ無ければ取得するため、最初の `on` には時間が
かかることがあります。**オフ**にしてもディスク上のものはそのまま残り、
ローカルでの応答だけを止めます。推論エンジン自体が入っていないパソコンでは、
`on` はその旨を伝えて [`waired init`](#waired-init) の実行を提案します。
エンジンを導入するのは `init` です。このパソコンでモデルを動かすかどうかを
尋ねるのが `init` だからです。設定は再起動をまたいで保持され、
バックグラウンドサービスが応答しない状態でも保存され、次回起動時に適用されます。

この設定が**オフ**の状態から始まるマシンは 1 種類です。リクエストに
現実的な時間で答えられないと計測されたマシンです。Waired が判断した場合は
`status` がその理由を表示します。
→ [選んでいないのにローカル推論がオフで始まった](/ja/troubleshooting/#local-inference-started-off-and-i-did-not-choose-that)

メモリの少ないマシンはこの対象ではなくなりました。載せられる最大のモデルが
選ばれ、それが非常に小さいモデルになることはあります。
→ [非常に小さいモデルが選ばれた](/ja/troubleshooting/#waired-chose-a-very-small-model-for-my-machine)

`unload` と `engine stop` はどちらもメモリを返しますが、別のものです。`unload` は
モデルだけを解放してエンジンは動かしたままにするので、このパソコンは応答を続けます
（次の質問でモデルを読み直すため、その 1 回だけ時間がかかります）。`engine stop` は
エンジン自体を止めるので、再び起動するまでこのパソコンでは何も答えません。しばらく
別のことにメモリを使いたいときは `unload`、このパソコンを完全に外したいときは
`engine stop` です。ほかのマシンからの利用を止めるのは
[`waired share off`](#waired-share) です。
→ [しばらく使わないようにする](/ja/guides/pause/)

**Waired は一度読み込んだモデルをメモリに保持し、質問が無い時間が続いても降ろしません。**
これは意図的なものです。読み直すには、マシンとモデルによって約 17 秒から 1 分ほど、
答えの最初のトークンが出るまでに余分にかかり、その大半は裏で読み直しておいても
取り戻せないためです。

これを変えるのが `residency` です。引数なしでは、現在の設定を表示します。

```text
Keep-alive: always (the model stays loaded).
```

引数に時間を渡すと設定します（`waired inference residency 30m`、`8h` など）。
`always`（または `0`）で「保持し続ける」に戻ります（既定）。

設定を変えたときにモデルがメモリにあれば、読み直しなしでそのモデルにすぐ適用され
ます。何も読み込まれていなければ、次に読み込まれるモデルに新しい設定が効くよう、
Waired は推論エンジンを再起動します。失うモデルが無いので、これには何の代償も
ありません。どちらの場合も設定は保存され、再起動をまたいで残ります。同じ設定は
`agent.json` の `idle_timeout`、`WAIRED_INFERENCE_IDLE_TIMEOUT`、
`--inference-idle-timeout` でも指定でき、Waired アプリの
**Inference → Keep-alive** からも選べます。

パソコンによっては、推論エンジンが動いている間ずっとモデルを保持し、設定できる
時間そのものが存在しません。`residency` と `unload` は、その場合に取り繕わず
こう答えます。

```text
The inference engine on this computer holds the model for as long as the engine runs,
so there is no idle timeout to set here.
To free the memory, stop the engine: `waired inference engine stop`
```

（このパソコンの推論エンジンは、エンジンが動いている間ずっとモデルを保持します。
ここで設定できる時間はありません。メモリを解放するにはエンジンを止めてください。）

Waired アプリも同じ理由で、そのパソコンでは **Keep-alive** と
**Unload model** を出しません。メモリを取り戻す手段は
`waired inference engine stop` で、そこではモデルの分だけでなくエンジンが
確保している分もまとめて返ります。

`memory status` は、Waired が最後に計測したときに空いていたメモリ量と、その
時刻を表示します。このパソコンでの「このモデルが載るか」の判断は、すべて
**現在の空き容量ではなくこの値**を基準にしています。計測はバックグラウンド
サービスが起動するたび、何かを読み込む前に行われ、**これまでに見た中で最大の
値**が残ります。大きな処理が動いている最中に計測してしまっても、その低い値は
捨てられるので、以後のモデル選択に引き継がれることはありません。

`memory remeasure` は計測をやり直し、その結果を——大きくても小さくても——
現在の値にします。恒常的に使えるメモリが減ったマシンで、値を**下げる**ための
手段です。推論エンジンが読み込まれている間は、そのエンジンのメモリをマシン側に
計上してしまうため実行を拒否します。先に `waired inference engine stop` で
停止してください。

### `waired share`

このパソコンをそもそも貸し出すかどうか — コンピューター側に残る唯一の
共有スイッチです。

```sh
waired share on
waired share off        # すべての提供を止め、実行中の処理も打ち切る
waired share status
```

オフにすると、あらゆる提供が即座に止まります。自分のほかのパソコンへの応答も、
アカウント外からの利用も打ち切られ、その瞬間に実行中だったリクエストは完了
しません。このパソコン上での自分の利用には影響しません。ウェブコンソールから
このスイッチをオンに戻すことはできません — このコマンドか、
[Waired アプリ](/ja/guides/waired-app/)の **Share this computer** だけです。

スイッチがオンのとき*誰に*提供するか — 自分のほかのパソコン、アカウント外の
人 — は、ここではなく[ウェブコンソール](/ja/guides/web-console/)で設定します。
`status` は全体を 1 コマンドで答えます:

```text
Sharing this computer: on
Your other computers: on
People outside your account: off
Who this computer is shared with is set in the Waired console.
```

先頭の行がこのパソコン自身のスイッチです。保存された選択と実際の状態が
違うときは 2 行目が説明します: アプリを終了したあとは `Paused because the
Waired app is not running. It resumes when the app starts.`、保存値がまだ
適用されていないときは `Saved choice: …`。続く 2 行はコンソールの決定で、
まだ届いていない間は `not known yet` と出ます。コンソールでゲスト数の上限が
設定されていれば `Guest limit: N at once` の行が付きます。

### `waired worker`

**このパソコン**のリクエストの行き先です。

```sh
waired worker get
waired worker set --mode=auto            # 自前の AI があればそれ、なければ他（既定）
waired worker set --mode=local-only      # ほかのパソコンは使わない
waired worker set --mode=peer-preferred  # ほかのパソコンを優先し、駄目ならここで動かす
waired worker set --mode=peer-only       # ほかのパソコンだけ。駄目ならここで動かさずエラー
waired worker set --pin=<peer>           # 常にこの 1 台（--mode=pinned になる）

waired worker set --prefer=speed         # いちばん速く答えられるパソコンへ（既定）
waired worker set --prefer=size          # いちばん大きいモデルを動かしているパソコンへ
waired worker set --min-model-size=medium  # これより小さいモデルのパソコンは使わない
waired worker set --min-model-size=""      # 下限なし（既定）
```

`waired worker get` は全部まとめて出します。

```
mode:           auto
prefer:         speed
smallest model: any
```

`<peer>` には、パソコンの名前か、`waired peers list` の `DEVICE-ID` 列にある
識別子を渡します。名前は各パソコンのホスト名が既定で、[Web
コンソール](/ja/guides/web-console/)から変更できます。

ここで選んでいるのは**パソコン**であって、モデルではありません。答えを返した
パソコンが動かしているモデルが、そのまま答えになります。両者が一致している必要は
なく、自前の AI を持たないノートパソコンから、AI を持っているマシンへ全部の
リクエストを送る、という使い方ができます。`--model` でモデルを指定すれば、その
モデルを持っているパソコンから、そのモデルの答えが返ります。パソコンを pin した
うえで、そのパソコンが動かしていないモデルを指定した場合は、**pin したパソコンが
優先されます** — あなたが選んだのはそのパソコンだからです。どのパソコンがどの
モデルで答えたかは `waired infer --explain` で確認できます。

pin したパソコンの電源が入っていない、または到達できないときは、黙ってほかへ
回さずにエラーになります。そのマシンを指定したのはあなたなので、使えないことを
Waired が伝えます。

#### 答えられるパソコンが複数あるとき

どれに回すかを `--prefer` で決めます。

既定の `speed` は、いちばん早く答えが返ってくるパソコンに回します。Waired は
各パソコンがプロンプトを読む速さを測っていて、**どのマシンでも同じ固定の
プロンプト長で測る**ので数値を比べられます。そのときどれだけ混んでいるかも
考慮します。

`size` は、代わりにいちばん大きいモデルを動かしているパソコンに回します。大きい
ほうが良いとは限らず、しばしばずっと遅くなります。実際の構成では、いちばん大きい
モデルを持つマシンが 9 分かけて返したターンを、別のマシンが 43 秒で返しました。

まだ測っていないパソコンは不利に扱われません。いちばん速いパソコンと同じ扱いに
なるので、ターンが回ってきて、そこで測られます。

#### 最小のモデルサイズを決める

`--min-model-size` は、指定したサイズより小さいモデルを動かしているパソコンを
使いません。サイズは `small` / `medium` / `large` の 3 つで、
[`waired public use`](#waired-public) と同じ語です。どのクラスのグラフィック
ボードで動くモデルかを表すもので、そのマシンの性能ではありません。

これは**除外**であって、後回しにするのではありません。このパソコン自身のモデルも
含めて下限を満たすものが 1 つも無ければ、そのリクエストは失敗します。エラーは
壊れたマシンではなく、この設定を名指しします。Claude Code ではこう出ます。

```
API Error: 400 No computer on Waired runs a medium model or larger. Change the floor with `waired worker set --min-model-size`. Pick an Anthropic model in /model to send this turn to the cloud, or run `waired doctor` to see what is missing.
```

既定は下限なしです。

どちらの設定も Waired アプリの **Inference routing** にあります。

### `waired peers`

```sh
waired peers list
```

自分のほかのパソコンと、それぞれのアドレス・エンジン・GPU・モデル。
`worker set --pin` に渡す名前はここで調べます。同じ名前を申告するパソコンが 2 台
あると、2 台目には番号が付きます。一覧に並ぶ名前は必ず一意です。

**MODEL** はそのパソコンで動いているモデル。隣の **MODELS** は同じモデルを AI
ソフトウェア側の名前で表したもので、Ollama と vLLM では異なります。

**WORKER-CAPABLE** は、そのパソコン自身の申告です。いま応答できると言っているか
どうか、できないと言っている場合はその理由も出ます (モデルを取得中なら
`no (downloading)`、そのパソコンの推論エンジンが自分に応答しなかったなら
`no (engine not answering)` など)。この申告は Waired アカウント経由で届くもので、
パソコン同士のプライベートネットワークを通ってはいません。つまり `yes` は申告で
あって、このパソコンが確かめた結果ではありません。

`no (stale)` はそのパソコンが報告を寄こさなくなった状態です。どれくらい古い報告が
stale 扱いになるかは表の下に出るので、推測する必要はありません。電源が切られている
パソコンも、ネットワークから削除するまで行は残ります — この一覧は「いま起きている
パソコン」ではなく「ネットワークに属しているパソコン」だからです。

一覧のうち、このパソコンが返事をもらえていない相手がいるときは、表の下に次の行が
出ます。

```
This computer has had no reply from: office-desktop.
WORKER-CAPABLE is what each computer reports about itself, not something this
computer checked. Run `waired doctor` to measure this computer's connection.
```

1 台からも返事をもらえていない場合、1 行目は `This computer has had no reply from
any computer listed above.` になります。たいていは向こう側ではなくこちら側の問題
です。この注記はあくまで手掛かりであって断定ではありません。返事が来ていれば接続が
生きている証拠になりますが、来ていないだけなら単に電源が切られているだけのことも
あります。実際に測るのは `waired doctor` です。

原因が 1 つだけ名指しされることがあります。これが直るまで他は何も動かないためです。

```
This computer's key does not match the one your network has for it, so no other
computer can reach it. Run `waired init` to register this device again.
```

`waired worker get` は、指定したパソコンについて同じ 2 つを報告します。`model:`
行と、応答できないときに理由を書く `status:` 行です。

### `waired ping`

```sh
waired ping <peer>
```

このパソコンから、プライベートネットワーク越しに別のマシンへ実際に届くかを確認します。

相手が応答しないときは、エラーにそのピアの名前が入ります。応答しないピアと、
このマシン側の Waired の問題とを読み分けられます。

### `waired public`

空いている処理能力をほかの Waired ユーザーに貸し、また借ります。
自分でオンにしない限りオフです。**先に[パブリック共有](/ja/public-share/)を読んでください** —
公開マシンの持ち主は、あなたが送った内容を読めます。

このパソコンを公開で共有するかどうかのオン/オフは、ここではなく
[ウェブコンソール](/ja/guides/web-console/)で切り替えます — `waired public
status` はその状態と、他人のマシンを使う側の自分の設定をあわせて報告します。

```sh
waired public status
waired public use                      # いまの設定を表示
waired public use --auto               # 自分のより速いときは他人のマシンを使う
waired public use --explicit           # 明示したときだけ使う
waired public use --off
waired public use --min-model-size small|medium|large   # このサイズ以上のモデルを動かすマシンだけ
waired public use --main on|off --sub on|off
```

`use` を最初に有効にするとき、ターミナルに一度だけプライバシー警告が表示され、
読んで承諾する必要があります。

`waired public status` は共有側から始まります: `Sharing this computer
publicly: on|off`（コンソールからまだ届いていない間は `not known yet`）、
続いて `Guest limit: N at once` または `Guest limit: automatic`、そして
公開共有は Waired コンソールで切り替える旨の 1 行です。このパソコン自身の
スイッチがオフのときは、何も共有されていない理由を説明する行が加わります:

```text
Sharing is off on this computer, so nothing is shared. Turn it back on with `waired share on`.
```

---

## コーディングツール

### `waired link` / `unlink`

```sh
waired link                  # 見つかったすべてのコーディングツールを設定
waired link claude-code
waired link opencode
waired link openclaw
waired unlink <エージェント>
```

`link` は、ほかのツールが必要とする鍵も作成します
（→ [チャットアプリから使う](/ja/guides/chat-clients/)）。
`unlink` は `link` が追加したものだけを取り消し、それ以外には触れません。
`link` が既存の設定ファイルを変更した場合（該当するのは OpenClaw だけです）、
変更前に取ったバックアップは残り、`unlink` がその場所を表示します。

### `waired claude`

```sh
waired claude status
sudo waired claude enable     # Claude Code を自分のモデルに向ける（init も行います）
sudo waired claude disable
```

`enable` / `disable` には管理者権限が必要です。認証情報は一切書き込まないので、
claude.ai のサブスクリプションには影響しません。

`enable` が書くのはマシン全体の設定だけで、自分の `~/.claude/settings.json` には
何も書きません。既定モデルは設定しないので、何も触っていないセッションは
Claude Code 自身の既定 — Anthropic のモデル — で始まり、`/model` で Waired の項目を
選んだときに自分のコンピュータへ移ります。以前の Waired が書いた既定はそのまま
残し、`disable` がそれを消します（Waired が書いたものだからです）。

ターンの実行先は `/model` で選んだモデルだけが決めます。Waired の項目なら自分の
コンピュータ、Anthropic のモデルなら本来の Anthropic API です。Waired がターンを
勝手に反対側へ移すことはありません。自分のどのコンピュータも答えられない Waired の
ターンは Claude Code の中で理由付きで失敗し、Anthropic のターンのエラーは
そのまま中継されます。マシン全体のルートを設定していた `waired claude route` は
無くなりました。実行するとその旨を表示します。

```
`waired claude route` was removed: a turn runs where its model says, so choose in Claude Code's /model — a Waired entry to run it on your computers, an Anthropic model to run it on your Claude subscription. `waired claude status` shows what the last turn did
```

Waired のターンに*どのマシン*が応答するかは [`waired worker`](#waired-worker)
側の話で、これではありません。

セッションのモデルが何であれ本来の Anthropic API へ送られるリクエストが 1 種類
あります。Claude Code の auto モードがツール呼び出しごとに実行する安全性チェック —
実行してよいかを判定する分類器（classifier）です。このモデルは Claude Code 自身が
選ぶため、Waired が許可の判定を肩代わりすることはできません。Anthropic に到達
できないときはこのチェックが失敗します。自分のモデルが答えることはありません。

`status` は、マシン全体の設定の状態と、新しいセッションがどのモデルで始まり
どこへ送られるかを示す `default model:` の行を表示します。一度でもターンを見て
いれば、直近のターンが運んだモデル ID、その ID がどちら側に送ったか、その時刻を
示す `last request:` の行も出ます。

```
last request:       claude-waired-auto → Waired   (2 minutes ago)
```

```sh
waired claude statusline install [--wrap]
waired claude statusline remove
```

セッションのターンがどこで動くかと、自分のハードウェアが応答した場合はそのモデル名を
示すフッター行を管理します。`enable` が自動で入れるので通常は不要です。`--wrap` は
既存のステータス行を置き換えずに包みます。包むスクリプトは Windows では PowerShell、
それ以外ではシェルスクリプトです（Claude Code がその OS で起動できる形に合わせて
います）。`waired claude disable` は元の行を復元し、これを削除します。

`waired claude status` は、ステータス行と `/model` の項目を更新するフックが別の OS の
シェル向けに書かれている場合 — このバージョンより前の Waired でセットアップした
Windows がこれにあたります — `installed, but not in the form this computer runs`
と表示します。`sudo waired claude enable`（Windows は管理者プロンプトから）で
書き直せます。

---

## ルーティング、アップデート、その他

### `waired pause` / `resume`

```sh
waired pause
waired resume
```

一時停止は**すべて**を止めます。ツールはクラウドに戻り、自分のモデルも応答しなくなります。
再起動をまたいで保持されます。「オフにする」の 4 通りの意味については
[しばらく使わないようにする](/ja/guides/pause/)を参照してください。

### `waired update`

```sh
waired update              # 現在のチャンネルのまま確認して適用
waired update --check      # 確認のみ
waired update --yes        # インストーラの確認を省いて適用
waired update --edge       # 最新の main ビルドへ切り替え
waired update --stable     # stable へ戻す
waired update --force      # キャッシュではなく取得元から確認し直す（Linux: パッケージインデックスを更新するため sudo を求める）
waired update --notify on|off   # アプリのアップデート通知ポップアップ
```

→ [Waired を更新する](/ja/getting-started/update/)。`--notify off` はポップアップだけを止め、
Waired アプリのメニュー内の項目はどちらでも残ります。

### `waired config`

保存される設定を変更します。今は**ログの詳細レベル**が対象です。

```sh
waired config log-level              # 現在のレベルを表示
waired config log-level debug        # 詳細ログを ON
waired config log-level info         # 通常へ戻す
```

レベルは `debug` / `info`（既定）/ `warn` / `error` の 4 つです。`debug` は問題を
再現する前に入れておく切り替えで、**再起動なし**でバックグラウンドサービスと
Waired アプリの両方に即時反映され、再起動後も保持されます。ON の間は保存量も
増え、ファイル 1 つあたり 128 MB（通常は 32 MB、古い控え 10 世代はどちらも同じ）に
なるので、数日後に気づいた問題でも記録が残っています。終わったら `info` に
戻してログを小さく保ってください。サービスが起動していない場合は保存され、次回の
起動時に反映されます。

### `waired logs`

最近のログを 1 つのファイルにまとめ、バグ報告に添付できるようにします。

```sh
waired logs                          # ここに waired-logs-<時刻>.txt を出力
waired logs -o report.txt            # 出力先を指定
waired logs --since 30m              # さかのぼる範囲（既定 1h）
waired logs --mask-pii               # ホームディレクトリ / ユーザー名 / ホスト名 / メールを伏せる
waired logs --full                   # 直近 16 MB ではなくローテート済みの全世代を集める
```

バックグラウンドサービスのログ（システムログから）、その OS でサービス自身が
持つログファイル、そして推論エンジンのログを集めます。2 番目は macOS では
`/Library/Logs`（およびアプリの `~/Library/Logs`）、Windows では状態フォルダ配下の
`logs\waired-agent.log` で、警告より下のものはすべてここに書かれます。ローテーション
済みの古い分も含めて回収するので、最後のローテーションより前に始まった問題も報告に
残ります。回収は新しい順に合計 16 MB までで、issue に添付できる大きさに収まります。
`--full` を付けるとローテート済みの全世代を集めますが、`debug` では数百 MB に
なることがあります。
最も役立つ報告にするには、まず詳細ログを ON にし、問題を再現してから集めてください。

```sh
waired config log-level debug
# ...問題を再現...
waired logs --mask-pii -o report.txt
waired config log-level info
```

`--mask-pii` は、ホームフォルダ・ユーザー名・マシン名・アカウントのメールアドレスを
プレースホルダに置き換えます（`waired init --mask-pii` と同じマスク。`WAIRED_PII_MASK=1`
のときは既定で ON）。ベストエフォートなので、いずれにせよ共有前にファイルの中身を
確認してください。ほかのローカルパスが残ることがあります。

インストール中に問題が起きた場合の対処や、ほかに添付すべきものを含めた全体の流れは
[不具合を報告する](/ja/getting-started/report-a-problem/)にあります。

### `waired version`

```sh
waired version
waired version --json      # {version, buildSHA, os, arch}
```

### `waired keygen`

WireGuard の鍵ペアを生成します。`init` が自動で行うので、
手で実行するのは特殊なことをするときだけです。

---

## ほとんどのコマンドで使えるフラグ

| フラグ | 意味 |
|---|---|
| `--mgmt <url>` | 常駐サービスの待ち受け先（既定 `http://127.0.0.1:9476`）。 |
| `--gateway <url>` | `waired infer` 用の、自分のモデルが応答するアドレス（既定 `http://127.0.0.1:9473`）。 |
| `--state-dir <dir>` | 識別情報と秘密情報の保存先。環境変数 `WAIRED_STATE_DIR` でも指定できます。 |

<a id="sharing-vs-pausing"></a>

## 混同されやすい 2 つの操作

- **`pause` / `resume`** は*すべて*を止めます。メッシュのルーティングも、
  ローカルの AI も応答しなくなります。このパソコンを完全に外したいときに使います。
- **`inference on` / `off`** は、このパソコンでモデルを動かすかどうかを決めます。
  オフでも、ほかのパソコンの AI は使えます。
- **`share on` / `off`** は、*自分以外の誰か* — 自分のほかのパソコンも、公開の
  ゲストも — がこのマシンの AI を使えるかどうかだけを制御します。共有オフでも、
  ここでは `waired infer` が動きます。共有オンのとき誰に提供するかは
  [ウェブコンソール](/ja/guides/web-console/)で設定します。

個人用のワークステーションなら共有は**オフ**のまま一時停止もしない、
GPU 専用機なら共有を**オン**にしてノートパソコンから使えるようにする、という使い分けになります。
