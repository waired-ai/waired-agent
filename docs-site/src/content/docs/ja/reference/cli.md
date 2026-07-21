---
title: CLI コマンド
description: すべての waired コマンドのリファレンス — セットアップ、モデルと推論、コーディングエージェント連携、ルーティング制御、メンテナンス。
---

`waired` CLI は、ローカルの `waired-agent` サービスと通信します。`waired --help`、
または任意のコマンド・サブコマンドで `waired <command> --help`(`-h` も可)を実行すると、
グループ分けされたコマンド固有のヘルプが表示されます。

## セットアップ & アイデンティティ

| コマンド | 機能 |
|---|---|
| `waired init` | このデバイスを Waired ネットワークに登録します (Google サインイン)。マシンごとに一度実行します。[初回実行](/ja/getting-started/first-run/) を参照。`--mask-pii`（または `WAIRED_PII_MASK=1`）を付けると、ホームディレクトリ・ユーザー名・ホスト名・アカウントのメールアドレスを出力上でマスクします — スクリーンショットや報告への貼り付け用のベストエフォート機能です。 |
| `waired status` | デーモン + アイデンティティのステータスを表示します。ライブのエンジン/メッシュのスナップショットには `--observability` を追加します。サービスインストールではデバイス状態が root 所有のため `sudo` で実行してください（Windows は管理者プロンプト）— 権限が無い場合は推測せず permission エラーで終了します。 |
| `waired doctor` | ✓/⚠/✗ チェックでセットアップを診断します。修正可能なものを修復するには `f` を押します (非対話的に修復するには `--fix`)。 |
| `waired auth status` | デバイストークンの状態 + 有効期限を表示し、必要に応じて再 init を提案します。`waired status` と同様、サービスインストールでは `sudo` が必要です。 |
| `waired logout` | このデバイスのアイデンティティ + シークレットを削除し、次の `init` がクリーンに再登録できるようにします。 |

## モデル & 推論

| コマンド | 機能 |
|---|---|
| `waired models` | ローカルモデルを管理します: `ls` / `pull` / `rm` / `refresh`。`ls` はダウンロード済み一覧を表示し、`ls --detail` は推奨スペック・ハードウェア適合・選定基準を含むカタログを表示します（[モデルカタログ](/ja/reference/model-catalog/) を参照）。`pull` はホストの推奨スペックを超えるモデルの場合に確認を求めます。スクリプトでは `--yes` / `-y` で確認を省略できます。 |
| `waired runtimes` | 推論ランタイムを管理します: `ls` / `install` / `uninstall` / `refresh` / `status` / `benchmark`。 |
| `waired infer "<prompt>"` | Local Gateway 経由でワンショットの推論リクエストを実行します。Auto-Selector のルーティングのドライランには `--explain` を追加します。 |
| `waired inference share <on\|off\|status>` | このエンジンをメッシュピアに提供するかどうかを切り替えます。[下記](#sharing-vs-pausing) を参照。 |
| `waired public status` | このコンピューターが他の Waired ユーザーに公開共有されているか、また他人の公開マシンを利用してよいかを表示します。`--json` で生のオブジェクトを出力します。詳細は[パブリック共有](/ja/public-share/)を参照。 |
| `waired public share` / `unshare` | このコンピューターを公開共有して他の Waired ユーザーが作業を実行できるようにする、またはそれを停止します。`share --max-clients N` は同時に利用できるゲスト数を制限します。`unshare` は現在他人が実行中の作業を切断します。詳細は[パブリック共有](/ja/public-share/)を参照。 |
| `waired public use [--auto\|--explicit\|--off] [--min-tier N] [--main on\|off] [--sub on\|off]` | このコンピューターが他人の公開マシンを利用するかどうかを表示・変更します。フラグなしでは現在の設定を表示するだけです。初めて有効にするときは、読んで承諾する必要がある一度きりのプライバシー警告がターミナルに表示されます。詳細は[パブリック共有](/ja/public-share/)を参照。 |
| `waired worker get` / `set` | アウトバウンド推論の流れ先を選択します: `set --mode=auto\|local-only\|peer-preferred`、または `set --pin=<peer>`。 |
| `waired peers list` | 既知のメッシュピア (DeviceID、IP、エンジン、GPU、モデル) を一覧表示します — `--pin` のターゲットを選ぶのに便利です。 |

## コーディングエージェント

| コマンド | 機能 |
|---|---|
| `waired link [agent]` | ユーザーごとのコーディングエージェント連携をセットアップします (Claude Code のスキル + OpenCode のプラグイン + ゲートウェイトークン)。[コーディングエージェント](/ja/guides/coding-agents/) を参照。 |
| `waired unlink [agent]` | 連携を削除します (外科的な操作で — `link` が追加したものだけを取り消します)。 |
| `waired claude <enable\|disable\|status>` | Claude Code のマネージド設定連携を管理します (Linux/macOS/Windows): `enable` は Claude Code をローカル推論へ向け (`waired init` でも実行されます)、`/waired-route` スラッシュコマンド・現在のルートを示すフッターのステータスライン・ターンごとのフォールバック通知をインストール、`disable` はそれらを元に戻し、`status` は現在の状態を表示します。資格情報は書き込まれないため claude.ai サブスクリプションは維持され、ローカルでの配信が停止しているときは本物の API へ fail open します。`enable`/`disable` には昇格 (`sudo`、Windows では管理者権限) が必要です。 |
| `waired claude route [auto\|waired\|anthropic] [--subagents same\|auto\|waired\|anthropic]` | Claude Code の実行先を再起動なしで表示・設定。引数は**メイン会話**、`--subagents` はサブエージェントを独立に設定(既定 `same` = main に追従)。`auto` は Waired 優先で失敗時に実 Anthropic API へフォールバック、`waired` は Waired 推論のみで Anthropic には一切繋がない、`anthropic` は Claude サインインで常に実 API。ハイブリッド: `waired claude route anthropic --subagents waired` はメイン会話を Anthropic、バルクなサブエージェントを Waired に([Privacy](/concepts/privacy/) 参照)。*どの* Waired ノードで配信するかは `waired worker` に従う。セッション内では `/waired-route` としても利用可。 |
| `waired claude statusline [install [--wrap]\|remove]` | 現在のルート(ローカル配信中は直前のリクエストに応答したモデルも)を示す Claude Code フッターのステータスラインを管理。`enable` が自動で追加(既存のステータスラインがあれば確認)、`install --wrap` は既存を包み、`remove` で戻す。引数なしのコマンドは Claude Code が毎ターン実行するもの。 |

## ルーティング制御

| コマンド | 機能 |
|---|---|
| `waired pause` | Waired のルーティングを一時停止します — ローカルゲートウェイは Anthropic/OpenAI 呼び出しのリダイレクトを停止します。再起動後も保持されます。 |
| `waired resume` | `pause` を取り消し、オーバーレイルーティングを復元します。 |

## ネットワーク & メンテナンス

| コマンド | 機能 |
|---|---|
| `waired ping <peer>` | デーモン経由でピアにオーバーレイ ping を送信します。 |
| `waired keygen` | WireGuard キーペアを生成します (通常は `init` がこれを行います)。 |
| `waired version` | ビルドバージョンを出力します (`{version, buildSHA, os, arch}` には `--json`)。 |
| `waired update` | 現在のチャンネルを維持したまま更新を確認して適用します (報告のみは `--check`、非対話的に適用するには `--yes`、チャンネル切り替えは `--edge` / `--stable`)。[インストール → 更新](/ja/getting-started/install/#update) を参照。 |

## グローバルフラグ

これらは `status` / `ping` / `models` / `runtimes` / `infer` に適用されます:

- `--mgmt <url>` — ローカル管理 API のベース URL (デフォルト `http://127.0.0.1:9476`)。
- `--gateway <url>` — `waired infer` 用の Local Gateway のベース URL (デフォルト `http://127.0.0.1:9479`、トークン不要のローカルゲートウェイ。トークン保護された `http://127.0.0.1:9473` を指定した場合、読み取れる場合に限り gateway token を自動で付与します)。

<a id="sharing-vs-pausing"></a>
## 共有と一時停止の違い

これら 2 つの制御は独立しています:

- **`waired pause` / `resume`** は*すべての*オーバーレイルーティングを停止します — 一時停止中は
  メッシュルーティングとローカル推論ゲートウェイの両方が 503 を返します。デバイスを
  一時的にループから外すために使用します。
- **`waired inference share on` / `off`** は、*他のピア*があなたのエンジンを使用できるか
  どうかだけを制御します。共有をオフにしても、自分で推論を実行することはできます
  (`waired infer` は動作します)。ピアがあなたのモデルに到達できなくなるだけです。

したがって、プライベートなワークステーションでは共有を **オフ** に保ちつつ一時停止しない
ままにしておくとよいでしょう。専用の GPU マシンでは、他のデバイスが使えるように共有を
**オン** にするとよいでしょう。
