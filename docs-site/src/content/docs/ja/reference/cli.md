---
title: CLI コマンド
description: すべての waired コマンドを、やりたいことごとにまとめたリファレンス。重要なフラグと、何が表示されるか。
meta:
  audience: ターミナルで作業する人、画面のないマシンを扱う人
  needs: Waired がインストール済みであること
  time: 索引を眺めて、必要な節だけ読む
sourceHash: ff8ee266feefe9aa
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
| [`waired models`](#waired-models) | 何が入っているか、追加、削除 |
| [`waired runtimes`](#waired-runtimes) | AI ソフトウェア本体と、速度テスト |
| [`waired inference`](#waired-inference) | エンジンの起動・停止、自分のほかのパソコンへの提供 |
| [`waired worker`](#waired-worker) | どのパソコンが答えるか |
| [`waired peers`](#waired-peers) / [`ping`](#waired-ping) | 自分のほかのパソコン |
| [`waired public`](#waired-public) | ほかの Waired ユーザーと空きマシンを貸し借りする |
| [`waired link`](#waired-link--unlink) / [`unlink`](#waired-link--unlink) | コーディングツールをつなぐ |
| [`waired claude`](#waired-claude) | Claude Code の実行先と、その場での切り替え |
| [`waired codeui`](#waired-codeui) | ブラウザで動くコーディングエージェント |
| [`waired pause`](#waired-pause--resume) / [`resume`](#waired-pause--resume) | ルーティングの停止と再開 |
| [`waired update`](#waired-update) | 新しい Waired を入れる |
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

### `waired logout`

このパソコンの識別情報と秘密を削除し、次の `waired init` が
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
waired models rm <モデルID>        # 削除して数 GB 空ける
waired models refresh             # このマシンにもっと合うモデルはあるか
```

`pull` はモデルが使える状態になるまで待ち、このパソコンの推奨スペックを超える場合は
確認を求めます（スクリプトでは `--yes` で省略）。`rm` も実行前に確認します。
モデル ID は[モデルカタログ](/ja/reference/model-catalog/)にあります。

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
両方のモデルを品質ランクつきで示すので、速さと質を見比べて選べます。

### `waired inference`

```sh
waired inference engine start     # モデルを読み込む
waired inference engine stop      # 確保しているメモリを解放する
waired inference engine status

waired inference share on         # 自分のほかのパソコンに、このマシンの AI を使わせる
waired inference share off
waired inference share status
```

`engine stop` はメモリ逼迫時の避難口、`share off` は自分の利用を保ったまま
ほかのマシンからの利用だけを閉じる設定です。
→ [しばらく使わないようにする](/ja/guides/pause/)

### `waired worker`

**このパソコン**のリクエストの行き先です。

```sh
waired worker get
waired worker set --mode=auto            # 自前の AI があればそれ、なければ他（既定）
waired worker set --mode=local-only      # ほかのパソコンは使わない
waired worker set --mode=peer-preferred  # ほかのパソコンを優先する
waired worker set --pin=<peer>           # 常にこの 1 台（--mode=pinned になる）
```

### `waired peers`

```sh
waired peers list
```

自分のほかのパソコンと、それぞれのアドレス・エンジン・グラフィックボード・モデル。
`worker set --pin` に渡す名前はここで調べます。

### `waired ping`

```sh
waired ping <peer>
```

このパソコンから、プライベートネットワーク越しに別のマシンへ実際に届くかを確認します。

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
waired public use --min-tier N         # この品質ランク以上のマシンだけ
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
waired link opencode
waired link openclaw
waired unlink <エージェント>
```

`link` は、ほかのツールが必要とする鍵も作成します
（→ [チャットアプリから使う](/ja/guides/chat-clients/)）。
`unlink` は正確で、`link` が追加したものだけを取り消します。

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

### `waired codeui`

コーディングエージェントをブラウザで、実際のプロジェクトを対象に、自分の AI で動かします。
インストールは不要です。

```sh
waired codeui open
waired codeui open --project DIR
waired codeui open --no-browser     # ブラウザを開かずアドレスを表示（SSH）
waired codeui url
waired codeui status
waired codeui stop
```

実行ユーザーは自分自身で、自分だけが使えます。
同じマシンのほかのユーザーも、ネットワーク上のほかのパソコンも拒否されます。

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
waired update --force      # キャッシュされた確認結果を無視
waired update --notify on|off   # アプリのアップデート通知ポップアップ
```

→ [Waired を更新する](/ja/getting-started/update/)。`--notify off` はポップアップだけを止め、
Waired アプリのメニュー内の項目はどちらでも残ります。

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
| `--state-dir <dir>` | 識別情報と秘密の保存先。環境変数 `WAIRED_STATE_DIR` でも指定できます。 |

<a id="sharing-vs-pausing"></a>

## 混同されやすい 2 つの操作

- **`pause` / `resume`** は*すべて*を止めます。メッシュのルーティングも、
  ローカルの AI も応答しなくなります。このパソコンを完全に外したいときに使います。
- **`inference share on` / `off`** は、*自分のほかのパソコン*がこのマシンの AI を
  使えるかどうかだけを制御します。共有オフでも、ここでは `waired infer` が動きます。

個人用のワークステーションなら共有は**オフ**のまま一時停止もしない、
GPU 専用機なら共有を**オン**にしてノートパソコンから使えるようにする、という使い分けになります。
