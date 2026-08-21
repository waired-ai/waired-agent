---
status: accepted
superseded_by:
  - docs/decisions/20260821/2142-gpu-lane-runs-on-a-per-run-l4-vm.md
---

# GPU vLLM install+serve の CI レーン新設 (nightly, GCP L4 self-hosted) (20260723 19:10)

## Status
Accepted（一部置換）。**runner の調達方針と「dormant」の実装**は
`docs/decisions/20260821/2142-gpu-lane-runs-on-a-per-run-l4-vm.md` が置き換えた
——常設 runner を待つのではなく、実行ごとに VM を作って消す形になり、
dispatch 側の queue ガードが `needs: gpu-up` として実装された。
**T4 不採用(SM 床未満)・L4 が対象・nightly+dispatch のみ・per-PR ゲートを
張らない・schedule は repo variable で守る**は、そのまま有効。

下の「未設定なら job は skip(queue し続けない)」は **schedule 側についてのみ
真だった**。dispatch 側は runner の在庫を見ておらず、存在しないラベルへの job は
24 時間 queue していた(waired-ai/waired#1229)。記録として残す。

## Context
vLLM の実インストール(~6 GB venv ビルド + CUDA JIT verify)と serving は GPU 必須で、これまで `make e2e-vllm*` のハーネス(`internal/e2e/inference/vllm_test.go`, `//go:build e2e && gpu`)を **リリース前に人手で** 走らせる運用だった(旧 monorepo の「GPU テスト実行義務」決定)。その決定は「将来 CI を実装する際は GPU lane を必須に含める。GPU runner 確保がブロッキング要件」と明記していたが、CI レーンは一度も作られていない(両 org リポの Actions 履歴に GPU workflow は皆無)。waired#835 Phase 2 で vLLM オンボーディングを実配線したのを機に、この GPU レーンを初めて CI 化する。

制約:(1) GitHub-hosted GPU runner は Tesla T4(compute capability 7.5)で、vLLM の SM_80 床(`VLLMVerifyImports`)を下回り **走らない**。(2) GPU runner はまだ存在しない。(3) ウィザード→executor→vLLM の完全 E2E は CP が `desired_engine=vllm` を配信すること(別 PR の CP flip の dev デプロイ)に依存するため、いま組めない。

## Decision
- 既存 nightly umbrella `installtest-inference.yml` に GPU 専用 job `vllm-install` を追加。**`make e2e-vllm` ハーネスをそのまま流用**する:`waired runtimes install vllm`(setup executor と同じ `vllmInstallCore` を通る実インストール)→ `make e2e-vllm-quick`(in-process gateway に対する serve smoke。daemon/CP/enrol 不要)。
- **GCP L4(`g2-standard-4`)の ephemeral self-hosted runner・on-demand** を対象(`runs-on: [self-hosted, linux, gpu, l4]`)。GitHub T4 は不採用(SM 床未満)。
- **nightly + workflow_dispatch のみ**、per-PR ゲートは張らない(~30–60 分・外部状態依存)。
- runner 未提供の間 **dormant**:schedule では repo variable `GPU_RUNNER_ENABLED == 'true'` のときだけ、dispatch では `gpu` input が `off` 以外のときだけ起動。未設定なら job は skip(queue し続けない)。
- ウィザード→executor→vLLM の VM 完全 E2E は CP flip の dev デプロイ後の **将来拡張** として据え置く。

## Consequences
- 「GPU テスト実行義務」を初めて CI で満たす足場ができた(runner 提供でアクティブ化)。
  **足場は 2026-07-24 から 2026-08-21 まで一度も踏まれなかった**:GPU ジョブの
  記録 55 件はすべて `skipped`、`GPU_RUNNER_ENABLED` は作られず、`gpu`/`l4` を
  持つ runner も登録されなかった(waired-ai/waired#1229)。
- 実行の前提はユーザ判断:GCP L4 quota(`NVIDIA_L4_GPUS`)+ ephemeral runner プール + `GPU_RUNNER_ENABLED` 変数。~$15.6/月(on-demand・平日 nightly・full ~60 分)。**この数字は US リージョンの
単価で、asia-northeast1 では同じ形が ~$20/月になる**(2026-08-21 再計算)。
- install レグは CLI 経路(`vllmInstallCore`)を踏むので executor 経路の venv ビルドと同一コアを検証するが、executor の判定ロジック(`vllmInstallDecision` 等)はユニットテスト側の担保のまま。

## Refs
- waired#835 Phase 2, #153, #154
- .github/workflows/installtest-inference.yml (job: vllm-install)
- internal/e2e/inference/vllm_test.go / Makefile (e2e-vllm*)
