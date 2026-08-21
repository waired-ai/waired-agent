---
status: accepted
superseded_by:
  - docs/decisions/20260822/0412-gpu-lane-is-driven-from-a-hosted-job.md
supersedes:
  - docs/decisions/20260723/1910-gpu-vllm-install-serve-ci-lane.md
---

# GPU レーンは実行ごとに作る L4 VM で走る (20260821 21:42)

## Status

Accepted。`docs/decisions/20260723/1910-gpu-vllm-install-serve-ci-lane.md`
の **runner 調達方針と休眠の実装**を置き換える(あちらの「T4 不採用」
「L4 が対象」「schedule は repo variable で守る」は維持)。**挙動が変わる**
(レーンが実際に実行されるようになる)。

**部分的に superseded。**
`docs/decisions/20260822/0412-gpu-lane-is-driven-from-a-hosted-job.md` が
**決定 2(`needs: gpu-up` を queue ガードにする)と決定 3(登録を
ephemeral/JIT にしない)を置き換える** —— どちらもセルフホストランナーを
登録する形に固有の話で、hosted ジョブが VM を API で駆動する形では
述べる対象が無い。決定 1(実行ごとに VM を作る)・決定 4(課金の上限を
GitHub に依存させない)・決定 5(有効なのに skip された夜は赤)は
**そのまま有効**。

## Context

1910 が `vllm-install` レーンを作ったのは 2026-07-24。以来 **2026-08-21 まで
一度も実行されていない**。`installtest-inference.yml` の全 run を走査した結果、
GPU ジョブの記録は 55 件すべて `skipped` で、success も failure も 0 件だった。
`GPU_RUNNER_ENABLED` は repo variable に作られたことがなく、`gpu` / `l4` ラベルを
持つ runner も登録されたことがない (waired-ai/waired#1229)。

その間に、この決定が守るはずだった面が素通りしている。waired-agent#891
(`--max-num-batched-tokens` / KV offloading / 起動失敗の診断) は
「`make e2e-vllm` が要る」と本文に書いたうえでマージされた。KV プールの
確保量を変える変更が、実機の観測なしで出荷されたということである。

沈黙していた理由は 2 つあり、どちらも 1910 が想定していなかった。

**1. dispatch は queue し続ける。** 1910 は「未設定なら job は skip
(queue し続けない)」と書いた。これが真なのは schedule 側だけで、あちらは
repo variable で守られている。dispatch 側の条件は `gpu != 'off'` だけで、
**runner の在庫を見ていない**。存在しないラベルに投げられた job は GitHub の
仕様上 24 時間 queue され、このワークフローの `concurrency`
(`cancel-in-progress: false`) が次の nightly をその後ろで待たせる。

**2. nightly の報告係は `skipped` を数えない。** `report` ジョブ (#215) は
`contains(needs.*.result, 'failure')` で発火する。四半期まるごと skip されている
レーンは、そこに一度も現れない。「誰も見ていない nightly の沈黙を終わらせる」
ために作られた仕組みが、この 1 レーンについては沈黙の側にいた。

## Decision

**1. runner は実行ごとに作って消す。**
`gpu-up` ジョブが private の infra リポにある instance template から
`g2-standard-4` (1×L4) を 1 台作り、`gpu-down` が消す。常設 runner は置かない。

理由はコストではなく**このリポジトリが public だから**である。常設登録された
`[gpu, l4]` runner は、VM の電源状態だけで守られた常設の的になる。実行ごとに
作れば、そのラベルは maintainer が始めた run の中でしか解決しない。
`runs-on` の宛先が実行時にしか存在しないことが、この設計の主目的である。

**2. `needs: gpu-up` が queue ガードそのものになる。**
`if:` は job を skip させない —— runner が居なければ GitHub は queue する。
依存を張れば「runner が居ない」は数分で skip になる。1910 が schedule 側にだけ
持っていた性質を、両方の入口に持たせる。

`gpu-up` の `if:` は **`vllm-install` と `agentgrade` の和集合でなければならない**。
ずれると、何も使わない夜に L4 を起こす —— 休眠しているレーンが唯一起こせない
はずの失敗様態である。

**3. 登録は ephemeral / JIT にしない。**
1 回の dispatch が **GPU ジョブを 2 本とも選べる** (`gpu != off` かつ
`agentgrade_model != ''`)。ephemeral runner は 1 job で退場するので、2 本目が
24 時間 queue に落ちる。分離境界は runner のプロトコルではなく **VM** が持つ ——
どちらにせよ run の終わりに消える。

副産物として `/opt/actions-runner/.env` が効く (JIT runner では効かない)。
`XDG_DATA_HOME` などのキャッシュ先はそこで与え、**public のワークフローには
private ホストのパスを書かない**。

**4. 課金の上限は GitHub に依存させない。**
`gpu-down` は `if: always()` の主経路だが、それだけでは上限にならない。
instance template が `--max-run-duration` + DELETE を持ち (GCE 側で強制、GitHub が
全落ちしていても効く)、private リポの毎時 reaper が孤児を掃く。したがって
`gpu-runner-down.sh` は**構造的に best-effort** で、ステップが失敗しても exit 0
にする —— 赤い teardown は「VM がまだ課金されている」を「壊れた夜」として
報告してしまい、それは意味が違う。

**5. 有効化されているのに skip された夜は、赤として報告する。**
`GPU_RUNNER_ENABLED == 'true'` かつ `vllm-install` が `skipped` なら
`report` を発火させる。**変数が未設定のうちは今までどおり黙る** —— 休眠は
設計であって障害ではない。1910 の休眠契約はここで保たれる。

## Consequences

* `vllm-install` / `agentgrade` は `needs: gpu-up` を持つ。`gpu-up` が失敗すると
  両方 `skipped` になり、**queue には入らない**。
* `report` の `needs` に `gpu-up` が入る。`gpu-down` は入れない —— best-effort で
  あることが決定なので、その赤は backstop が既に持っている条件を名指すだけになる。
* instance template が private リポにあるため、**GPU ホストで何が動くかを変える
  には private 側のレビューが要る**。public 側が渡せるのは名前と登録トークンだけ。
* 登録トークンを発行する PAT は Secret Manager に置く。GitHub secret にも VM にも
  置かない。VM 側は登録後に metadata server への到達を落とす。
* zone は在庫優先で切り替える。キャッシュディスクは zonal なので、代替 zone では
  **cold で走る** (~40 分 / 通常 ~15 分)。在庫切れは壁時計で払うのであって、
  赤い夜で払うのではない。
* `make e2e-vllm` が緑でも **waired-agent#891 のフラグは検証されない**。
  `internal/e2e/inference` は `VLLMConfig` を自前で組み立てており、
  `MaxNumBatchedTokens` / `KVOffloadingGiB` を設定しない —— あの derivation は
  `cmd/waired-agent/inference_vllm_tuning.go` にあり、e2e はそこを通らない。
  レーンが買うのは「vLLM が配信できる」であって「我々のチューニングが正しい」
  ではない。別途 issue にする。
* `agentgrade` は ollama が無いホストでは
  `internal/e2e/agentgrade/agentgrade_test.go` が skip するので、**何も測らずに
  success を返す**。runner イメージに ollama を焼くことが、このレーンが
  「モデルの判定」を返すための前提になる。

## Refs
- waired-ai/waired#1229 (レーンが全 schedule で skip されていた件)
- waired-ai/waired#590 (ephemeral GCP L4 で GPU e2e を回す実装 issue)
- waired-ai/waired#588 (vLLM の host 前提: libcuda.so / python3-dev)
- waired-agent#891 (実機観測なしで出荷された vLLM サーブフラグ)
- docs/decisions/20260723/1910-gpu-vllm-install-serve-ci-lane.md
