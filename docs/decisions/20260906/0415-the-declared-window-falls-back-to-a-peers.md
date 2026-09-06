---
status: accepted
---

# 宣言する窓は、エンジンが無ければ届く窓に落とす (20260906 04:15)

## Status
Accepted

## Context

managed settings が持つ `CLAUDE_CODE_MAX_CONTEXT_TOKENS` は**セッションに 1 個**で、
`waired-agent#1185` 以降は Waired の `/model` 行すべてに効く。行ごとの窓は違う ——
`Waired local` はこのホスト、`Waired peer` / `Waired peer: <name>` /
`Waired public share` は相手のホストの窓。

Claude Code には**行ごとに窓を宣言する文書化された手段が無い**(`behavesAs` は
「この版が知らないモデルに、知っているモデルのクライアント側の扱いを当てる」もので
別物、かつ公開 settings リファレンスに無い)。

さらに、エンジンを持たないホストでは `LocalContextWindow` が 0 になり、**変数自体が
書かれない**。その結果:

- Claude Code は自分の既定 200k を仮定する
- Waired の全行に `"<id>" isn't described by this version's model catalog` の注意書きが出る
  (2026-09-06 実測: この変数があると注意書きは消える)

エンジン無しホストが出す行は**すべてピア行**なので、そこでピアの窓を書くことは
「もっと良い数の近似」ではなく、**手に入る唯一の正直な数**である(waired-agent#1246)。

## Decision

1. **一般の場合は現状維持。**エンジンのあるホストは、これまでどおり自分の窓を書く。
   最も使われる行(`Waired local`)にとって正確で、他の行に対する過大宣言は
   ゲートウェイ自身の 400 が受け止める —— `waired-agent#1187` で
   `capability_rejected: prompt_too_long` に統一済み。変数は compaction のヒントであって
   実際に断る側ではない(`docs/decisions/20260714/0241-drop-static-auto-compact-window-pin.md`)。
2. **ローカルの窓が 0 のときだけ、到達可能なピアの窓に落とす。**
   `WriteOptions.PeerContextWindow` / `RemoveOptions.PeerContextWindow` を足し、
   両者が `DeclaredContextWindow()`(local > 0 なら local、でなければ peer)を使う。
3. **落とすときは最小値を採る。**この数は 1 セッションを決めるのに、対象は複数の
   コンピュータ。最大を宣言すると、ゲートウェイに断られてから初めて compaction が
   走るという、この変数が避けるための唯一の帰結を招く。
4. **1 つも分からなければ今までどおり何も書かない。**
   `claudeLocalWindowFromModels` と同じ規則 —— 昇格したプロセスが Claude Code に
   窓を教える判断なので、当て推量より辞退が良い。
5. **scrub は同じ数を認識する。**`waired claude disable` の所有権判定も
   `DeclaredContextWindow()` を使う。でなければピア由来の値が残り、
   `waired-agent#1174` が警告している「Waired を消したあともセッションを操り続ける値」に
   なる。

## Consequences

- エンジン無しホストで注意書きが消え、200k の思い込みが実際に届く窓に置き換わる。
- ピア行の窓は依然として近似。過小側に倒しているので、断られてから縮むのではなく
  早めに縮む方向に外れる。
- ホストの構成が変わると宣言値は次の `waired claude enable`(あるいは init の
  仕上げの top-up)まで動かない。既存の挙動と同じで、変えていない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1246
- docs/decisions/20260714/0241-drop-static-auto-compact-window-pin.md
