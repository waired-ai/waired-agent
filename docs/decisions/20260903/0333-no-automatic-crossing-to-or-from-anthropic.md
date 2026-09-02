---
status: accepted
supersedes:
  - docs/decisions/20260828/0221-auto-mode-classifier-goes-to-anthropic.md
  - docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md
  - docs/decisions/20260828/0143-peer-leg-waits-while-the-peer-works.md
  - docs/decisions/20260820/0400-picker-cache-refreshes-on-session-start.md
---

# ターンは自分の側を離れない: Waired と Anthropic の間の自動遷移を撤去する (20260903 03:33)

## Status
Accepted。オーナー裁定（2026-09-03、waired-ai/waired#1313 レーン L97）。上の 4 件は
いずれも**部分的に**超えられ、どの部分かは各記録の `## Status` に書いた。この記録は
挙動の裁定だけを載せる。背景にある Anthropic 公式 docs との突き合わせと評価の全文は
private 側の記録（`waired/docs/decisions/20260903/0333-claude-follows-documented-gateway-paths.md`、waired-ai/waired#1313）にある。リポジトリをまたぐ
supersede は guard が front-matter で表現できないので、この対応は散文で書く。

## Context

Claude ループバックゲートウェイ（`:9472`）には、waired が失敗や不達を判断して
ターンの側を移す経路が 2 つあった。

- `auto` ルート（既定、`/model` の `Waired auto — 200k / 1M` 行）: ローカル脚が最初の
  1 バイトを出す前に失敗すると実 Anthropic API へ再送し、agent が未配線・degraded なら
  素通しする。SSE 応答に通知の text block を splice し（waired-ai/waired#757）、Stop フックが
  `systemMessage` を出す。
- `anthropic` ルート: 上流に接続できないときだけローカルモデルへ降格し、応答の
  `model` は名指しされた id をエコーする（waired-ai/waired#665）。

`docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md` は
「名指したモデルがそのターンの実行先」と裁定した。`auto` はどこで動くか言わない唯一の
行で、この裁定と整合しない。オーナーの所感（2026-09-03）: waired を使うつもりなのに
気づかないうちに Anthropic に行く感覚のずれ、"auto" の意味の不明瞭さ（配置プロファイル
の Auto、Claude Code 自身の auto mode と衝突）、往復で Anthropic のプロンプトキャッシュ
とローカルの KV の両方が冷えること。

Claude Code の公式 docs は、gateway が Anthropic 宛の本文を改変しないこと
（"inspect without modifying"、"forward error response bodies unmodified"）を求め、
gateway 自身がエラーを返すときの `capability_rejected:` トークン、サブエージェントの
モデルを決める `CLAUDE_CODE_SUBAGENT_MODEL` と `_FORCE`、`/model` の行を並べる
`modelPicker` 設定を文書化している
（https://code.claude.com/docs/en/llm-gateway-protocol 、
https://code.claude.com/docs/en/sub-agents 、
https://code.claude.com/docs/en/settings-reference#modelpicker）。

## Decision

1. **ターンは名指された側で完結する。** Waired の行を選んだターンは Waired のノードで
   答えるか、理由付きで失敗する。Anthropic のモデルを名指したターンは Anthropic へ
   透過し、上流のエラーはそのまま中継する。waired が失敗や不達を判断して側を移す経路は
   持たない。
2. **`auto` ルートを撤去する。** post-dispatch フォールバック（`fallback.go`）、SSE への
   reroute notice（`reroute_notice.go`）、Stop フックの fallback 通知、
   `X-Waired-Fallback` / `X-Waired-Fallback-Allowed` / TTFB 予算のヘッダ、
   `last_fallback`、agent 未配線時の fail-open 素通しは、いずれも撤去する。
3. **`anthropic` ルートの上流不達時ローカル降格（waired-ai/waired#665）を撤去する。** auto mode の
   許可分類器も例外にしない。不達は Claude Code に届くエラーである。
4. **fail-closed のエラーは Claude Code がリトライしない 4xx で返す。** 何が答えられ
   なかったか（この端末 / peer の表示名 / peer 無し）と出口（`/model` で Anthropic の
   モデルを選ぶ、`waired doctor`）を名指す。5xx は Claude Code が最大 10 回リトライして
   から表示する（https://code.claude.com/docs/en/errors#automatic-retries）ので、今日の
   503 は使わない。
5. **`/model` の Waired 行は `Waired`（どの自分のノードでもよい）/ `Waired local` /
   `Waired peer`（+ per-peer、public）。** `auto` と cloud の行は消える。行は
   `modelPicker` 設定で出し、Claude Code の private cache
   （`~/.claude/cache/gateway-models.json`）は書かない。
6. **サブエージェントの配置は 1 スイッチ。** 「追従」（未設定。サブエージェントは
   Claude Code が解決した id を運び、その id が言う側で動く）か「Waired」
   （`CLAUDE_CODE_SUBAGENT_MODEL=<Waired の any-node id>` + `_FORCE=1` をユーザー自身の
   settings に書く）。`waired/subagent` ラベルと passthrough の model 書換は撤去する。
   「main は Waired、sub は Anthropic」はスイッチにせず、agent 定義で実 Anthropic の
   モデルを pin する文書化された方法に委ねる。
7. **非 Anthropic 脚へ渡すリクエストは許可リストで新規に組み立てる。**
   `Authorization` / `x-api-key` / `Cookie` / `Proxy-Authorization` は engine・peer・
   ログ・観測リングに到達しない。実送信で既に落ちている事実をテストで固定する。
8. **gateway が Claude Code に向けて作るエラーは文書化されたトークンで書く。**
   窓超過の 400 は `capability_rejected: prompt_too_long` を運び、Anthropic の文言は
   写さない。

## Consequences

- Waired の行のセッションでローカル・ピアが落ちていると、ターンは即座に理由付きで
  失敗する。以前は黙って Anthropic へ行っていた。復旧するか `/model` を切り替えるのは
  ユーザー。
- 画像貼付や未対応の要求形（ローカルの 400）は Anthropic へ透過されず、エラーになる。
- `Waired auto — 1M` 行が消えるので、`[1m]` 由来の窓の要求は無くなる。窓超過は
  per-request の 400（8）が担う。
- 待ちのポリシーは残る: ローカル脚の keepalive（`20260821/2142`）、ピア脚の稼働監視
  （`20260828/0143`）。打ち切りの先が「Anthropic へ再送」から「理由付きのエラー」に
  変わる。auto 行が消えるので、全ローカル脚が「逃げ場のない脚」になり、keepalive の
  排他規則は単純化する。
- セッションが片側に固定されるので、Anthropic のプロンプトキャッシュもローカルの KV も
  温まったまま。
- `waired claude route`、`/waired-route`、tray の main 3 行 / sub 4 行、statusline の
  fallback 表示、e2e ハーネスの「`api.anthropic.com` を blackhole して escape を捕まえる」
  レーンの意味が変わる。番人の主張は「Anthropic のモデルを名指したターン以外は端末を
  離れない」になる（`20260829/1655` の `last_request_route` 観測はそのまま使える）。
- 前提が変わる既存 issue: #1168 / #1171 / #1180（fallback 前提）。
- 実装は waired-ai/waired-agent#1183 / #1184 / #1185 / #1186 / #1187 / #1188。
  それぞれのテストとコメントは本記録を批准元として引く。

## Refs

- waired-ai/waired#1313（トラッキング）/ #1314（記録の改訂）
- `docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md`（整合する裁定）
- `docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md`（peer-only の fail-closed。整合）
- `docs/decisions/20260829/1655-the-sentinel-observes-the-decision.md`（番人の観測点。整合）
- https://code.claude.com/docs/en/llm-gateway-protocol / https://code.claude.com/docs/en/sub-agents / https://code.claude.com/docs/en/settings-reference#modelpicker / https://code.claude.com/docs/en/errors#automatic-retries
- private 側の対: `waired/docs/decisions/20260903/0333-claude-follows-documented-gateway-paths.md`
