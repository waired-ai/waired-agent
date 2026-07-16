---
title: トラブルシューティング
description: 何かおかしいときに実行する 3 つのコマンド、症状から対処への対応表、そして最もよくある Waired の問題への対処法。
---

何かおかしいと感じたら、3 つのコマンドでほぼすべてを診断できます。順番に実行して
ください。

| # | コマンド | わかること |
|---|---|---|
| 1 | `waired doctor` | ゲートウェイトークン、一時停止状態、エンジンの準備状況、メッシュピア、コーディングエージェント統合について ✓/⚠/✗ を表示。ターミナルでは `f` を押すと修復可能なものを修復します（非対話的に修復するには `--fix`）。 |
| 2 | `waired status --observability` | ライブスナップショット: エンジン、共有、一時停止、メッシュ（enrolled / reachable / ready）、および直近の推論の判断 + レイテンシ。マシン可読な出力には `-o json` を追加します。 |
| 3 | `waired claude status` | Claude Code のマネージド設定連携が有効かどうか、そして `managed-settings.json` ファイルの場所。 |

:::tip[まずは必ず `waired doctor` を実行]
Waired は、障害が静かにではなく可視になるよう設計されています。`waired doctor` は
正面玄関です。ログを掘り下げる前にこれを実行してください。
:::

## 症状 → 最初のコマンド

| 症状 | まず実行 | 何が起きているか |
|---|---|---|
| Claude Code がローカルモデルではなくクラウドを使う | `waired doctor` → `waired claude status` | ゲートウェイトークン / 一時停止 / エンジン / メッシュピアが ✗ を示す場合、サービングは Anthropic へフェイルオープンしています。`f` を押してトークン + スキルを再構築してください。マネージド設定が未有効なら有効化します（下記参照）。 |
| `waired infer` が 503 `waired_paused` を返す | `waired resume` | ルーティングが一時停止中です。再開してください。 |
| `waired infer` が 503 `waired_inference_disabled` を返す | `waired inference share on` | 共有がオフになっています。（ローカル専用利用ならオフのままで問題ありません — 自分の `waired infer` は引き続き動作します。） |
| `Engine: not ready` | `waired runtimes status` + `waired models ls` | ランタイムがダウンしているか、モデルがまだプル中です。 |
| メッシュピアが現れない / `reachable=false` | `waired status --observability -o json` | エンロール、コントロールプレーン同期、WireGuard の到達性を確認してください。 |
| システムトレイのアイコンが表示されない（Linux） | `waired doctor` | GNOME ではトレイに AppIndicator ホスト拡張が必要です。`system tray host` の行で拡張が無いことがわかります（下記参照）。 |
| `waired status` が「システム全体でエンロール済み」だが要昇格と表示する | `sudo waired status` | サービスインストールではデバイス状態が root 所有です。完全な状態を見るには昇格して再実行してください（下記参照）。 |
| コマンドが「waired-agent is not running」と表示する | `waired doctor` | ローカルデーモンに到達できません — サービスを再起動してください（[さらに深く](#さらに深くログ)参照）。 |

## よくある対処法

### `waired status` が「システム全体でエンロール済み」（要昇格）と表示する

サービスインストール(Linux/macOS で通常の `sudo waired init` を行ったケース、または
Windows のサービス)では、デバイス状態は root(Windows では SYSTEM +
Administrators)だけが読めるシステムディレクトリ(Linux では `/var/lib/waired`、
Windows では `%ProgramData%\waired`)に保存されます。一般ユーザーで `waired status`
や `waired auth status` を実行してもこれを読めないため、コマンドは推測せず、デバイスが
**システム全体でエンロール済み**であることを伝えて終了コード 0 で終了します(状態確認は
失敗ではありません)。完全な識別情報とデーモン状態を見るには、昇格して再実行してください
— `sudo waired status`(Windows では管理者プロンプトから)。

代わりに `Not enrolled. Run \`waired init\` to connect this device.` が終了コード 0 で
表示される場合は、そのマシンは本当に未エンロールです — `waired init` を実行してください。

### `waired init` が「デバイス数の上限に達した」と表示する

1 アカウントでエンロールできるデバイス数には（十分に大きめの）上限があります。
`waired init` が **device limit reached**（デバイス数上限）のメッセージで止まった
場合はこの上限に達しています。ほとんどは、もう使っていない古いマシンがまだエンロール
されたままなのが原因です。Web 管理画面を開いて不要なデバイスを削除してから、もう一度
`waired init` を実行してください。**すでにエンロール済み**のマシンで（再認証のために）
`waired init` を実行する分には、上限にはカウントされません。

### Claude Code（または OpenCode）が自分のモデルを使っていない

`waired doctor` を実行します。ゲートウェイトークンや統合についてフラグが立った場合は、
`f` を押す（または `waired link all` を実行する）と再構築されます。`waired claude status`
がマネージド設定連携が未有効であることを示している場合は有効化します: `sudo waired claude enable`
（Linux/macOS）、または管理者権限で `waired claude enable`（Windows）。
[コーディングエージェント](/ja/guides/coding-agents/) を参照してください。

`waired claude status` は、リクエストが自分のモデルではなく本物の Anthropic API に
送られた際の**最後のフォールバック**とその理由も表示します:

- `local_no_model` — このデバイスでアクティブなローカルモデルがまだありません。
  `waired status` を確認してモデルを選択/取得してください。
- `local_status_<コード>` — フォールバック直前にローカル配信がその HTTP ステータスで
  エラーになりました。詳細は `waired status --observability` で確認できます。

### Claude Code の長いセッションがコンパクトされる（または「prompt is too long」）

これは想定どおりの正常な挙動です。ローカルモデルのコンテキスト窓は Claude の
ホスト型モデルより小さいため、Waired が実際の窓を Claude Code に伝え、Claude Code
が会話を自動コンパクト（要約）して収めます — 長いセッションでも、コンテキストの
先頭が無言で切り捨てられることなく機能し続けます。一瞬「prompt is too long」の
表示が出ても、Claude Code が自動でコンパクトして再送するので操作は不要です。
より大きな窓が欲しい場合は `/waired-route anthropic` でセッションを本物の
Anthropic API に切り替えられます — 選択中モデル本来の窓（`[1m]` モデルなら 1M）が、
同じセッションでも次のリクエストから適用されます。
[コーディングエージェント](/ja/guides/coding-agents/)を参照。

### Claude Code に waired のステータスラインが表示されない

**プロジェクトディレクトリの中で** `waired claude status` を実行してください。
Claude Code のステータスラインは単一スロットで優先順位が厳格に決まっており、
プロジェクトの `.claude/settings.local.json` / `.claude/settings.json` は
Waired が入れるユーザーレベルの設定より優先されます。shadow されている場合、
status の出力が該当ファイル名と、そのステータスラインスクリプトに追記すれば
ルートを表示できる 1 行スニペットを表示します。また `waired claude enable` の
あとに Claude セッションを再起動したかも確認してください。

### エンジンが「not ready」のまま

サインイン中、`waired init` はエンジンの起動を段階的に表示します — 「Starting the
inference engine…」「Preparing to download <モデル>…」のあと、ライブの
「Downloading <モデル>: NN%  X.X GB / Y.Y GB (Z MB/s)」バー、最後に「<モデル> ready」。
大きなモデルの初回ダウンロードは数 GB あるため、ダウンロード段階に時間がかかるのは
正常で、バーは進み続けます。

代わりに「Waiting for the inference engine to start…」のまま止まり、その後エンジンが
まだ起動していない旨が表示された場合は、ローカルエンジンが起動していません。
`waired status` で現在の状態を、`waired doctor`（Linux では
`journalctl -u waired-agent -e`）で詳細を確認してください。
モデルがまだダウンロード中の可能性もあります — `waired models ls` で進捗が表示されます。
ランタイム自体がダウンしている場合は、`waired runtimes status` に詳細が表示されます。
モデルの初回ロードは遅く（コールドな CUDA ロードは約 60 秒かかることがあり、ROCm も
同様です）、エンジンは自動的に再試行して回復します。

<a id="a-model-wont-load-on-an-integrated-gpu"></a>
### 統合 GPU でモデルがロードできない

最近の Ollama バージョンは、統合 GPU（AMD Strix Halo / Radeon、Intel iGPU）を
デフォルトで無効にし、静かに CPU へフォールバックします。これらには
`OLLAMA_IGPU_ENABLE=1` が必要です。Windows インストーラは既知の iGPU についてこれを
設定します。その他のシステムでは、Ollama サービス向けにこの環境変数を設定して
再起動してください。また、モデルが収まるかどうかも確認してください —
[モデルカタログ](/ja/reference/model-catalog/) の RAM/VRAM の列を参照してください。

### 「このマシンはごく小さなモデルしか動かせません」— 有効化すべき?

RAM の少ないマシンでは、`waired init` が「ごく小さいモデルしか収まらない」と
表示し、それでもローカル推論を有効にするか確認することがあります（既定は
**いいえ**）。そのサイズではローカルのコーディング品質が極めて低く、壊れた出力に
なりがちで、通常は動かす価値がありません。断っても Waired は安全な
ゲートウェイ/リレーとして動作します（メッシュ上の能力あるピアへルーティング
可能）。同様に、init 末尾のベンチマークでモデルが遅すぎ、さらに軽い選択肢が
その極小モデルしか無い場合は、ローカル推論を無効化するか尋ねます。いずれの選択も
後から `waired runtimes benchmark`、あるいはより高性能なハードウェアで
`waired init` を再実行して見直せます。

補足: ベンチマークはリクエストのオーバーヘッドを除いた純粋なデコード速度を
計測するようになったため、tok/s の値は以前のリリースより高く表示されます
（従来は高速なマシンほど過小評価されていました）。アップグレード後の初回起動
では、古いキャッシュ値を使わず一度だけ再計測します。

### 推奨スペックを超えるモデルを選んだ場合

Waired はホストの推奨スペックを超えるモデルの実行を **警告はするが禁止はしません**。
そのようなモデルを pull / 切り替えると、不足分（例: `needs 32 GB RAM (have 31 GB)`）
を示す確認が一度だけ表示されるので、続行を選べます。スクリプトでは
`waired models pull` に `--yes` を渡すと確認を省略できます。

推論自体はスペック超過を理由にブロックされません。モデルはロードされ動作します
（遅くなる場合や、重みが本当に収まらない場合はエンジン側で明確なロード失敗と
なります — 事前の一律拒否ではありません）。以前のリリースは、わずかに推奨メモリ
を超えただけでもリクエスト時に `422 hardware_insufficient` を返していましたが、
その推論時ブロックは廃止しました。

推奨 RAM/VRAM 値には安全マージンが含まれ、ユニファイドメモリ機（Apple Silicon,
AMD Strix Halo）では残りのシステム RAM ではなく GPU が使えるプール容量で判定
されます。ホスト上の各モデルの適合状況は `waired models ls --detail` または
[モデルカタログ](/ja/reference/model-catalog/) で確認できます。

### システムトレイのアイコンが表示されない（Linux / GNOME）

GNOME には組み込みのシステムトレイが無いため、`waired-tray` のアイコンは
**AppIndicator ホスト拡張** がインストールされ有効になっているときだけ描画されます。
`waired init` は GNOME を検出すると拡張を自動でインストール・有効化し、
`waired doctor` は拡張が無いと `system tray host` の警告を表示します。手動で設定する
場合:

```sh
sudo apt install gnome-shell-extension-appindicator
gnome-extensions enable appindicatorsupport@rgcjonas.gmail.com
```

その後、GNOME が拡張を読み込むよう **ログアウトして再ログイン**（Wayland では必須）
してください。KDE Plasma はトレイホストを内蔵しているため何も不要です。MATE は
アイコンをまったく表示できません — GNOME（拡張あり）か KDE で表示してください。

### Windows: `waired infer` が 502 を返す（Ollama がインストールされていない）

現在のインストーラーはデフォルトで Ollama を同梱しますが、`-SkipOllama` /
`WAIRED_NO_OLLAMA=1` でインストールしたホスト（または旧インストーラーで
インストールしたホスト）にはエンジンがありません。昇格プロンプトで
`waired runtimes install ollama` を実行して追加するか、直接インストールして
ください:

```powershell
iwr -useb https://github.com/waired-ai/waired-agent/releases/latest/download/ollama-windows.ps1 | iex
```

### ピアに到達できない

`waired status --observability -o json` を実行し、各ピアの `reachable` と
`last_check` を確認してください。次に以下を確認します:

- 両方のデバイスが **同じ Google アカウント** でエンロールされていること（同じ
  ネットワーク）。各デバイスの `waired status` の `account` / `network` 行を比較して
  ください。
- WireGuard が接続できること — `waired peers list` でピアのエンドポイントが表示
  されます。直接 UDP を開けない場合はファイアウォールや NAT を疑ってください。
  Waired は自動的にリレーへフォールバックするため、直接の経路が機能しなくても接続性は
  保たれるはずです。
- Network Map が最新であること — `waired status` が古いマップを示している場合は、
  エージェントを再起動してください。

## さらに深く（ログ）

`waired doctor` を実行した後でのみ:

- **Linux:** `journalctl -u waired-agent -e`
- **Windows:** `Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50`
- **Ollama（同梱エンジン）:** モデルロード中に 503 が繰り返される場合は、waired の
  state ディレクトリ配下のエンジンログ `…/runtimes/ollama/logs/engine.log`（Linux:
  `/var/lib/waired/…`、macOS: `/Library/Application Support/waired/…`）。自前の
  Ollama を使っている場合（`--skip-ollama` + reuse）は `~/.ollama/logs/server.log`。

`Restart-Service waired-agent`（Windows）または `systemctl restart waired-agent`
（Linux）で、一時的な不整合のほとんどは解消します。
