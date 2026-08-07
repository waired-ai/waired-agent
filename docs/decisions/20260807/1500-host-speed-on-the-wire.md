---
status: accepted
---

# ホスト速度の実測値をワイヤに載せる (20260807 15:00)

## Status

Accepted

## Context

#496 の足切り（PR #550）は出荷済みだが、判定はローカルで完結しており、
**測定値も判定理由もマシンの外に出ない**。運用上これが二つの穴になっている。

1. **理由が誰にも見えない。** 判定の痕跡はデーモンのログ 1 行だけで、
   `waired inference status` は `Local inference: off` としか言わない。
   ユーザーには「誰かが設定を切った」ように見える。
2. **ウィザード経由のホストでは測定すら走らない。** `awaitPrePullRelease` は
   セットアップがモデルを指名した時点で pre-pull を降ろすため
   (`boot pre-pull stands down: setup chose a model for this host`)、
   ブラウザで入った大多数のホストは一度も測られない。

同時に、#496 の scope item 2（実測レートをモデル推奨に食わせる）は
測定の結果として破棄済みであり、publish の目的は「CP が同じランキングを
出す」ことではなくなった。残る消費者は次の 3 つである:

- ローカル推論が off な理由の説明（CLI / 管理画面）
- waired#1065 の public share 速度ゲート（needs-design、**ほぼ全ホスト**で値が要る）
- #202 の実トラフィック由来の実測（本値がその初期値になる）

## Decision

1. **測定と判定を分離する。** 測定は「エンジンのビルドごとに一度取る
   ホストの性質」とし、判定（ローカル推論を off にする）は従来どおり
   「誰もこのホストのために選んでいない経路」に限る。人がモデルを指名した
   ホストでも測定はする — #465 の既定はその人のものを覆さないが、値自体は
   ホストの事実である。
2. **本体モデルのダウンロードより前に、直列で測る。** 20〜45GB の転送と
   並行して測ると混雑を測ることになる（較正時、同居ジョブありの 1 回が
   中央値から +21% ブレた）。ウィザード経路は `setupApplyModel` の
   `SwapPreferredModel` 直前に入れる。
3. **中央値 3 サンプル + ばらつき + 総時間 3 分の上限。**
   `proto/signer/inference_state.go` の
   `memory_bandwidth_measured_gbs` doc が定める規律（N サンプルの中央値と
   spread、単発不可）に合わせる。上限に達したら取れた分で判定し、実際の
   サンプル数を wire に載せる。**フィールドごとの中央値ではなく中央の
   サンプルそのもの**を publish する — 別々の run の prefill と decode を
   組み合わせた turn は、どの測定にも属さないため。
4. **wire には生の測定値だけを載せ、判定は載せない。**
   `signer.InferenceState.HostSpeed`（ポインタ + omitempty）。閾値は
   `hostfit.HostCutoffTurnBudgetSeconds` であり、消費者は自分の問いに
   合った閾値を当てる。#1065 が別の数字に落ち着いても形は変わらない —
   proto は additive-only なので、消費者が未確定のうちは「答え」ではなく
   「数字」を出す形が唯一安全である。
5. **閾値と式は `proto/hostfit` に移す。** CP は `proto/` しか import
   できないため、`internal/router` に置いたままでは 45.0 が両側に二重に
   存在する。`proto/hostfit` はまさに「agent の picker と CP の onboarding
   カタログが共有する唯一の実装」（waired#942）として作られた場所である。
6. **served NetworkMap からは CP 側で除去する。**
   `RecommendedMaxParallel` / `NotShared` と同じ扱い。ピアは Capacity と
   Models で経路を決めるので不要であり、署名済みマップに乗ったフィールドは
   それを知らない agent が canonical 再整形で落として**マップ全体の検証に
   失敗する**。proto を bump する前の CP は構造体にフィールドが無く unmarshal
   時に捨てるので、**bump と除去を同じ PR に入れる限り**跨ぎの窓は生じない。
7. **`memory_bandwidth_measured_gbs`（#252 の予約名）は使わない。**
   あちらは decode を下から抑える帯域値、こちらは 1 モデルで端から端まで
   測ったターン時間で、互いに代替できない。
8. **off の理由をローカルにも出す。** 足切りが off にしたときだけ
   `TurnedInferenceOff` を state ディレクトリに記録し、
   `waired inference status` が 1 行で説明する。この記録は因果の主張なので
   `WriteDesiredInferenceState`（＝他の誰かがトグルを動かす経路）が必ず
   落とす。測定値そのものは残る。
9. **1 行あたりのトークン数は定数をやめ、本測定の前に較正する。**
   詳細と実測は docs/knowledges/20260807/1450-prompt-token-cost-must-be-measured.md。

## Consequences

- ウィザード経路のインストールは、本体ダウンロードの前に約 1GB の
  プローブモデル取得と 15 秒〜3 分の測定を挟む。カード機で 14 秒、
  リファレンスの CPU-only 機で 2 分 15 秒（実測）。
- ローカル推論を明示的に on にしたホストでも、次にモデルを入れる経路で
  一度だけ測定が走る。判定は行われない。
- CP 側（private waired）は proto の bump と同じ PR で
  `effectiveInferenceState` の除去・validator のレンジ検査・
  `inferenceStateContentEqual` への追加をやる必要がある。
- ウィザードの**推奨判定**に本値を反映するかは本決定の範囲外。
  `management_device_model_catalog.go` の「ベンチマーク結果を読まない」
  ガード（waired#941）を開ける判断になるため、別 issue に切り出す。

## Refs

- #496 / PR #550（足切り本体）
- docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md
- docs/knowledges/20260807/1450-prompt-token-cost-must-be-measured.md
- waired-ai/waired#1065（public share の速度ゲート）
- waired-ai/waired#1056（ハード＝確実な OOM のみ、他は推奨）
- #252（`memory_bandwidth_measured_gbs`）/ #668（エンジン版とベンチキャッシュ）
