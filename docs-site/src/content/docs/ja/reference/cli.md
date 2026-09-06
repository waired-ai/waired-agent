---
title: CLIコマンド
description: すべてのwairedコマンドを目的ごとに分け、共通のフラグと、各グループを説明するページを示します。
meta:
  audience: ターミナルで作業する人、画面のないパソコンを使う人
  needs: インストール済みのWaired
  time: コマンドを探して、そのページを開くだけ
---

これらのページにあることは、注記がないかぎり[Wairedアプリ](/ja/guides/waired-app/)
からも行えます。コマンドのフラグの一覧は`waired <command> --help`で表示されます。
これらのページでは、フラグが何のためにあるかを説明します。

## <a id="commands-by-group"></a>グループ別のコマンド

### <a id="setup-and-sign-in-commands"></a>[セットアップとサインインのコマンド](/ja/reference/cli/setup/)

| コマンド | 動作 |
|---|---|
| [`waired init`](/ja/reference/cli/setup/#waired-init) | このパソコンをサインインさせてセットアップする |
| [`waired status`](/ja/reference/cli/setup/#waired-status) | すべて動いているか |
| [`waired doctor`](/ja/reference/cli/setup/#waired-doctor) | 各項目を検査し、大半を修復する |
| [`waired auth status`](/ja/reference/cli/setup/#waired-auth-status) | このパソコンのサインインはいつ失効するか |
| [`waired logout`](/ja/reference/cli/setup/#waired-logout) | このパソコンの識別情報を削除する |

### <a id="model-and-engine-commands"></a>[モデルとエンジンのコマンド](/ja/reference/cli/models/)

| コマンド | 動作 |
|---|---|
| [`waired infer`](/ja/reference/cli/models/#waired-infer) | 自分のモデルにいまリクエストを1つ送る |
| [`waired models`](/ja/reference/cli/models/#waired-models) | ダウンロード済みの確認、追加のダウンロード、動かすモデルの選択、ダウンロードの中止、削除 |
| [`waired runtimes`](/ja/reference/cli/models/#waired-runtimes) | 推論エンジン自体と、ベンチマーク |
| [`waired inference`](/ja/reference/cli/models/#waired-inference) | ここでモデルを動かすかどうか、推論エンジンの起動と停止、keep-alive、メモリ |

### <a id="routing-and-sharing-commands"></a>[ルーティングと共有のコマンド](/ja/reference/cli/routing/)

| コマンド | 動作 |
|---|---|
| [`waired share`](/ja/reference/cli/routing/#waired-share) | このパソコンを貸し出すかどうか |
| [`waired worker`](/ja/reference/cli/routing/#waired-worker) | どのパソコンがリクエストに答えるか |
| [`waired peers`](/ja/reference/cli/routing/#waired-peers)と[`waired ping`](/ja/reference/cli/routing/#waired-ping) | 自分のほかのパソコン |
| [`waired public`](/ja/reference/cli/routing/#waired-public) | ほかのWairedユーザーと空いているパソコンを貸し借りする |
| [`waired pause`](/ja/reference/cli/routing/#waired-pause-and-resume)と[`waired resume`](/ja/reference/cli/routing/#waired-pause-and-resume) | ルーティングの停止と再開 |

### <a id="coding-tool-commands"></a>[コーディングツールのコマンド](/ja/reference/cli/coding-tools/)

| コマンド | 動作 |
|---|---|
| [`waired link`](/ja/reference/cli/coding-tools/#waired-link-and-unlink)と[`waired unlink`](/ja/reference/cli/coding-tools/#waired-link-and-unlink) | コーディングツールを接続する |
| [`waired claude`](/ja/reference/cli/coding-tools/#waired-claude) | Claude CodeをWairedに向ける、ステータス行、サブエージェントの実行先 |

### <a id="maintenance-commands"></a>[メンテナンスのコマンド](/ja/reference/cli/maintenance/)

| コマンド | 動作 |
|---|---|
| [`waired update`](/ja/reference/cli/maintenance/#waired-update) | 新しいWairedをインストールする |
| [`waired config`](/ja/reference/cli/maintenance/#waired-config) | 詳細なログのオンとオフ |
| [`waired logs`](/ja/reference/cli/maintenance/#waired-logs) | 不具合報告のために直近のログをファイルに保存する |
| [`waired version`](/ja/reference/cli/maintenance/#waired-version) | これはどのビルドか |
| [`waired keygen`](/ja/reference/cli/maintenance/#waired-keygen) | 鍵ペアを手動で生成する |

## <a id="flags-that-apply-nearly-everywhere"></a>ほとんどのコマンドで使えるフラグ

| フラグ | 意味 |
|---|---|
| `--mgmt <url>` | バックグラウンドサービスが待ち受けている場所。既定は`http://127.0.0.1:9476`。 |
| `--gateway <url>` | `waired infer`で、自分のモデルが答える場所。既定は`http://127.0.0.1:9473`。 |
| `--state-dir <dir>` | Wairedが識別情報と秘密情報を置く場所。`WAIRED_STATE_DIR`でも設定できます。 |

`waired models`のように動詞なしでコマンドグループだけを入力すると、ヘルプを表示
してエラーで終了します。スクリプトが何かを実行したと誤解しないためです。

<a id="sharing-vs-pausing"></a>

## <a id="three-controls-people-mix-up"></a>混同されやすい3つの操作

- **`pause`と`resume`**は、このパソコンのすべてを止めます。ルーティングもローカル
  推論も答えなくなります。パソコンを処理の輪から外すときに使います。
- **`inference on`と`off`**は、このパソコンでモデルを動かすかどうかを決めます。
  オフでも、自分のほかのパソコンのモデルは使います。
- **`share on`と`off`**は、自分のほかのパソコンや公開のゲストがこのパソコンの
  モデルを使えるかどうかだけを決めます。共有がオフでも、`waired infer`はここで
  動きます。

個人のワークステーションでは共有をオフにして一時停止はしない、専用のGPUマシンでは
ノートパソコンから使えるよう共有をオンにする、といった使い分けになります。
[Wairedを一時停止する](/ja/guides/pause/)を参照してください。
