---
title: CLI コマンド
description: すべての waired コマンドを、やりたいことごとにまとめたリファレンス。重要なフラグと、何が表示されるか。
meta:
  audience: ターミナルで作業する人、画面のないマシンを扱う人
  needs: Waired がインストール済みであること
  time: 索引を眺めて、必要な節だけ読む
sourceHash: 3436f5e32119d40a
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
| [`waired infer`](#waired-infer) | いますぐ自分の AI に尋ねる |
| [`waired models`](#waired-models) | 何が入っているか、追加、ダウンロードの中止、削除 |
| [`waired runtimes`](#waired-runtimes) | AI ソフトウェア本体と、速度テスト |
| [`waired inference`](#waired-inference) | ここで AI モデルを動かすかどうか、エンジンの起動・停止、自分のほかのパソコンへの提供 |
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

AI ソフトウェアをインストールするため管理者権限が必要です。
**実行中はこのコマンド自身が、ブラウザのセットアップ画面が要求する作業を行っています**。
セットアップが終わるまでウィンドウを閉じないでください。
→ [サインインとセットアップ](/ja/getting-started/first-run/)

| フラグ | 使いどころ |
|---|---|
| `--mask-pii` | 出力中のホームフォルダ・ユーザー名・マシン名・アカウントのメールアドレスを伏せます。バグ報告に貼るとき用。ベストエフォート。 |
| `--non-interactive` | 何も聞かず既定値で進めます。スクリプト用。 |
| `--no-browser` | ブラウザを開かず、サインイン用リンクを表示します。SSH 用。 |
| `--inference-enabled=true\|false` | 「このパソコンで AI を動かすか」に、聞かれずに答えます。 |
| `--share-with-mesh=true\|false` | 「ほかの端末に使わせるか」に、聞かれずに答えます。 |
| `--skip-claude-route` | セットアップは行いつつ、Claude Code は Anthropic API のままにします。スキルやプラグインは入ります。あとから `waired claude enable` で切り替えられます。 |
| `--skip-integration` | コーディングツールの設定を丸ごと省きます（Claude Code も OpenClaw も変更しません）。 |
| `--device-name <name>` | ホスト名ではなく、指定した名前でこのパソコンを登録します。 |
| `--control <URL>` | 既定ではなく指定したコントロールプレーンでサインインします。→ [インストールの詳細オプション](/ja/reference/install-options/) |
| `--auth-key <key>` | ブラウザでのサインインの代わりに認証キーで参加します（サーバーやコンテナ向け）。`file:/path/to/key` も指定でき、フラグを省略すると `$WAIRED_AUTH_KEY` を読みます。キーは[管理コンソール](/ja/guides/web-console/)の **設定 → 認証キー** で作成します。→ [サインインとセットアップ](/ja/getting-started/first-run/#servers-and-containers-auth-keys) |
| `--force-reauth` | すでにサインイン済みのパソコンで、あらためてサインインし直します。これを付けない場合、`waired init` はセットアップの続きから進み、既存のサインインはそのままにします（`--auth-key` を渡した場合も、そのキーは使われません）。 |

正式な一覧は `waired init --help` です。ここに載せていない開発者向け・CI 専用の
フラグもそちらに含まれます。

すでにサインイン済みのパソコンで実行し直しても安全です。最初からサインインし直すのでは
なく、セットアップの続きから進むため、何度実行してもかまいません。Waired が自分から
サインインし直すのは、既存のサインインが修復不能なほど期限切れになっている場合だけです。

スクリプト向けの **終了コード**:

| コード | 意味 |
|---|---|
| `0` | サインイン済みで、ローカル AI も動いている(または最初から使わない設定)。 |
| `3` | サインイン済みだが、この端末でローカル AI が動いていない — AI エンジンをインストールできなかったか、起動しても動き続けなかった。サインイン自体は完了している。[セットアップで「AI エンジンが起動しなかった」と出た](/ja/troubleshooting/#setup-says-the-ai-engine-failed-to-start)を参照。 |
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
バックグラウンドのサービスが続けます。数分後には利用者が何もしなくてもローカル AI が
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
設定も AI ソフトウェアも、ネットワーク上でのこのパソコンの位置づけもそのままで、
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

### `waired models`

```sh
waired models ls                  # ダウンロード済みのモデルと、動作中のモデル
waired models ls --detail         # カタログ全体と、このパソコンで動くかどうか
waired models pull <モデルID>      # ダウンロードする
waired models cancel <モデルID>    # 実行中のダウンロードを止める
waired models rm <モデルID>        # 削除して数 GB 空ける
waired models refresh             # このマシンにもっと合うモデルはあるか
waired models check-agent         # コーディングエージェントで使えるモデルか
```

`ls` は各モデルがディスク上で占める容量を **SIZE** 列に表示します。`rm` で
どれだけ空くかはここで分かります。値は AI エンジンから取得するので、ダウンロード
済みでもエンジンが停止している場合は `-` になります（ゼロではなく「不明」です）。

`pull` はモデルが使える状態になるまで待ちます。ここで動きはするが Waired が
選ばないモデルは確認を求めます（スクリプトでは `--yes` で省略）。このパソコンの
メモリに載らないモデルは、不足量を示したうえで**再確認**を求めます
（既定は No）— ダウンロード完了後の読み込みに失敗する見込みだからです。この
再確認は `--yes` だけでは省略できません。本当に実行したいスクリプトは
`--yes --force` を渡します。`rm` も実行前に確認します。
モデル ID は[モデルカタログ](/ja/reference/model-catalog/)にあります。

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

途中まで取得したデータはディスクに残るので、同じモデルを再度 `pull` すると
最初からではなく途中から再開します。この分を空けるには、ダウンロードを完了させて
から `rm` してください — 完了しなかったダウンロードが残したデータは、まだ `rm`
が名前で指せません。

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

モデルそのものではなく、モデルを読み込んで動かす **AI ソフトウェア**の側です。

```sh
waired runtimes ls
waired runtimes status
waired runtimes install [エンジン]
waired runtimes uninstall <エンジン>
waired runtimes benchmark         # このパソコンの実際の速度を測る
```

注目すべきは `benchmark` です。実測のスループットを計測し、
別のモデルのほうが合っている場合は切り替えを提案し、
両方のモデル名とどちら向きの切り替えかを示すので、速さと質を見比べて選べます。

### `waired inference`

```sh
waired inference on               # このパソコンで AI モデルを動かす
waired inference off
waired inference status

waired inference engine start     # モデルを読み込む
waired inference engine stop      # 確保しているメモリを解放する
waired inference engine status

waired inference share on         # 自分のほかのパソコンに、このマシンの AI を使わせる
waired inference share off
waired inference share status

waired inference memory status    # モデル選択の基準になっているメモリ計測値
waired inference memory remeasure # その計測をやり直す
```

`on` / `off` は、このパソコンでモデルを動かすかどうかそのものです。**オン**に
すると、AI エンジンと選ばれたモデルがまだ無ければあわせて導入するため、最初の
`on` には時間がかかることがあります。**オフ**にしてもディスク上のものはそのまま
残り、ローカルでの応答だけを止めます。設定は再起動をまたいで保持され、
バックグラウンドサービスが応答しない状態でも保存され、次回起動時に適用されます。

この設定が**オフ**の状態から始まるマシンは 1 種類です。コーディングの質問に
現実的な時間で答えられないと計測されたマシンです。Waired が判断した場合は
`status` がその理由を表示します。
→ [選んでいないのにローカル AI がオフで始まった](/ja/troubleshooting/#local-ai-started-off-and-i-did-not-choose-that)

メモリの少ないマシンはこの対象ではなくなりました。載せられる最大のモデルが
選ばれ、それが非常に小さいモデルになることはあります。
→ [非常に小さいモデルが選ばれた](/ja/troubleshooting/#waired-chose-a-very-small-model-for-my-machine)

`engine stop` はメモリ逼迫時の緊急手段、`share off` は自分の利用を保ったまま
ほかのマシンからの利用だけを閉じる設定です。
→ [しばらく使わないようにする](/ja/guides/pause/)

`memory status` は、Waired の導入時に空いていたメモリ量と、その計測時刻を
表示します。このパソコンでの「このモデルが載るか」の判断は、すべて**現在の
空き容量ではなくこの値**を基準にしており、次回のインストールまたはアップグレード
まで固定です。大きな処理が動いている最中に計測された場合、値はそのマシンの
実力より低くなり、以後のモデル選択がその値を引き継ぎます。`memory remeasure`
で計測をやり直せます。AI エンジンが読み込まれている間は、そのエンジンのメモリを
マシン側に計上してしまうため実行を拒否します。先に
`waired inference engine stop` で停止してください。

### `waired worker`

**このパソコン**のリクエストの行き先です。

```sh
waired worker get
waired worker set --mode=auto            # 自前の AI があればそれ、なければ他（既定）
waired worker set --mode=local-only      # ほかのパソコンは使わない
waired worker set --mode=peer-preferred  # ほかのパソコンを優先し、駄目ならここで動かす
waired worker set --mode=peer-only       # ほかのパソコンだけ。駄目ならここで動かさずエラー
waired worker set --pin=<peer>           # 常にこの 1 台（--mode=pinned になる）
```

### `waired peers`

```sh
waired peers list
```

自分のほかのパソコンと、それぞれのアドレス・エンジン・グラフィックボード・モデル。
`worker set --pin` に渡す名前はここで調べます。

**MODEL** はそのパソコンで動いているモデル。隣の **MODELS** は同じモデルを AI
ソフトウェア側の名前で表したもので、Ollama と vLLM では異なります。
**WORKER-CAPABLE** はいま応答できるかどうかで、できない場合はその理由も出ます
(モデルを取得中なら `no (loading)` など)。

`no (stale)` はそのパソコンが報告を寄こさなくなった状態です。どれくらい古い報告が
stale 扱いになるかは表の下に出るので、推測する必要はありません。電源が切られている
パソコンも、ネットワークから削除するまで行は残ります — この一覧は「いま起きている
パソコン」ではなく「ネットワークに属しているパソコン」だからです。

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

```sh
waired public status
waired public share --max-clients N    # このパソコンを提供する
waired public unshare                  # やめる（実行中の他人の処理も打ち切られます）
waired public use                      # いまの設定を表示
waired public use --auto               # 自分のより速いときは他人のマシンを使う
waired public use --explicit           # 明示したときだけ使う
waired public use --off
waired public use --min-model-size small|medium|large   # このサイズ以上のモデルを動かすマシンだけ
waired public use --main on|off --sub on|off
```

`use` を最初に有効にするとき、ターミナルに一度だけプライバシー警告が表示され、
読んで承諾する必要があります。

---

## コーディングツール

### `waired link` / `unlink`

```sh
waired link                  # 見つかったすべてのコーディングツールを設定
waired link claude-code
waired link openclaw
waired link openclaw
waired unlink <エージェント>
```

`link` は、ほかのツールが必要とする鍵も作成します
（→ [チャットアプリから使う](/ja/guides/chat-clients/)）。
`unlink` は `link` が追加したものだけを取り消し、それ以外には触れません。

### `waired claude`

```sh
waired claude status
sudo waired claude enable     # Claude Code を自分の AI に向ける（init も行います）
sudo waired claude disable
```

`enable` / `disable` には管理者権限が必要です。認証情報は一切書き込まないので、
claude.ai のサブスクリプションには影響しません。

実行先の切り替えは、再起動なしでその場で反映されます。

```sh
waired claude route                                # 表示
waired claude route waired                         # 自分の AI のみ
waired claude route anthropic                      # 本来の Anthropic API
waired claude route auto                           # 自分を優先し、必要ならフォールバック
waired claude route anthropic --subagents waired   # 分ける
```

引数は**本体の会話**を設定し、`--subagents` はサブエージェントを独立に設定します。
分けるのは実際に有効です → [Claude Code から使う](/ja/guides/claude-code/)。
セッション中は `/waired-route` で同じことができます。
*どのマシン*が応答するかは [`waired worker`](#waired-worker) 側の話で、これではありません。

```sh
waired claude statusline install [--wrap]
waired claude statusline remove
```

現在の経路と、自分のハードウェアが応答した場合はそのモデル名を示すフッター行を管理します。
`enable` が自動で入れるので通常は不要です。`--wrap` は既存のステータス行を
置き換えずに包みます。

---

## ルーティング、アップデート、その他

### `waired pause` / `resume`

```sh
waired pause
waired resume
```

一時停止は**すべて**を止めます。ツールはクラウドに戻り、自分の AI も応答しなくなります。
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
持つログファイル、そして AI エンジンのログを集めます。2 番目は macOS では
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
| `--gateway <url>` | `waired infer` 用の、自分の AI が応答するアドレス（既定 `http://127.0.0.1:9479`。鍵の要らないループバック）。 |
| `--state-dir <dir>` | 識別情報と秘密情報の保存先。環境変数 `WAIRED_STATE_DIR` でも指定できます。 |

<a id="sharing-vs-pausing"></a>

## 混同されやすい 2 つの操作

- **`pause` / `resume`** は*すべて*を止めます。メッシュのルーティングも、
  ローカルの AI も応答しなくなります。このパソコンを完全に外したいときに使います。
- **`inference on` / `off`** は、このパソコンで AI モデルを動かすかどうかを決めます。
  オフでも、ほかのパソコンの AI は使えます。
- **`inference share on` / `off`** は、*自分のほかのパソコン*がこのマシンの AI を
  使えるかどうかだけを制御します。共有オフでも、ここでは `waired infer` が動きます。

個人用のワークステーションなら共有は**オフ**のまま一時停止もしない、
GPU 専用機なら共有を**オン**にしてノートパソコンから使えるようにする、という使い分けになります。
