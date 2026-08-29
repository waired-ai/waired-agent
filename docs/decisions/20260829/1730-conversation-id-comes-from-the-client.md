---
status: accepted
---

# 会話 ID は本文の先頭バイトではなくクライアントの識別子から取る (20260829 17:30)

## Status
Accepted

## Context

sticky ルーティングが KV 親和性を結ぶ「会話の同一性」は、本文の先頭 1 KiB の
SHA-256 だった。実 `claude` CLI を Anthropic 形の捕捉エンドポイントに向けて
計測したところ、**両方向に壊れていた**（waired-agent#1125）。

- 同一リポジトリで開いた 2 セッションが**同じ id**。最初に食い違うバイトは
  6,628 で、窓の 6.5 倍先。手前は全部が同一の system-reminder ブロックだった
- 1 セッションの 2 ターンが**別の id**。最初に食い違うバイトは 17 で、`model`
  の値の中。`model` はワイヤ上の第 1 キーなので、**どんな窓幅でも必ず入る**

後者はこの製品では仮定の話ではない。`claude-waired-auto` / `-peer` / `-local` /
`-cloud` は**モデル ID としてクライアントに配られている**（#830, #1036）ので、
`/model` で別の項目を選ぶと束縛が落ち、プレフィックスの再構築を買う。実測で
追記ターン 2.57 s に対しプレフィックス喪失 35.38 s、コールド 33.85 s。

`applyStickyFirst` は束縛されたピアを**順位を一切見ずに** index 0 へ持ち上げる
ので、この id は選択でもっとも重い事実である。

## Decision

解決の連鎖を「ヘッダ → クライアントの識別子 → 本文プレフィクス（最後の手段）」
にする。識別子の段は**クライアント自身の user id と最初のメッセージの両方**を
混ぜる。

片方だけでは足りず、互いの穴を埋めるため:

- **user id だけでは併合しすぎる。** `metadata.user_id`（Anthropic）も `user`
  （OpenAI）も仕様上は**人**の識別子であり、会話の識別子ではない。Claude Code は
  たまたま値の中に session id を入れているが、安定した per-user 文字列を送る
  クライアントでは全会話が 1 台に潰れる
- **最初のメッセージだけでも併合しすぎる。** 全会話を同じ前置きで始める
  クライアントで分離できない

両方を混ぜると、同一リポジトリの 2 セッション（最初のメッセージが 6,628 バイト目で
分岐する）を分離し、1 セッションをターンをまたいで保つ（最初のメッセージは
バイト単位で同一、user id は `--continue` をまたいで安定）。

**`model` はハッシュに入れない。**

## Consequences

- どちらの面でも**新しい全文パースを増やさない**。両ハンドラは id を求める前に
  本文をデコード済みなので、`StickyIdentity` がそのデコード結果を運ぶ。OpenAI 側の
  read-only 抽出は明示的な `decodeJSONObject` になり、その map が model フィールドと
  識別子の両方を供給する
- 素材は `ComputeStickyID` の外に出ない。Claude Code の `metadata.user_id` は
  device / session の識別子を含むので、保存・ログに残るのは従来どおりダイジェスト
  のみ。テストのフィクスチャは**実捕捉ではなく計測した形の合成**（このリポジトリは
  公開）
- 何も使える識別子を出さないクライアントは従来の本文プレフィクスに落ちる

## Refs
- waired-ai/waired-agent#1125, #1129, #830, #1036
- docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md
- docs/knowledges/20260828/0300-what-claude-code-puts-on-the-wire.md
