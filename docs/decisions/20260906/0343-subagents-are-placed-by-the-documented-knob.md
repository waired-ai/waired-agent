---
status: accepted
---

# サブエージェントは文書化されたノブで配置し、組織管理下の設定は書かない (20260906 03:43)

## Status

Accepted。オーナー裁定
`docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`
裁定 6 の実装(waired-ai/waired-agent#1186)と、その隣で見つかった
managed settings の非対称性の是正(waired-ai/waired-agent#1188)。

## Context — なぜ偽の id が必要だったか

ゲートウェイは「このターンはメイン会話か、サブエージェントか」を知る必要がある
(#645/#646)。クラスはルートではないが、3 つのことを決める: sticky id の名前空間、
ピア脚の TTFB 予算、そして**ルーティング記録の抑止**(サブエージェントのモデルで
`waired claude status` の「最後のターン」を上書きしない)。

Claude Code がそれを言う手段が無いと思われていたので、waired は自分で作った:
machine-wide の managed settings に `CLAUDE_CODE_SUBAGENT_MODEL=waired/subagent`
を書き、全サブエージェントにその id を持たせ、ゲートウェイは id を読んでいた。
代償として、その id は実在のモデルではないので、**Anthropic へ出ていく脚では
本文の `model` を実在の id に書き換える**必要があった —— waired が「ユーザーが
打っていないモデル id をワイヤに置く」唯一の場所で、#1036 はまさにその機構が
ホストを丸ごと詰まらせた事故である。

## Decision

1. **クラスはヘッダから読む。** Claude Code はセッション内で自分が起こした
   エージェントのリクエストに `x-claude-code-agent-id` を付ける
   (https://code.claude.com/docs/en/llm-gateway-protocol#request-headers、
   「present only on requests from an agent Claude Code spawned inside the
   session」)。2.1.261 で実測(2026-09-06): メインのターンにもタイトル生成にも
   付かず、サブエージェントのリクエストにだけ付く。トップレベルの
   サブエージェントには親 id ヘッダは付かない。

   **ラベルより厳密に良い**: 定義が自分でモデルを pin しているサブエージェントは、
   その id で届くのでラベルでは分類できなかった。ヘッダは付く。

2. **`CLAUDE_CODE_SUBAGENT_MODEL` は本来の意味に戻す** —— サブエージェントを
   どこで走らせるかの選択。waired が書くのは操作者がそう選んだときだけで、
   書き先は**その人自身の** `~/.claude/settings.json`(昇格不要、組織が全員に
   設定した値を上書きしない)。

3. **値は 2 つだけ**: `follow`(何も書かない。各サブエージェントは Claude Code が
   解決した id で走る)/ `waired`(全部 Waired)。「メインは Waired・サブは
   Anthropic」は 3 つ目の値にしない —— それが欲しい人はエージェント定義に実在の
   Anthropic モデルを pin する。文書化された方法で、既に動く。

4. **`waired` を選んだら `CLAUDE_CODE_SUBAGENT_MODEL_FORCE=1` も書く。**
   2.1.261 で実測: 定義が `model: claude-opus-4-8` を pin し env が `waired` の
   とき、`_FORCE` 無しではサブエージェントのリクエストは `claude-opus-4-8` で
   届き、`_FORCE=1` では `waired` で届く。片方だけ書くと「サブエージェントを
   Waired に」が、**わざわざモデルを指定されたエージェントにだけ静かに効かない**
   ことになる。

5. **passthrough の model 書換を撤去し、退役した cloud 行は fail-closed にする。**
   書換の最後の利用者は `claude-waired-cloud[1m]` だった。この行は #1037 で
   広告から外れており、中継するには本文の model を別の id に書き換えるしかない。
   中継をやめて、この機械で答えて直し方を名指しする。結果として
   **passthrough の本文はクライアントのものがそのまま出る** —— ゲートウェイ契約が
   求める "inspect without modifying" と一致する。

6. **組織が管理している Claude Code には書かない**(#1188)。次のいずれかが
   managed settings に在れば、`waired claude enable` は**書かずに止めて**何が
   見つかったかを印字する: `forceLoginOrgUUID` / `forceLoginMethod` /
   `forceLoginGatewayUrl` / `availableModels` / `modelPicker` / loopback でない
   `ANTHROPIC_BASE_URL`。

   これは体裁の問題ではない。非既定の `ANTHROPIC_BASE_URL` は
   「server-managed settings が**迂回される**方法」として Security considerations
   に列挙されている(https://code.claude.com/docs/en/server-managed-settings) ——
   つまりこの書込は、その機械の全セッションに対して組織が配っているポリシーを
   黙って切る。インストーラが下してよい判断ではない。

   **「それでも書く」経路は用意しない。**上書きされるものが、持ち主が「いやだ」と
   言うための機構そのものだから。代わりに、機械全体に触らない道を案内する
   (`waired link --list`)。

   これは元々あった非対称性の是正でもある: `Remove` は loopback 接頭辞を持つ
   `ANTHROPIC_BASE_URL` しか消さない(操作者のゲートウェイを消さない)のに、
   `Write` は無条件に上書きしていた。両方向が揃った。

## `managed-settings.d/` ドロップインは採らない

#1188 は「waired のキーを `managed-settings.d/50-waired.json` に移せば
アンインストールがファイル削除になり、組織のファイルを一度も編集しない」案の
調査も求めていた。**採らない**、理由は 2 つ:

- **問題を解かない。**ドロップインは `managed-settings.json` の**後**に辞書順で
  マージされ、`env` はキー単位、`modelPicker` は「後の lineup が丸ごと置き換える」
  (https://code.claude.com/docs/en/managed-settings)。つまり `ANTHROPIC_BASE_URL`
  も組織の picker lineup も、ドロップインから同じように上書きできる。
  上の検出が先で、それが済めば書き先がどちらでも結果は変わらない。
- **今より壊れやすい。**ドロップインを読む最低版が上がり(現行の最低要件より
  新しい)、それを満たさないホストでは waired の設定が**黙って効かない** ——
  今日の「1 つのファイルにマージし、`waired claude status` が読み返す」形は、
  効いていないことが見える。

再検討する条件: 組織のファイルを一切開かずに書けることが、検出だけでは足りない
形で必要になったとき(例: 監査要件でファイルの mtime を触れない)。

## Consequence

- `waired claude subagents [follow|waired]` が増える。引数なしで現状を報告する。
- `waired claude status` に `subagents:` 行が増える。
- 旧ラベル `waired/subagent` は `Remove` と `Write` の両方が回収する。
  操作者が自分で選んだ subagent モデルはどちらも触らない。
- e2e の `claude-unresolvable-id` レグは駆動値をラベルから
  `acme-labs/coder-v2` に替え、**クラスそのものの e2e を
  `claude-subagent-class` レグとして新設**した。ラベルが唯一のクラス経路
  だったので、替えるだけだとクラスの e2e カバレッジが 0 になっていた
  (`docs/decisions/20260829/1655` §4 が #600 について同じ指摘をしている)。

## Refs

- 実測: `docs/knowledges/20260906/0350-the-subagent-header-and-the-force-flag.md`
- https://code.claude.com/docs/en/sub-agents
- https://code.claude.com/docs/en/llm-gateway-protocol#request-headers
- https://code.claude.com/docs/en/server-managed-settings
- https://code.claude.com/docs/en/managed-settings
- waired-ai/waired-agent#1186, #1188, #1036, #1037; waired-ai/waired#645, #646, #1313
