---
status: accepted
---

# 生産者のいない Claude イベント種別 2 つを廃止する (20260906 04:16)

## Status
Accepted

## Context

`internal/observability/eventring.go` の `KindClaudeNodeChange` と
`KindClaudeNodeFallback` は、`waired-agent#1198`(= `#1184`、auto ルート撤去)以降
**書き手が 1 人もいない**。

- `KindClaudeNodeFallback` の doc コメント自身が、生産者は anthropic ルートの降格
  だったと書いている。その経路は撤去された。
- `KindClaudeNodeChange` はクラス別ルートの遷移を記録していた。クラス別ルートは無い。

リングは共有面なので、外に読み手がいないことを確かめてから決める必要があった
(waired-agent#1246)。

## Decision

**両方を型ごと廃止する。**確認した範囲:

- 公開リポ(`waired-agent`)の Go / スクリプト / docs: 定義そのもの以外の参照ゼロ。
- 私有リポ(`waired`): **コードの読み手ゼロ**。dev-docs が 2 か所で言及するのみ
  (`infra/observability.md` の表、`claude-proxy.md` の現行動作の記述)。

`KindPinnedPeerUnreachable` は残す —— `waired-agent#325` 以降、pin ダウンの唯一の
記録はそちらで、こちらとは別物。

## Consequences

- 私有 dev-docs の該当 2 か所を追随させる。日付入りの過去記録
  (`claude-proxy.md` の rc7 レビュー機の記述、`docs/decisions/20260802/0631`)は
  **凍結**する —— そのとき実際に観測された事実なので。
- 別の遷移(運用者がローカル推論を切る、など)を記録したくなったら、この形を
  復活させるのではなく、そのとき必要な形で起こす。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1246
