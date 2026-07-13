# Decision Log

New entries at the top. Format: see CLAUDE.md §Decision Log.

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
