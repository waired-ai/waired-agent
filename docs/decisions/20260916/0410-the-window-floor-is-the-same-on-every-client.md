---
status: accepted
supersedes:
  - docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md
  - docs/decisions/20260822/2116-plugins-declare-the-window-the-host-reports.md
  - docs/decisions/20260820/0200-model-picker-can-name-a-node.md
---

# モデル行のコンテキストウィンドウの条件は、どのクライアントでも同じ (20260916 04:10)

## Status

Accepted。オーナー決定(2026-09-16、waired-ai/waired-agent#1395)。
次の 3 件を部分的に置き換える。

- `docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md` 決定 4 の「public 行に双子は無い」。
- `docs/decisions/20260822/2116-plugins-declare-the-window-the-host-reports.md` の「プラグインはホストが申告したコンテキストウィンドウを書く」。コンピュータを名指さない行は、ホストの値ではなく行の条件を示す。
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md` 決定 4 の「`RequiredWindowFor` は 0」。peer と public の id は 200,704 トークンの条件を運ぶ。

## Context

Claude Code の routing ページは「Waired の行は 200,000 トークンの会話を保持できるコンピュータだけを使う」と書いていたが、実装はそうなっていなかった。

- コンテキストウィンドウの条件(`Request.MinContextWindow`)は、ウィンドウを宣言していないコンピュータ(0)を通していた。「フィールドより前の agent を切らないため」だったが、そういう agent はもう無い。宣言できる最小のウィンドウより小さいコンピュータ(gpt-oss、32k のモデル)は 0 を公開するので、Waired の行に答えていた。
- `:9473` の `GET /v1/models` は、コンピュータを名指さない行(`waired/default`、`waired/peer`、`waired/public`)に**要求元**のコンテキストウィンドウを `max_input_tokens` として載せていた。ルーティングはそれを守っていない。`[1m]` の双子行も無かった。OpenCode と OpenClaw が読むのはこの一覧なので、Claude Code の `/model` と行が違っていた。
- 固定したコンピュータ(`waired worker set --pin`、または `waired/peer-<name>` の行)がフィルタ(ウィンドウ、Serve main conversation / Serve subagents、Public Share の判定)で外れると、要求は黙ってほかのコンピュータに流れていた。固定が排除するはずの代替そのもの。
- 条件を満たすコンピュータが無いときの `ErrNoEndpointForWindow` は、どちらのリスナーにも自分の arm が無く 500 になっていた。Claude Code は 5xx を 10 回再試行する。
- 双子行は、コンピュータごとの行数を 0 にすると消えていた。公開コンピュータは Public Share がオフでも `Waired peer: <name>` の行を持っていた。

## Decision

オーナー決定(2026-09-16、waired-ai/waired-agent#1395):

1. **OpenCode と OpenClaw が使う OpenAI 互換のリスナーは、Claude Code の `/model` と同じモデル行の仕様に従う。** `(1M context)` の双子行と、200k のコンテキストウィンドウの条件を含む。
2. **OpenCode と OpenClaw では、どのコンピュータでもよい行を `waired/default`、その双子を `waired/default[1m]` として表示し続ける**(既存の設定を壊さない)。プラグインはこれらを Claude Code が送る id `waired` / `waired[1m]` として**送る**。条件を運ぶのはこの id。チャットアプリ(chat-clients ガイド、`waired infer`)が直接送る `waired/default` は今日の挙動のまま: 条件無し。
3. **`waired/peer` と `waired/public`(Claude Code の行 `Waired peer` / `Waired public share`)も 200,000 トークンの条件と `(1M context)` の双子を持つ。** 3 つのクライアントで同じ。条件が無いのは `waired/local`(`Waired local`)と、コンピュータを 1 台名指す行(`waired/peer-<name>`、`Waired peer: <name>`)だけ。これらの行はそのコンピュータ自身のコンテキストウィンドウを示す。

実装者の既定(オーナー決定から導いたもので、決定そのものではない):

- **条件付きの要求に対して 0 は合格ではない。** 宣言しないコンピュータは、条件を満たさないコンピュータと同じに扱う。`proto/signer` の `InferenceState.ContextWindow` の doc も同じ読み方に改めた(条件を持たない読み手、たとえば溢れの検査や表示は、0 を「不明」と読み続ける)。
- **フィルタで外れた固定は、名前を挙げて失敗する。** ウィンドウ、Serve main conversation / Serve subagents のオフ、Public Share の判定のどれで外れても、`waired worker` の固定と `waired/peer-<name>` の行の両方で失敗する(`ErrPinnedPeerDeclined`、両リスナーで 400)。例外は 1 つ: **`waired worker` の固定先が、この版の Waired が知らないモデルを動かしている場合**は、`docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md` のとおりほかのコンピュータに流れる。`waired/peer-<name>` の行は「そのコンピュータだけ」を言っているので、この場合も失敗する。
- **条件を満たすコンピュータが無いときは両リスナーで 400。** 文面は「プロンプトが長すぎる」と読める語を避ける。OpenCode はその語(`packages/llm/src/provider-error.ts` の判定パターン)を見ると会話を要約して再試行するが、この条件はプロンプトの長さでは解けない。新しい文面で OpenCode 1.18.30 がエラーを表示するだけで要約も再試行もしないことを確認した。
- **双子行は、その行に答えられるコンピュータが 1M を宣言しているときだけ出す。** 公開コンピュータもコンテキストウィンドウを公開しているので、public 行も双子を持つ。Serve main conversation をオフにしたコンピュータは双子の根拠にしない。コンピュータごとの行数の上限は双子に影響しない。Public Share がオフの間、公開コンピュータは `Waired peer: <name>` の行を持たない。
- **OpenClaw のプラグインは、行(名前とウィンドウ)が変わったとき、または古い版のテンプレートが書いたものであるときに書き直す。** `waired init`、`waired link all`、`waired doctor --fix` から。`waired doctor` はずれを警告する。
- **`CLAUDE_CODE_MAX_CONTEXT_TOKENS` は今回触らない。** 引き続きこのコンピュータのウィンドウから来る(waired-ai/waired-agent#1396)。
- **Public Share の grant の要求をウィンドウで選ぶ**のはコントロールプレーンの変更で、private リポで追跡する。

## Consequences

- Waired / Waired peer / Waired public share の行で、gpt-oss や 32k のモデルを動かすコンピュータは使われなくなる。そのコンピュータの Waired local と `Waired peer: <name>` の行は今までどおり答える。docs の routing ページの記述が実装と一致する。
- `:9473` の `max_input_tokens` は行が示す値になる: 200704(default / peer / public)、1048576(双子)、`waired/local` はこのコンピュータの値、`waired/peer-<name>` はそのコンピュータの値、宣言が無ければ載せない。OpenCode は起動のたびに読む(`limit.context`)ので、要約の目安が行に合う。
- OpenClaw の refresh の行は `Updated OpenClaw's list of Waired models and their context windows.` になる(旧 `OpenClaw now knows this computer serves N tokens of context.` は、`waired/default` の値がこのコンピュータの値でなくなったので意味を失った)。`waired doctor` の `openclaw context window` の detail に `the plugin was written by an older version of Waired` / `the plugin's list of Waired models is out of date` が加わる。
- Claude Code のステータス行の赤い理由に `the pinned computer cannot take this turn` と `no computer has the context window this needs` が加わる。
- 名指したコンピュータが居ないときの文は `no computer named in "<name>" is on your network right now — restart your coding tool and pick a computer from its model list again` になる(旧文は Claude Code の `/model` を名指していたが、同じ Selector を OpenCode / OpenClaw も通る)。
- 上の製品文字列はオーナーが 2026-09-16 に承認した。変えるときは docs と同じ PR で動かす。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1395
- https://github.com/waired-ai/waired-agent/issues/1396(`CLAUDE_CODE_MAX_CONTEXT_TOKENS` の追随)
- `docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md`
- `docs/decisions/20260822/2116-plugins-declare-the-window-the-host-reports.md`
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md`
- `docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md`
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`
