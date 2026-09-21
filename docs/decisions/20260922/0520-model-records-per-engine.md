---
status: accepted
---

# モデルの記録を推論エンジンごとに持つ (20260922 05:20)

## Status
Accepted

## Context

`state.json` の `models` は、モデル ID ごとに 1 行で、どの推論エンジンの行かを持っていなかった。読む側の約 20 箇所は「Ready なら、いま動かしている推論エンジンにとって Ready」と読んでいた（`models ls`、`models pull` の即答、tuning の対象、Active の読み手、ルーターのローカル判定など）。

製品の操作だけで食い違いに至る経路があった。ollama で動かしているパソコンで vLLM の速度計測（#1298）がモデルを取得すると、そのモデルの行が vLLM のビルドで Ready になる。すると次のことが起きていた。

- ollama はそのモデルを持っているものとして扱われ、`models pull` はすぐ「ready」を返した。
- tuning は vLLM のビルドの大きさで計算された。
- ベンチマークは ollama に、取得していないモデルをモデル ID の名前で問い合わせて 404 になった。

書く側にも混ざる経路があった。vLLM の HF ダウンロードは、ollama の Ready 行を refresh とみなして上書きし、1 行が 2 つの推論エンジンの重みを指す形になっていた。

## Decision

- 記録を推論エンジンごとに分ける。`models` は ollama の記録、`vllm_models`（空なら書かない）は vLLM の記録にする。`staged_variants` / `retained_variants` はこれまでどおり ollama だけのもの。
- 読み書きは `internal/catalog/engine_rows.go` のアクセサ（`ModelFor` / `ModelsFor` / `SetModel` / `RemoveModel` / `RecordsFor`）を通し、呼び出し側がエンジンを名指す。
  - いまの推論エンジンについての判断（`models ls`、`models pull` の待ち、セットアップの行、同梱・選択モデルの有効化、事前取得、ルーター）は、動かしている推論エンジンの記録を読む。
  - Active を読むもの（`subsystemFacts`、`EngineReady`、`activeEngineTag`、`activeEngineModel`）は、Active の推論エンジンの記録を読む。記録が無ければ、その推論エンジンが答える名前は無い（モデル ID で代用しない）。
  - vLLM 専用の経路（HF ダウンロード、`resolveVLLMStart`、前のモデルの候補）は `vllm_models` を読み書きする。
- `models rm` はモデルを名指すので、両方の推論エンジンの重みと記録を消す。
- `models ls` は動かしている推論エンジンの分だけを出す。もう一方の推論エンジン用にだけあるモデルは `not_present` と表示し、`pull` はこの推論エンジン用に取得する（オーナー判断 2026-09-22）。
- 古い `state.json` からの移行は書かない。旧版からのアップグレード経路は考慮しないというオーナー判断（2026-09-22）による。
- tuning の対象の解決には、ollama で動かない variant を記録から採らないガードも入れる。

## Consequences

- この変更より前の agent が `models` に書いた vLLM の行は、ollama の記録として読まれる。移行は無いので、その推論エンジンでもう一度取得するか `models rm` で消すまで残る。tuning のガードは、そうした行で ollama の tuning を vLLM のビルドの大きさで計算しないためのもの。
- vLLM の速度計測が残す記録（#1326、残すかどうかは未決）は、vLLM の側に置かれる。ollama の判断には影響しない。
- 新しい推論エンジンを足すときは、`EngineRuntimes` とアクセサに足す。知らない runtime への書き込みは捨てられ、読みは「記録なし」を返す。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1520
- https://github.com/waired-ai/waired-agent/issues/1298
- https://github.com/waired-ai/waired-agent/issues/1326
- `internal/catalog/engine_rows.go`
