---
status: accepted
---

# `/model` の public share 項目は絞るだけで、姿勢を上書きしない (20260820 05:00)

## Status

Accepted。オーナー裁定（2026-08-20、waired-ai/waired-agent#901 の3つの設問に対する回答）。
`docs/decisions/20260820/0200-model-picker-can-name-a-node.md` の「項目はノードを
名指してよい」を、ノードの**クラス**（他人のマシン）にも適用する。Public Share の
同意機構（spec §4.2）には手を入れない。

## Context

peer 別エントリ（waired-agent#830）を実装する際に「Public Share のマシンも 1 台
1 行で出すか」を確認したところ、オーナーの回答は:

> public share という項目だけつくってはどうか。ノード単位はいらない。

既存の `PublicMode` は off / auto / explicit の3値で、いずれも**利用者の常時姿勢**
（`agentconfig.PublicUse.EffectiveMode` がルータより手前で解決する）。
`buildMeshCandidates` は自分のノードと public を混ぜて自分優先で並べるので、
「public のみ」という状態は存在しなかった。

## Decision

1. **`claude-waired-public` を 1 項目だけ足す。** peer 別行と違い、マシンごとの行は
   作らない。ラベルは `Waired public share (someone else's computer)`
   （オーナー承認、2026-08-20）。

2. **同意していないホストには出さない。** picker は「選べない行」を表現できない
   （実測: gateway 由来の行はすべて `From gateway` 固定で描画される）ので、選択肢は
   「出して選んだら失敗させる」か「出さない」の2つしかなく、後者を採る。
   management の `EffectiveMode` が既に「現行の警告文への同意が無ければ off」を
   意味するので、判定は1フィールドで済む。あとから同意したホストは、次の `claude`
   起動で拾う（SessionStart フックが毎回作り直すため）。

3. **常時姿勢を上書きしない。** この項目は候補集合を**絞るだけ**で、広げない。
   姿勢が `auto` なら tier 比較はそのまま効き、他人のマシンが自分のハードウェアを
   上回らなければ選ばれない。「明示的に選んだのだから比較を飛ばす」という解釈は
   採らなかった。同意の射程は姿勢そのものであって、個々のリクエストではない。

4. **見送られたときは2つの理由を区別する。** 「届く public マシンが1台も無い」と
   「姿勢が auto で、どれも自分を上回らない」は別の事実で、後者は設定どおりに
   動いている状態。これを可用性の問題として報告すると障害に見える。判定には
   既存の `snapshotHasPublicProvider` と `publicShortfall` が持つ gate を使う。

5. **ルータ側は `Inputs.PublicOnly` の1フィールド。** `state.RoutingMode` に6番目の
   値としては足さない。`RoutingMode` は**永続化される**オペレータ設定
   （`<state-dir>/runtime/desired-worker`、tray・`waired worker`・management API が
   読む）であり、1リクエストの選択をそこに置くべきではない。

## Consequences

- Public Share が off のホストでは、この項目はキャッシュに書かれない。`/model` の
  Waired 行数はホストの状態で変わる（engine-less なら local も落ちる）ので、
  docs から件数のハードコードを外した。
- picker キャッシュの書き手が管理 API をもう1本読む（`/waired/v1/public/use`）。
  ソケット経由・失敗時は「出さない」に倒す。local 行とは逆向きなのは意図的で、
  出せなかった local は選択肢を1つ失うだけだが、同意していないのに出た public は
  してはいけない提案をすることになる。
- `PublicOnly` は絞る方向にしか効かないので、この項目から同意していないマシンへ
  到達する経路は構造上存在しない。テストで固定してある。

## Refs

- waired-ai/waired-agent#901 / waired-ai/waired-agent#830 / waired-ai/waired#1227 レーン L64
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md`
- `docs/knowledges/20260820/0300-model-picker-measured-on-device.md`
