---
status: accepted
---

# 提供元は waired が管理するエンジンだけにする (20260804 19:41)

## Status
Accepted

## Context

waired は 2 つの推論エンジン（Ollama と vLLM）を提供する。vLLM は
`<state-dir>/runtimes/vllm` の venv からしか起動しない（`vllmVenvActive`）。
Ollama にはこの不変条件がなく、waired が管理しないソフトウェアで応答する経路が
3 つ残っていた。

1. **Ollama reuse モード**（`ollama_source: "reuse"`、#188）。11434 で動く利用者
   自身の Ollama を借用する。対話経路はすでに撤去済みで（init のプロンプトは削除、
   ウィザード実行部は bundled をハードコード）、到達手段は `agent.json` の手編集
   だけだった。
2. **Windows / macOS の暗黙の受け入れ**。bundled リゾルバが厳格なのは Linux
   だけで、他 2 OS は検出できた Ollama を無条件に「存在する」と扱う。
3. **`inference.external_endpoints`**（openai-compat アダプタ）。

テスト可能性の問題である。エージェントは自分がピン留めしたエンジンバージョンに
対してしか検証できない。macOS は `releases/latest` を取得してピンが無く、
Windows / macOS は任意の利用者バイナリを受け入れるため、「CI が検証したもの」と
「現場で応答するもの」が構造的に乖離する。#139 の偽 GREEN がこれである。

## Decision

**1. waired が応答に使う Ollama は、waired 自身がインストールし管理するものだけに
する。** 本エントリは上記 1 の除去（#489）を記録する。2 は #492 / #493、3 は #490
が担当し、#494 が全 OS で厳格解決を強制して installtest で検証する。追跡は #488。

**2. `inference.ollama_source` は設定項目ごと削除する。** リリース前のため旧
バージョン互換の機構は作らない。`MergeJSON` は素の `json.Unmarshal` なので、
残存キーは読み飛ばされ、デーモンの起動が失敗することはない。キーは次回の `Save`
でファイルから消える。11434 → 9475 のフリップは維持する。旧 `agent.json` を
利用者自身のエンジンのポートに載せないための唯一の防波堤だからである。

**3. adopted は残す。** 前回実行の孤児プロセス（ピン完全一致）を引き継ぐ経路は
クラッシュ復旧のためのものであり、管理外ソフトウェアで応答するための経路では
ない。management API の `managed` 軸、power エンドポイントの 409、トレイの
無効行はすべて adopted のために残る。

**4. 管理外エンジンの文面から reuse の含意を落とす。** 本変更後
`EngineManaged == false` の意味は adopted ただ 1 つになるため、文面は事実だけを
述べる（CLI `(not managed by waired; stop/start unavailable)`、トレイ
`Engine not managed`、409 `engine is not managed by waired; power control
unavailable`）。原因の全開示は docs 側に置く。

## Consequences

* `OllamaConfig.Borrowed` / `EngineModeBorrowed` / `ErrEngineBorrowed`、
  `EnsureRunning` のプローブ専用経路、`Stop` / `Park` の borrowed ガードを削除。
* 版数警告は完全一致のみになった。利用者提供エンジン用の下限
  `OllamaSupportedMinVersion`、その薄いラッパー `OllamaVersionAtLeast`、
  `OllamaDetection.Supported`（読み手ゼロ）も同時に撤去。汎用の
  `internal/version.AtLeast` が後継。
* Linux の `resolveOllamaBinary` に逃げ道が無くなった。Windows / macOS の PATH
  フォールバックは、両 OS の install がまだ state dir 外にあるため #492 / #493
  まで残す。
* `agent.json` が `ollama_source: "reuse"` と **11434 以外の明示ポート**を持つ場合、
  waired は利用者のエンジンが占有するポートへ spawn しに行く。相手のバージョンが
  ピンと完全一致すれば adopted として引き継ぎ、不一致なら明示的に失敗する。
  adopted の判定はポートではなくバージョンで行う設計（#336 の管轄）のため本
  変更では触れない。よくある 2 形（ポート未指定 / 11434）はフリップで 9475 に落ちる。
* docs.waired.ai から「自前の Ollama を使う」旨の記述を EN / ja とも撤去。
  `--skip-ollama` の説明は「このパソコンでモデルを動かさない場合」に置き換えた。

## Refs

#489, #488, #188, #139, #43, #336
