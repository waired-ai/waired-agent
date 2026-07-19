# Decision Log

New entries at the top. Format: see CLAUDE.md §Decision Log.

## proto の並行開発運用 — auto-tag / additive-only guard / 擬似バージョン許可 (20260719)

### Status
Accepted

### Context
`proto/` は control plane / relay からも import される共有 wire contract で、公開バージョン(`proto/vX.Y.Z`)は不変。複数の開発セッションが同時に proto へ変更を積もうとすると、タグの採番・打鍵と消費側のタグ待ちが直列化ボトルネックになる。一方で契約を git ブランチで分岐させることはできない — デプロイされたフリートが喋るプロトコルは常に 1 本のマージ済みタイムラインであり、ブランチ性は「未確定作業の隔離」にしか使えない。

### Decision
1. **開発中は main に触れない**: 本リポジトリは in-tree `replace`、消費側リポジトリは一時 `replace` または「マージ済み main コミット」の擬似バージョンで参照する。ブランチ上のコミットハッシュへの依存は禁止(rebase で消える)。タグへの正規化は後続の 1 行 chore。
2. **設計確定ゲート**: proto 変更は tracking issue に確定フィールド表が載り actionable になってから、単独の小 PR としてマージする。公開はラチェット(取り消せない)なので、確定前の契約面を main に積まない。
3. **additive-only を CI で機械強制**(`proto-guard` / `scripts/ci/protoguard`): 直前タグとの比較で、公開済み exported API の削除・型変更・struct タグ変更・const 値変更・シグネチャ変更を fail。公開済み struct へのフィールド追加は `omitempty`(または `json:"-"`)必須。canonical JSON バイト同一性テストを同 PR の必須慣行とする。
4. **タグ自動発行**(`proto-tag.yml`): main への `proto/**` マージごとに次のパッチタグを自動で打つ(concurrency group で採番レースなし)。minor/major は workflow_dispatch で手動。`proto/v0.2.0` までは休止(マイルストーンタグは手動で 1 回)。
5. **実行時の分岐は capability 文字列**(例: `public-share-v1`)で行い、未完成の契約面はエージェントが宣言するまで不活性に保つ。

検討して退けた代替: submodule pin(バンプ競合が gitlink に移るだけで UX が悪化)、擬似バージョン一本化(MVS の順序意味論と go.mod diff のレビュー可視性を喪失)、ソースコピー同期(真実の源が二重化し drift 検査という新しい機械が必要)。

### Consequences
- 複数セッションの proto 変更が採番調整なしに並行でき、直列区間は main へのマージ順序のみになる。
- 公開済み契約は消せないため、品質の関門は git 構造ではなく設計確定ゲート(2)。誤公開は `retract` + deprecated コメントで処理する。
- 追加フィールドの既定が omitempty + capability ゲートに固定されることで、署名済み map のバイト同一性(非対応 poller への互換)が構造的に守られる。

### Refs
- .github/workflows/proto-tag.yml / proto-guard.yml、scripts/ci/protoguard/
- https://github.com/waired-ai/waired-agent/pull/84 (proto/v0.2.0 契約、waired#816)

## Ollama tuning verify を per-model 化し、num_parallel は runner の実値を報告 (20260714 21:31)

### Status
Accepted

### Context
waired#763: post-load tuning verify (`verifyOllamaTuning`) は `/api/ps` の
先頭モデルを見て `OLLAMA_CONTEXT_LENGTH` の適用を判定していた。「context は
server-global」という前提だったが、現行 Ollama はモデルごとに llama-server を
`-c` 付きで起動する per-model 構成。モデル切替直後は前モデルがまだ `/api/ps` に
residentで、別モデルの context を対象 tuning と突き合わせて
`OLLAMA_CONTEXT_LENGTH did not apply` を誤検知していた(正常な大窓構成が壊れて
見える)。加えて status の `num_parallel` は常に intent 値で、Ollama が
per-slot KV 不足時に `OLLAMA_NUM_PARALLEL` を黙って下げても runner の実 `-np` を
反映していなかった。

### Decision
1. **per-model 化**: verify は対象 tag の runner に一致した時のみ判定し、別モデル
   しか載っていなければ `tuningInconclusive` で abstain(次回 boot/swap で再検証)。
   これで cross-model の誤 warning が消える。
2. **runner 実値の報告**: 新規 `internal/platform/proclist`(linux `/proc`,
   windows `Get-CimInstance`, darwin `ps -axww`, 他は unsupported stub)で
   llama-server / `ollama runner` の command line を読み、対象 runner を context で
   相関して実 `-np` を取得。`ModelTuning.ObservedNumParallel` に記録し、status は
   観測値があればそれを、無ければ intent を報告。観測 < intent の時は誤アラームでは
   ない reduce の note を残す。context 表示は per-request 窓(intent)を維持
   (`-c` は総和で誤解を招くため表示には使わず相関のみ)。

### Consequences
- 誤検知 warning が止まり、`num_parallel` が実配信値になる。
- 純粋パーサは shared file に置き linux CI でテスト可能、I/O のみ per-OS。process
  列挙は verify 1 回のみで hot path ではない。相関は単一/一意一致のみ採用し、曖昧なら
  intent へ fallback(誤帰属を避ける)。
- 新規 `internal/` パッケージは testnet-nonrelevant に分類。

### Refs
- https://github.com/waired-ai/waired-agent/pull/21 (Fixes waired-ai/waired#763)
- waired-ai/waired#763 / #761 / #621 / #623

## モデル切替の restart-first を維持し、pre-restart の pull を廃止 (20260714 02:44)

### Status
Accepted

### Context
waired#774: ベンチマーク後のモデル切替受諾が fire-and-forget で、インストール終了時に
マシンが使える状態にならない。`/inference/preferred-model` ハンドラは (a) リクエスト
コンテキストで background pull を起動し、(b) pull を待たず即座にエージェント再起動を
スケジュールしていた。再起動が (a) の pull を数ミリ秒でキャンセルするため、この pull は
実質機能せず、キャンセル時に一時的な failed 状態を書き残す。

### Decision
再起動を pull 完了まで遅延させるのではなく、restart-first を維持し、ハンドラからの
pre-restart pull 呼び出しを削除する。CLI 側 (`waitForModelSwitch`) がフォアグラウンドで
`/inference/status` を model ID 指定でポーリングし、進捗表示・Enter でのバックグラウンド化を
提供する。

### Consequences
- 実際の pull は再起動後の `bootstrapPreferredModel`(#347)が一元的に実行し、
  `activatePreferredIfNeeded` が完了後にのみ Active を切り替える。ダウンロード中も
  旧モデルが配信を継続するため、restart 遅延による可用性向上はない。
- ハンドラ内に pull 完了監視という第二の適用パスを持たずに済む。
- キャンセル起因の一時的 failed 状態のレースが元から消える。CLI 側は防御として
  failed の 3 連続観測までを過渡状態として扱う。
- 再起動直後は management API が数秒落ちるため、CLI のポーリングは到達不能を
  許容して継続する。

### Refs
- https://github.com/waired-ai/waired/issues/774
- internal/management/inference_preferred_model.go
- cmd/waired/init_pull.go (waitForModelSwitch)
- cmd/waired-agent/inference.go (bootstrapPreferredModel / activatePreferredIfNeeded)

## 静的 CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000 を撤去し、窓は Claude Code のモデル別解決 + per-request 400 に任せる (20260714 02:41)

### Status
Accepted

### Context
waired#623 は「ゲートウェイ越しの Claude Code は窓を 200K と仮定する」前提で、
ルート対応 /v1/models 広告のバックストップとして managed settings に静的
`CLAUDE_CODE_AUTO_COMPACT_WINDOW=200000` を書いた。waired#771 で、この env が
Claude Code の窓解決の最優先層(`window = min(モデル窓, env値)`、起動時固定)
であり、anthropic ルートで 1M(`[1m]`)モデルを選んでも 200k セッションに
潰れることが判明。v2.1.207 バイナリの解析で、(a) gateway discovery の
`max_input_tokens` は compaction 窓に流れない(ピッカー専用)、(b) モデル窓は
モデル id からターン毎に再評価される、(c) 200k 未満のローカル窓を実際に
守っていたのは常に per-request の "prompt is too long" 400 だった、が確定
(詳細: docs/knowledges/20260714/0241-claude-code-context-window-internals.md)。

### Decision
- managed settings への静的 pin の書き込みを廃止。`Write` はレガシー値
  `"200000"` をスクラブし(オペレータ設定の別値は Write/Remove とも温存)、
  窓は Claude Code 自身のモデル別解決に任せる。
- ローカル実効窓の防衛は合成 400(文言は Claude Code のパース正規表現に
  完全一致)を不変条件として維持。文言はユニットテストでピン留め。
- `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` は維持(ピッカーに有用・
  無害。Claude Code 側の capability キャッシュが有効化された時点で
  ルート対応 /v1/models 広告が窓にも効き始める)。
- Claude Code 側の前提(env 名 2 つ + 400 トリガー文言)は週次の
  `claude-code-canary` ワークフローで継続監視。

### Consequences
- anthropic ルート: `[1m]` モデルの 1M 窓(clientdata チューン込み)が復活。
  窓解決はターン毎なので、`/waired-route anthropic` + `/model` 切替は
  同一セッション内でも次リクエストから追従する。
- waired/auto ルート: 選択モデル相応の窓(200k/1M)を仮定し、超過時は
  400 → 自動要約 → リトライ(従来どおり)。200k 超のローカル窓を非 `[1m]`
  id で使い切れない制約は残る(Claude Code 側の制約、canary で監視)。
- 既存インストールは次回の `waired claude enable` / `waired init` で
  レガシー値が掃除される。

### Refs
- https://github.com/waired-ai/waired-agent/pull/11 (Fixes waired#771)
- waired#623 / waired#621(経緯)、waired-agent#10(testnet ゲート、無関係だが
  同時期に main へ合流)
- docs/knowledges/20260714/0241-claude-code-context-window-internals.md
