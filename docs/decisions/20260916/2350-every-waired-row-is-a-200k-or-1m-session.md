---
status: accepted
supersedes:
  - docs/decisions/20260916/0410-the-window-floor-is-the-same-on-every-client.md
  - docs/decisions/20260906/0415-the-declared-window-falls-back-to-a-peers.md
  - docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md
  - docs/decisions/20260822/2116-plugins-declare-the-window-the-host-reports.md
  - docs/decisions/20260820/0200-model-picker-can-name-a-node.md
  - docs/decisions/20260802/0908-claude-window-elevated-cli-only.md
---

# Waired の行はすべて 200k か 1M のセッション。どのコンピュータが答えても変わらない (20260916 23:50)

## Status

Accepted。オーナー決定(2026-09-16、waired-ai/waired-agent#1396)。次の記録を置き換える。

- `docs/decisions/20260906/0415-the-declared-window-falls-back-to-a-peers.md` 全体(エンジンの無いホストは届くピアの最小値を書く)。
- `docs/decisions/20260916/0410-the-window-floor-is-the-same-on-every-client.md` の決定 3 のうち「条件が無いのは `waired/local` と、コンピュータを 1 台名指す行だけ。これらの行はそのコンピュータ自身のコンテキストウィンドウを示す」と、実装者の既定「`CLAUDE_CODE_MAX_CONTEXT_TOKENS` は今回触らない」、Consequences の `max_input_tokens` の値。
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md` 決定 4 の「`RequiredWindowFor` は 0」のうち、0410 が残した `waired/local` と `waired/peer-<name>` の分。
- `docs/decisions/20260822/2116-plugins-declare-the-window-the-host-reports.md` の「プラグインが宣言する窓は、そのホストのゲートウェイが申告した値にする。申告が得られないときは、その項目を書かない」。カタログの id に `max_input_tokens` を載せる部分は残る。
- `docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md` 決定 3 の「managed settings が書くその値はこの機械の窓」。
- `docs/decisions/20260802/0908-claude-window-elevated-cli-only.md` の「値は `/v1/models` の実窓から導く」と、訂正の「`local window:` の STALE 行は恒久的に必要」。書き手を昇格した CLI に限る決定は残る。

## Context

Claude Code には行ごとにコンテキストウィンドウを伝える手段が無い。`anthropic-beta: context-1m-*` を送る `[1m]` の行を除けば、Waired の行はすべて `CLAUDE_CODE_MAX_CONTEXT_TOKENS` の 1 個の値でセッションの大きさが決まる。その値は「このコンピュータのエンジンの窓」(#408)か「届くピアの最小値」(#1246)で、次の問題があった。

- 値はこのコンピュータに正しく、ほかの行には近似でしかなかった。答えるコンピュータが違えば、Claude Code が要約する位置と、実際に保持できる量がずれる。
- モデルを切り替えると値が古くなる。書けるのは昇格した CLI だけで、Claude Code は起動時にしか env を読まない。`waired claude status` の STALE 行はこのずれを見せるためにあった。
- agent に聞けないとき(ウィザードが何も動く前に経路を書く、agent が止まっている)は何も書かれず、Claude Code は既定の 200k と「model catalog にない」の注意書きになった。
- `waired/local` と `waired/peer-<name>` には条件が無く、1M を保持するコンピュータが `[1m]` の付かない行に答えると、ガードは 1M まで通していた。Claude Code が 200k として要約しているセッションに対し、その先を黙って受け入れていた。
- OpenCode と OpenClaw の行は、名指すコンピュータ自身の窓を示していた。窓を宣言しないコンピュータの行には数字が無く、クライアントの既定になった。

## Decision

オーナー決定(2026-09-16、waired-ai/waired-agent#1396):

1. **`[1m]` の付かない Waired の行はすべて 200,704 トークンのセッション。`[1m]` の行は 1,048,576 トークン。** 答えるコンピュータが 1M を保持していても、行の id で決まる。対象は `waired`、`waired/local`、`waired/peer`、`waired/public`、`waired/peer-<name>`、OpenCode と OpenClaw の `waired/default`(送るときは `waired`)と、それぞれの `[1m]`。
2. **`CLAUDE_CODE_MAX_CONTEXT_TOKENS` は 200704 に固定する。**
3. **OpenCode と OpenClaw も同じ設計にする。**

実装者の既定(オーナー決定から導いたもので、決定そのものではない):

- **条件(`RequiredWindowFor`)は Waired の id すべてで 200,704、`[1m]` で 1,048,576。** 名指した行で条件を満たさないコンピュータは、0410 が入れた固定の拒否(`PinnedPeerDeclinedError`、ローカルは `WindowFloorError`)で名前と理由を挙げて失敗する。
- **超過ガードは `min(答えるエンジンの窓, 行の窓)`。** Claude のリスナーは `routeReq.MinContextWindow`(beta ヘッダ込み)、OpenAI 互換のリスナーは `applyRouteDirective` の結果を使う。行でない要求(カタログの id、チャットアプリが送る `waired/default`)は今までどおりエンジンの窓だけで判定する。
- **一覧は行の窓だけを示す。** `:9473` の `max_input_tokens` と Claude のリスナーの一覧の directive id は 200704 / 1048576。コンピュータごとの行は、200,704 以上を宣言するコンピュータにだけ出す(選ぶと失敗する行は出さない、waired-agent#901 と同じ理屈)。id の解決には全ピアを使い続けるので、以前選んだ行は名前を挙げて失敗する。
- **Claude Code のフッターは 200,704 の条件付きで次のターンを問う。** 条件を満たすコンピュータが無いのに緑を出さない。
- **managed settings。** `waired claude enable` は agent に聞かずに `"200704"` を書く。`waired init` / `waired link` / `waired doctor --fix` の top-up は、経路が Waired を向いていて値が `"200704"` でなければ書き直す(古い build が書いた値の移行と、ウィザードが書けなかった値の補填)。所有の判定は `"200704"`、`"250000"`(#408 以前)、古い build がこのホストで導いたはずの値(`waired/default` の `max_input_tokens`、なければ届くピアの最小値)。アンインストールのスクリプト(sh の Python と JXA、ps1)も `"200704"` を Waired の値として認識する。
- **`waired claude status` の行は `context window:`。** 値は常に 200704 で、managed settings が `200704` / 未設定 / ほかの値のどれかを示す。後の 2 つは `sudo waired claude enable` を案内する。経路が Waired を向いていないとき、行がオフのときは出さない。
- **プラグインは窓をキーから決める。** OpenCode は `limit.context`、OpenClaw は `contextWindow`。ゲートウェイの一覧から読まないので、一覧に無いキーや、agent に聞けないときの既定の行でも同じ値になる。OpenClaw の `PLUGIN_REV` は 3。`CONTEXT_WINDOW` は常に 200704 で、`waired doctor` はほかの値や古い revision を警告し、top-up が書き直す。
- **200,704 未満を配信するエンジンは、この PR では変えない。** そういうコンピュータはどの Waired の行にも答えなくなる。エンジンが 200,704 か 1,048,576 だけを配信するようにする変更(vLLM、ollama、宣言、推奨)は別の PR で扱う。コントロールプレーンと NAVI(Public Share のマッチングの既定の条件、表示)は private リポで扱う。

## Consequences

- どのクライアントでも、Waired の行を選べばセッションの大きさが決まる。答えたコンピュータで要約の位置が変わらない。
- `waired claude status` の STALE は、古い build が書いた値か、書かれていない値だけを指す。モデルを切り替えても値は古くならない。
- 200,704 未満を配信するコンピュータ(vLLM が VRAM に合わせてコンテキストウィンドウを縮めたものなど)は、`Waired local` と自分の `Waired peer: <name>` の行でも使えなくなる。
- 1M を保持するコンピュータでも、`[1m]` の付かない行のターンは 200,704 トークンを超えると `prompt is too long` の 400 になり、Claude Code は要約して続ける。
- 製品の文字列: `waired claude status` の `context window:` 行(3 通り)を追加し、`local window:` 行を削除した。ウィンドウの拒否文から `A model row that names one computer does not ask for this.` を削除した。OpenClaw の `waired doctor` の OK の detail `this computer did not report a window; leaving the plugin as it is` と、`waired link` のログの「context window unknown」の分岐を削除した。文言はオーナーが 2026-09-17 に承認した。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1396
- https://github.com/waired-ai/waired-agent/issues/1395
- https://github.com/waired-ai/waired-agent/issues/1246
- https://github.com/waired-ai/waired-agent/issues/408
- `docs/decisions/20260916/0410-the-window-floor-is-the-same-on-every-client.md`
- `docs/decisions/20260906/0415-the-declared-window-falls-back-to-a-peers.md`
- `docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md`
- `docs/decisions/20260822/2116-plugins-declare-the-window-the-host-reports.md`
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md`
- `docs/decisions/20260802/0908-claude-window-elevated-cli-only.md`
