---
status: accepted
---

# /healthz プローブは Selector の順位を尊重する（到着順で勝者を決めない） (20260805 17:03)

## Status

Accepted

## Context

waired-ai/waired#828（並行 sub-agent の分散）に着手して分かったこと。

`ParallelProbe`（`internal/gateway/probe.go`）は `SelectK(k=probeFanoutK=3)` の候補
全部に同時に `/healthz` を投げ、**最初に到着した ready** を勝者にしていた。

```go
for sig := range sigs {
    results[sig.idx] = sig.r
    if winnerIdx < 0 && sig.r.IsReady() {
        winnerIdx = sig.idx
        cancel()
    }
}
```

候補は全部同時に probe されるので、ピアが 3 台以下のメッシュでは
`sortMeshCandidates` が作った順位は結果に一切影響しない。捨てられていたのは
sticky 束縛だけではない:

- admin routing priority（High > Middle > Low）
- catalog score
- #532 で配線したばかりの `ErrorWindow` の失敗率タイブレーク
- weighted-least-loaded の `loadFraction`

つまり、**勝者は毎回 `/healthz` の応答が最も速かったピア**だった。同程度のピアが
並んでいる場合、これはターンごとのコイン投げになる。

実測（`internal/gateway/sticky_spread_e2e_test.go` を旧規則に戻して実行、3 ピア）:

- 同一会話の**逐次 6 ターン**が 3 ピアに散る（`peer-A:1 peer-B:1 peer-C:4`）。
  KV プレフィックス親和性という sticky routing の存在理由が成立していない。
- 無関係な**並行 3 本**が 2 ピアに偏る（`peer-A:2 peer-B:1`）。1 台が遊ぶ。

既存の `TestPhase7Integration_StickyAffinityHoldsAcrossManyRequests` が緑だったのは、
それが Selector の `Select`(k=1) を直接叩いており、gateway の probe 競争を通って
いなかったため。`probe_test.go` の `TestParallelProbe_StickyOrderingRespected` は
見出しコメントで「position-0 が勝つ」と書きながら本体は `winner != 0 && winner != 1`
しか検査しておらず、コメント側が実装から取り残されていた。

## Decision

`ParallelProbe` の勝者を「**ready のうち最小 index**」に変える。index `i` を勝者と
確定できるのは「`i` が ready、かつ `0..i-1` が全て解決済みで non-ready」のとき。

waired#828 の「格下げ」はこの上に素直に乗る: 会話の束縛先が同一キーの
リクエストを既に処理中なら、その候補を probe 窓の内側・同 tier の最後尾へ動かす
（`demoteBusySticky`）。順位が尊重されるので候補を除外する必要がなく、上位が
全滅したときに束縛先が最後の砦として勝つ経路も順序から自動的に得られる。

## Consequences

- Selector の順位が初めて実際に結果を左右する。priority / score / errorRate /
  loadFraction / sticky のすべてが対象で、これは #532 が `ErrorWindow` に値を
  入れたのと同じ「配線されていた半分をつなぐ」変更。
- 先頭候補のプローブ応答が critical path に乗る。上限は `probeBudget = 50 ms` で
  従来と変わらず、not-ready を返したピアは即座に判定を解放するので、実際に
  待つのは「上位ピアが黙って固まっている」場合だけ。RTT band は既に sort キー
  （`rttBucket`）なので恒常的に遅いピアはそもそも下位に沈む。
- waired#729 の「disco silence でピアを黒穴化しない」は保たれる。silent なピアは
  除外されず probe もされ、上位が全滅すれば勝つ。`sortMeshCandidates` の論拠
  コメントにあった「silent なピアも到着順で勝てる」という一文は成立しなく
  なったので、「最後の砦であって除外ではない」に書き換えた。
- 失敗 probe の telemetry は変わらない。判定後も `sigs` は最後まで drain する。
