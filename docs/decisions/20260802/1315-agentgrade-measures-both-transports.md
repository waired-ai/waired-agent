---
status: accepted
---

# agentgrade は両トランスポートを測り、ハーネス世代を記録する (20260802 13:15)

## Status
Accepted

## Context

`internal/catalog/agentgrade.json` の verdict は、それが測られたときの
**フィクスチャ**を `fixture_revision` で記録している。ダイジェストであって
手書きのバージョン文字列ではないのは、「ツール説明を編集した人がバージョンを
上げ忘れる」ことこそが避けたい失敗だったからだ（`revision.go`）。

その規律に穴があった。**ダイジェストは gateway を含まない。** #409 で
gateway がエンジンの取りこぼしたツール呼び出しを復元するようになった結果、
同じモデル・同じフィクスチャ・同じエンジンで 4 つのモデルの verdict が
`fail` から `pass` に変わったが、ファイル上はどの行も「現行」のままだった。
新旧 2 世代の測定が無言で混ざり、読み手に区別する手段がない（#426）。

もう一つ、測っている経路が実際の経路と違っていた。プローブは
`stream: false` で 1 発叩いていたが、**コーディングエージェントは常に
streaming する**。#409 の作業量の大半は streaming 側にあり、そこでの復元は
まったく別の実装だ — 全文を見てから決められる非 streaming 側と違い、
`toolTextSieve` はターンの終わりを見る前に「何を差し止めるか」を決めなければ
ならない。したがって従来のプローブは #409 の半分しか測っていなかった。

## Decision

**1. トランスポートをノブにし、両方測る。**
`Probe.Stream` / `make e2e-agentgrade STREAM=1`。既定は従来どおり
非 streaming で、保存済み verdict との比較可能性を壊さない。

**2. 分類器は分岐させない。** SSE は `readAnthropicStream` で非 streaming
と同じブロック列に畳み直してから `Classify` に渡す。分類器が経路ごとに
分かれた瞬間、「モデルの性質」と「こちらのコードの性質」が混ざる。
両経路が食い違ったら、それは **gateway の欠陥であってモデルの性質ではない**
—— `TestProbeTransportsAgree` が実 gateway と実 SSE エンコーダを通して
これをビルド失敗にする。

**3. ハーネス世代を、手で立てるフラグではなくビルドが打つ刻印で記録する。**
`agent_revision`（`-ldflags -X` で埋めた commit）と `transport` を report と
store の両方に持たせる。#426 は `gateway_tool_recovery: true` を提案していたが、
手で維持する真偽値は `fixture_revision` がダイジェストである理由そのものに
反する。刻印はビルドが打つので、実際に動いていたコードから乖離できない。

`--import` は刻印のない report と `-dirty` の report を**拒否**する。
再現できない木の上で測った verdict は、後から判断をやり直す材料にならない。
`--transport unary+stream` だけは operator が渡す —— 「両方測って一致した」は
1 回の実行では知りえない事実だからで、それ以外の値は受け付けない。

## Consequences

- 測定は `make e2e-agentgrade` 経由が必須になった。素の `go test` で出した
  report は import できない。刻印を打つのがターゲットだからで、これは
  「測定は手順であって習慣ではない」という #322 の姿勢と一致する。
- `agent_revision` は **陳腐化の判定には使わない**。フィクスチャ変更と違い、
  無関係な commit は測定を無効にしない。使えば merge のたびにファイル全体が
  `CoverageGaps` に落ち、CI の `--check` が恒常的に赤くなる。provenance で
  あって gate ではない。
- `fixture_revision` と `--fixture-bytes` は不変。`stream` フラグは
  `BuildRequest` ではなくプローブ側で立てているので、フィクスチャの
  「重さ」（canary が実クライアントと比べている数字）は動かない。
- streaming 側の usage は忠実に復元されない。gateway の `message_start` は
  `input_tokens: 0` を積み、実数はストリームに乗らない。採点は usage を
  読まないので、数字をでっち上げるよりゼロのほうがましだと判断した。

Refs #426, #409, #322
