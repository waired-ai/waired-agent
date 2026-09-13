---
status: accepted
---

# OpenCode のサブエージェントはサブエージェントとして扱い、使用中なら待たせる (20260914 03:00)

## Status

Accepted。オーナー決定(2026-09-14、waired-ai/waired-agent#1366 の修正方針の相談で)。

## Context

OpenCode は OpenAI 互換の口(`:9473`)から Waired を使う。0.0.3-rc6 の実機検証
(waired-ai/waired#1361 の記録)で、次のことが分かった。

- OpenCode 1.18.30 は、どのリクエストにも `x-session-id` と `x-session-affinity` を付ける。
  サブエージェントのリクエストにだけ `x-parent-session-id`(起動した親セッションの id)を付ける。
  中継プロキシで採取した。
- `:9473` にはクラスを読む処理が無かった。そのため次のものが OpenCode には効いていなかった。
  - Web コンソールの「Serve main conversation / Serve subagents」
  - sticky id のクラス別の名前空間
  - 使用中のときの待ち
- 待ちが無いことの根拠は、コードのコメントだけだった。
  コメントは「`waired infer` は 1 本ずつしか送らない」「このハンドラはメッシュからの受け口も兼ねる」の 2 点を挙げていた。
  - 前者は、OpenCode が並列にサブエージェントを送るので成り立たない。
  - 後者は、受け口が overlay リスナーの別の HandlerSet なので成り立たない。
  - 決定記録は無かった。
- 実測: ピアだけの行にサブエージェント 6 本を向けたところ、85 秒で 503 が 13 回出て、1 本が失敗した。
- sticky id は本文の最初のメッセージから作られていた。OpenCode は同じエージェントの全セッションを同じ system プロンプトで始める。
  そのため、1 ターンのサブエージェントがすべて同じ会話とみなされていた。

## Decision

1. **`:9473` で OpenCode のサブエージェントをサブエージェントとして扱う。** クラスは次のように読む。
   - `x-parent-session-id` があれば `sub`。
   - `x-session-id` だけなら `main`。
   - どちらも無いクライアント(`waired infer`、チャットアプリ、OpenClaw)は、これまでどおりクラス無し。
     メイン会話もサブエージェントも持たないクライアントを、スイッチの対象にしないため。
2. **「Serve main conversation / Serve subagents」は OpenCode にも効く。** docs と Web コンソールの説明文も「Claude Code と OpenCode」に直す。
3. **使用中のときは待たせる。** 待ち時間は Claude の口と同じクラス別の値(既定でメイン 60 秒、サブ 20 秒)。
   - `TTFBBudget` ではなく `CapacityQueueBudget` で渡す。`:9473` のピア脚に TTFB の打ち切りを足さないため。
   - overlay リスナーには配線しないので、他のパソコンから中継されてきた要求はこれまでどおりすぐ答える。
4. **`x-session-affinity` を sticky id に使う。** `X-Waired-Conversation-Id` の次で、本文のハッシュより優先する。

## Consequences

- OpenCode のサブエージェントは、行が満杯でも最大 20 秒待ってから 503 になる。OpenCode は 503 を自分で再試行する。
- 「Serve main conversation」をオフにしたパソコンは、OpenCode のメイン会話も受けなくなる。
- 公開共有の「メイン会話 / サブエージェント」の許可も、OpenCode のリクエストに効く(`classAllowsPublic`)。
- Claude の口の設定値(`ClaudeTTFBBudgetMainMs` / `ClaudeTTFBBudgetSubMs`)を OpenCode の待ちにも使う。
  名前は Claude だが、「このデプロイが最初のバイトまでどれだけ待てるか」という同じ問いに答える値だから。
  パソコンごとの行数の上限と同じ扱い(`cmd/waired-agent/inference.go`)。
- proto の `ExcludeMain` / `ExcludeSub` のコメントは「Claude Code の」と書かれたまま。
  proto の変更は単独の PR にする規則なので、ここでは触らない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1366
- https://github.com/waired-ai/waired-agent/issues/1354
- `docs/decisions/20260906/0343-subagents-are-placed-by-the-documented-knob.md`(Claude Code 側のクラスの読み方)
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`(待ちがすべての脚に効く理由)
