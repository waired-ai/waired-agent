---
status: accepted
---

# コーディングツールのプラグインは、ホストが申告した窓を書く。分からなければ書かない (20260822 21:16)

## Status
Accepted

## Context

OpenClaw プラグインのテンプレートは、コンテキスト窓を
`contextWindow: 32768` という**リテラル**で宣言していた
(`internal/integration/openclaw/templates/index.mjs.tmpl`)。ホストにも
モデルにも一切依存しない固定値で、`renderEntry` が差し替えていたのは
baseURL だけだった。

**実機で代償が出た**(2026-08-22、Linux system service + Windows system
service の2台、実バイナリ `openclaw@2026.7.1-2`)。このホストの
ゲートウェイが申告する `waired/default` の窓は **200704**
(`:9472/v1/models` の `max_input_tokens`)。プラグインが 32768 と言うので、
**OpenClaw は全セッションの1ターン目で auto-compaction を起こす**。
同一ホスト・同一プロンプト・毎回新規セッション・両方向で A/B:

| 宣言値 | :9479 へのリクエスト数 | compaction |
|---|---|---|
| 32768(出荷値) | **3** | `reason=threshold` / `compactionCount=1` |
| 262144(手編集) | **1** | なし |

Windows でも同一(waired-agent#1001)。compaction は**文脈を捨てる**ので、
コストは往復2回だけではない。

これは **#408 と同じ型**である。`internal/integration/claudemanaged/managedsettings.go`
の冒頭がその裁定を記録している:

> That value is the window this host can ACTUALLY serve, not a claim (#408):
> the caller resolves it from the gateway (`Deps.ContextWindowFor` — min of
> the manifest's native window and the tuning the engine really applied) …
> Before #408 it was a static 250000, which promised ~256k on hosts serving 32k

3つの連携が同じ問いに3通り答えていた: claude-code=ホストごとに実測 /
opencode=**何も宣言しない** / openclaw=**静的 32768**。

## Decision

**プラグインが宣言する窓は、そのホストのゲートウェイが申告した値にする。
申告が得られないときは、その項目を書かない。**

- データプレーンの `GET /v1/models`(`internal/gateway/openai.go`)に
  **`max_input_tokens` を載せる**。Anthropic 側の一覧が既に同じ
  `Deps.ContextWindowFor` から同じ名前で載せているので、フィールド名も
  出所も揃える。OpenAI のモデルオブジェクトには無い項目だが、知らない
  クライアントは無視する。**解決できないときは `omitempty` で消す** —
  読み手が「分からない」と実数を区別できなければならず、0 は実数に見える。
- `waired link openclaw` が Apply の中で**その値を引き**、テンプレートに
  焼き込む。プラグインの `resolveDynamicModel` は同期関数なので、実行時に
  取りに行く形は取れない。
- **引けなかったら 0 を渡し、プラグインは `contextWindow` を出さない**。
  ウィザードは何かが serving する前に統合を適用するし、`waired link` は
  agent が落ちているホストでも走る。**間違った数字は、無いより悪い**。
  リンク自体は失敗させない(`fetchContextWindow` はエラーを返さない)。

`:9472` の既存の解決器(`cmd/waired/claude.go` の `claudeLocalContextWindow`)を
呼ぶ案もあったが、採らなかった。openclaw の経路から Claude ゲートウェイの
ポートを参照することになり、結合が不自然になる。データプレーン URL は
アダプタが**既に持っている**。

## Consequences

- 窓が変わる(モデルを切り替える、チューニングが変わる)と、プラグインの
  値は**次に link するまで古いまま**になる。#408 が Claude 側で受け入れた
  のと同じ性質で、書き手は常に CLI であり daemon ではない(waired#935)。
  tray の「Reconfigure…」行が `waired link openclaw` を撃つので、更新の口は
  既にある。**ずれを検出して doctor に出す**のは今回入れていない。
- 新規インストール直後は 0(＝宣言なし)になりやすい。OpenClaw の既定に
  委ねる形で、出荷値より悪くなることはない。
- OpenCode 側のプラグインは今も窓を宣言しない。同じ口から値を取れるように
  なったので、必要になれば同じ形で足せる。
- `maxTokens: 8192` は同じリテラル群の中にあるが、今回は測っていないので
  触っていない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1001
- https://github.com/waired-ai/waired-agent/issues/408 (静的な窓を実測値に置き換えた先例)
- https://github.com/waired-ai/waired/issues/518 (実バイナリ e2e の go/no-go — 今回の A/B はその材料)
- docs/decisions/20260822/2030-integration-backs-up-only-a-real-change.md
