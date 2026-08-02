---
status: accepted
---

# 分類できない失敗は network_error ではなく internal (20260802 08:32)

## Status
Accepted

## Context

`classifySetupFailure` はプロセス境界を越えてきた自由文字列から §7 のエラーコードを
推測する。実装は長らく「disk_full のマーカーに当たらなければ全部 `network_error`」
で、これは機能が入った当初「この経路は無条件に network_error を報告していた」ことの
名残だった。

rc7 の実機レビューでこれが表に出た。Linux ホストでモデル DL が 33.9/33.9 GB で失敗し、
daemon の journal には

```
"msg":"ollama pull failed","err":"exit status 1"
"msg":"ollama pull failed","err":"download: start ollama: context canceled"
```

が残っていた。どちらもネットワークの話ではない — 後者は pull パイプラインの自己 kill
(#305) で、エンジンを起動できなかったという意味である。それが NAVI では
「could not finish downloading. **Check its internet connection.**」になり、レビュアは
問題のない回線を疑いに行った。

`internal/` 側の分類器 (`classifyModelRejection`) はセンチネルで判定できるので影響が
なく、エグゼキュータが宣言したコードは `executorErrorCode` が優先するので、影響範囲は
「テキストしか手がかりがない失敗」に限られる。

## Decision

`classifySetupFailure` の既定値を `internal` にし、`network_error` は**本物の
ネットワーク語彙に当たったときだけ**返す。あわせて `timeout` と、この計算機自身が
原因の失敗を `internal` に落とすマーカー集合を追加する。

判定順は disk_full → 中断/自己 kill → timeout → network → internal。順序は仕様であり
テストで固定する:

- **中断を最初に見る**理由 — `connection refused` は宛先が 127.0.0.1 のとき
  「このホストのエンジンが上がっていない」であって回線の話ではない。テキストから
  それを見分ける手がかりはループバックアドレスしかないので、ネットワーク語彙より
  先に読む必要がある。
- **timeout を network より先に見る**理由 — `i/o timeout` は両方に該当する。
  「時間がかかりすぎて中止した」のほうが操作可能。

## Consequences

- 見覚えのない失敗は「Something went wrong on …」になる。**情報量は減っていない** —
  真の理由は `error_detail` と mgmt スナップショットの `failures` に載っており、
  この決定と同じ PR でどちらも運ばれるようになった。減ったのは間違った断定のほう。
- 既存テスト 2 本の期待値が反転する。どちらも「未知のテキスト = network_error」を
  固定していたもので、`write state.json: boom` を「ネットワークエラー」と呼んでいた。
  反転そのものが修正であり、PR body に明記した。
- マーカー方式なので取りこぼしは残る。ただし非対称性が逆向きになった: 以前は
  「分からない → 回線を疑わせる」で必ず外し、今は「分からない → 分からないと言い、
  理由を見せる」で外しようがない。
- 文字列マッチである以上、将来ダウンローダの文言が変われば network 集合の更新が要る。
  更新漏れの罰は `internal` への降格であって誤誘導ではない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/328
- https://github.com/waired-ai/waired-agent/issues/305 (自己 kill の発生源)
- waired-ai/waired#986 (rc7 レビュー, F39 / F07)
