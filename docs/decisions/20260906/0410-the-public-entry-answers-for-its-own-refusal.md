---
status: accepted
---

# Public Share の項目は自分の拒否に自分で答える (20260906 04:10)

## Status
Accepted

## Context

`/model` の **Waired public share** を選んだターンが断られたとき、返る文は 3 通りしか
無かった(`internal/router/public_candidates.go` の `peerOnlyMissNote`)。一方
`publicGateFor` は 4 つの原因を同じゼロ値に潰していた —— policy 未配線 / モード off /
未同意 / トラフィッククラスのトグル off。

結果として、運用者自身が Public Share を切っているホストで返っていたのは
`no public machine is reachable right now` だった。posture が off のあいだ grant
取得器は保持中の grant をすべて解放する(`cmd/waired-agent/public_grants.go`)ので、
地図に provider が 1 台も居らず、到達性の枝に落ちる。曖昧なのではなく**偽**である。

公開 docs は「この項目は見送ることがあり、そのとき 2 つの理由のどちらだったかを伝える」
と約束していた。off はその 2 つのどちらでもない第 3 の状態で、しかも 1 つ目を誤って
名乗っていた(waired-agent#1201、rc5 所見 F7、record waired-ai/waired#1309)。

加えて 2 点:

- 断り文は `No mesh peer is available (routing=public-share-only: …); local state="ready".`
  の**括弧の中**に入っていた。見出しは mesh の話で、`routing=` と `local state=` は
  Waired の内部語であり、しかも `local state` は**この経路が使わないと決めた機械**の状態。
- 運用者の `waired worker set --min-model-size` がこのホストのエンジンを落としている場合、
  `SizeFloorError` の包みが先に立ち、ゲートウェイは `waired_model_too_small` を名乗って
  そのコマンドを案内していた。public 専用ターンはローカルエンジンを最初から使わない。

## Decision

1. **4 つの原因を言い分ける。** `publicGate` に `denial` を持たせ、`publicGateFor` が
   どのスイッチで断ったかを記録する。**同意をモードより先に見る** ——
   `agentconfig.PublicUse.EffectiveMode` が未同意を既に off に畳んでいるため、
   逆順だと未同意が永久に名乗れない。理由は**その試行が実際に使った gate** から取る
   (policy は atomic pointer で差し替わるので、記録と出力の間に設定変更が挟まると
   原因でない posture を説明してしまう)。
2. **設定の枝を到達性の枝より先に置く。** これが欠陥そのもの。到達性は auto の比較より
   先のまま(貸し手ゼロなら比較は走っていない)。
3. **消費側のサイズ下限を別に数える。** `PublicPolicy.MinModelSize` は
   `waired public use --min-model-size`、運用者の `Inputs.MinModelSize` は
   `waired worker set --min-model-size`。別の設定・別のコマンドなので `belowFloor` に
   混ぜない。`SizeFloorError` の起動条件にも入れない。
4. **文言にコマンド名を入れる**(オーナー裁定 2026-09-06)。語彙は
   `waired public status` が既に出しているもの(`Use public nodes` / `Consented` /
   `Smallest model accepted` / `Main agent` / `Sub agents`)だけを使い、新語を作らない。
5. **public 専用の見出しを立てる。** `ModelNotReadyError` に `PublicShare` を足し、
   `Error()` は `Waired public share declined this turn: <理由>` を返す。
   `routing=` と `local state=` はこの経路からのみ消える —— waired-agent#828 が
   local state を入れたのは別の枝で、そちらは変えない。
6. **public 専用ターンでは運用者の routing floor で包まない**(オーナー裁定 2026-09-06)。
   waired-agent#1128 の「floor 最優先」をこの 1 ケースだけ狭める。ローカルエンジンに
   かかる下限は、ローカルを使わないと決めたターンの拒否理由ではない。

## Consequences

- `/model` の public 行を選んで断られた人は、自分のどの設定が断ったかと、それを
  変えるコマンドを 1 文で受け取る。
- 公開 docs の「2 つの理由」は事実でなくなるので、`guides/claude-code.mdx`(+ja)を
  書き替える。docs は製品文字列を逐語で引く規則なので、両者は同じ PR で動く。
- `waired-agent#1128` の裁定は生きているが、public 専用ターンという例外を持つ。
  この 2 つの順序は `internal/router/public_refusal_note_test.go` の
  `TestPublicShareRefusal_OutranksTheOperatorFloor` が固定する。
- 未読の `public_use.json` も「未同意」と名乗る(ゼロ値の policy が publish される
  ため)。daemon 側は真因をログに出しており、行き先のスイッチはどちらでも同じなので
  記録するにとどめる。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1201
- https://github.com/waired-ai/waired-agent/issues/1252
- docs/decisions/20260820/0500-public-share-in-the-picker-narrows-only.md
