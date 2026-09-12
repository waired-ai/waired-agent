---
status: accepted
---

# エンジンは選ぶもので、検出するものではない (20260912 11:00)

## Status
Accepted

## Context

`router.PickEngine` は、明示の指定が無いとき **ハードウェアだけで vLLM を選ぶ**
規則を持っていた（`VLLMAutoSelectable = true`、NVIDIA GPU + Linux +
VRAM ≥ `MinVLLMVRAMMB` + カタログに fit する vLLM variant が在ること）。

この規則が発火する経路は wizard を通らないものに限られる:
`waired init --non-interactive`、`internal/setup` の初回インストール時の
モデル自動選択、`waired runtimes install --auto` の `recommendEngine`、
そして制御プレーンの推奨表示。ブラウザの wizard はエンジンを事前選択せず、
選択は `desired_engine` として届いて venv の導入という形でホストに現れる。

誰も気づかなかったのは、最後の項（fit する vLLM variant が在ること、#572）が
VRAM 40 GB 未満のホストでは偽だったからで、答えは結局 ollama に落ちていた。
`qwen3.5` 系に vLLM variant を足す作業（#575）がその項を真にするため、
放置すると**該当する全ホストの既定が ollama から vLLM に変わる**。

`VLLMAutoSelectable` が true だった理由はコード上「#557 で vLLM の serving が
配線されたから」であり、vLLM を既定にするという裁定記録は存在しない。

## Decision

**エンジンはオペレーターが選ぶもので、既定は ollama**（オーナー裁定 2026-09-12、
waired-agent#1311）。

- `VLLMAutoSelectable = false`。ハードウェア自動選択は常に ollama を返し、
  理由文も「エンジンは選ぶもので、検出するものではない」と述べる。
- vLLM に到達する明示の経路 3 つは不変: wizard の `desired_engine`、
  `--prefer vllm`、`agent.json` の `inference.preferred_engine`。
- **serving の選択鎖はこの var から切り離す**（`chooseEngine`）。鎖は
  `[vllm, ollama]` 固定で、各段は `engineViable` が判定する。venv が入って
  いることがオペレーターの選択がホストに届いた形そのものなので、推奨側の
  var で serving まで止めると、wizard で vLLM を入れたホストが「venv は在る
  のに誰もそれで serve しない」状態になる。

## Consequences

- `waired runtimes install --auto` は全ホストで ollama を答える。vLLM は
  `waired runtimes install vllm` で明示的に入れる。
- #575 で vLLM variant を足しても、選んでいない人の既定は動かない。
- ハードウェアの梯子自体は残る（`VLLMAutoEligible`、`engineServesAModelHere`）。
  gate を開けたときの答えは
  `TestPickEngine_ShippedCatalog_LadderWithAutoSelectionOn` が記録する。
- 記録だった以下のテストは新しい既定に更新した:
  `TestPickEngine_ShippedCatalog_TodaysVerdicts`、`TestRecommendEngineFor`、
  `TestChooseEngine_*`。`TestChooseEngine_NoEngineReasonSkipsUnwalkedHops` は
  前提（gate が鎖を短くする）が消えたため削除した。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1311
- https://github.com/waired-ai/waired-agent/issues/575
- https://github.com/waired-ai/waired-agent/issues/1298
- https://github.com/waired-ai/waired-agent/issues/557
- https://github.com/waired-ai/waired-agent/issues/572
