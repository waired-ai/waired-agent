---
status: accepted
---

# ローカル推論のオン/オフを 1 つの永続値にまとめる (20260805 12:36)

## Status
Accepted。方針の一次ソースは waired-ai/waired#1056 のオーナー決定コメント
(2026-08-03)、決定ログは `waired` の
`docs/decisions/20260803/1332-hard-vs-soft-model-limits.md` §4「under-spec は
ラッチではない」。用語の裁定はオーナーコメント (2026-08-04、
waired-ai/waired-agent#465): **under-spec → 推奨要件未満 (below recommended
spec)**。本記録は waired-agent 側の実装決定を固定する。実装は
waired-ai/waired-agent#465。

## Context

「ローカル推論を動かすか」を表す永続値が 2 つあり、繋がっていなかった。

| | A: インストール時の選択 | B: 実行時トグル |
|---|---|---|
| 保存先 | `agent.json` の `inference.enabled` | `<state-dir>/runtime/desired-inference` |
| 書き手 | `applyBundledSelection`（推奨要件未満で false） | `inferenceController` |
| 読み手 | `main.go` の 1 箇所、起動時に一度だけ | エンジン準備判定・ゲートウェイの 503・`subsystem_state` |
| 製品から変える手段 | **無し** | `POST /inference/{enable,disable}`、トレイ、`waired init` |

A が false になると `--disable-inference` が強制され、推論サブシステム自体が
構築されない。その帰結として、

- 管理 API の推論系ルートが未登録になり、CLI は `404 page not found` を出す
- `setupRec` が nil になり `onboarding-v1/v2/v3` を capability CSV から取り下げ、
  CP は `onboarding_reason=unavailable` を返す（ブラウザウィザードの袋小路）
- トレイは `/inference/status` の 404 を「古いデーモン」と解釈し、推論メニュー群
  ごと隠す（アプリ側の袋小路。issue 本文にも記載が無かった 3 つ目）
- `POST /inference/enable` は 200 を返すが B しか書かず、A が false の間 B には
  消費者が存在しないため何も起きない
- `shouldAutoSelectBundledModel` は `agent.json` があると二度と走らないので、
  A を戻す書き手が製品内に存在しない

`--inference-enabled=true` は A を**その起動限りで**上書きするだけで永続しない。
docs-site が復旧手段として案内していたのはこの経路だった。

## Decision

1. **永続する真実は `desired-inference` の 1 つ**。`agent.json` の
   `inference.enabled` は「未設定のときの初期値」に降格する。
   `Inference.ShareWithMesh` が `desired-share` を裏打ちする既存の形と同じ。
2. `ReadDesiredInferenceState` は未設定を `""` で返す（従来は
   `InferenceEnabled`）。4 つある `desired-*` リーダーのうちこれだけが
   「一度も触られていない」を表現できず、それが起動経路に既定値の参照先を
   与えなかった原因。
3. 起動時の判定は純粋関数 `planInitialInference(cfgEnabled, persisted,
   explicit)` に置く。優先度は **明示フラグ > 永続値 > `agent.json` > 既定**。
   置き換えた 3 行にはテストが 1 つも無かった。
4. **明示的な `--inference-enabled` / `WAIRED_INFERENCE_ENABLED` は永続値を
   書く**。1 回の起動だけ効いて何も残さない挙動が、案内されていた復旧手段が
   効かなかった理由そのもの。`--disable-inference` は永続しない運用者用の
   キルスイッチとして無変更で残す（フラグなのでラッチになり得ない）。
5. **「オフ」の意味はエンジン側に移す**。`startEngineAndBootstrap` はトグルが
   オフの間スタンドダウンし、**ラッチは立てない**（立てるとオプトイン後に
   ブートストラップ末尾が二度と走らない）。起動時ベンチマークも同様に閉じる。
   一方で**ハードウェアプロファイルとプローブ送信は閉じない** — CP が推奨要件
   未満のホストにモデル一覧とオプトインを描くために必要で、常時のコストは無い。
6. **動くオプトイン**: `waired inference on|off|status` を追加する。既存の
   2 つの案内文がこのコマンドを名指ししていた。トレイは `no_engine` かつ
   オフのときにトグルを隠さない（隠していたことが、一度もセットアップして
   いないパソコンで唯一の入口を塞いでいた）。
7. **明示指定のサーフェス整合**: `models pull` は容量超過（確実 OOM）を
   `--yes` でも拒否する。ブラウザは以前からラジオを無効化しており、
   docs-site も「メモリが足りないモデルは拒否する」と公開済みだった。
   推奨されないだけのモデルは警告のうえ通す。
8. 用語は **推奨要件未満 / below recommended spec**。意味が変わる識別子
   (`BundledModelSelection.UnderSpec` 他) も改名する。`ErrHardwareInsufficient`
   は #464 以降「容量ゲートを 1 本も通らない」の意味で正確なため据え置く。

## Consequences

- 推奨要件未満のホストでも推論サブシステムは構築される。ルート・
  onboarding capability・トレイのメニュー群・ウィザードがすべて生き、
  `subsystem_state` は既存の `disabled` をそのまま報告する（ワイヤの新規
  フィールドは不要）。
- エンジンとモデルの取得はオプトインが引き金になる。`--disable-inference`
  のときだけサブシステムが存在しない、という一本の条件に整理された。
- `proto/` は触っていない。`proto/hostfit/hostfit.go` と
  `proto/catalog/manifest.go` に残る旧語彙のコメントは、共有モジュールの変更を
  独立 PR に切るリポジトリ規約（CLAUDE.md §Modules）に従い、
  waired-ai/waired-agent#473 の一括置換に回す。
- ブラウザウィザード側（private repo の `setup_readiness.go` /
  `web/admin`）は本 PR の対象外。`onboardingUnavailable` の「Unreachable
  today」コメントは #133 以降実際には到達していたが、本変更でこの理由では
  再び到達不能になる。
- docs-site の復旧手段の記述が事実と一致するようになった。#473 の用語一括
  置換は本記録の裁定を前提にできる。

## Refs
- waired-ai/waired#1056（方針の一次ソース・オーナー決定コメント 2026-08-03）
- waired-ai/waired-agent#465（本実装）、#464（容量と推奨の算式）
- waired-ai/waired-agent#471（`BelowFloorModelID` を強制起動経路にだけ配線した
  先行 PR。対話オプトイン・コピー・管理 API・ブラウザは本 issue に残した）
- waired-ai/waired-agent#473（用語一括置換）
- `waired` `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`
- `docs/decisions/20260803/1812-bundled-model-default-is-derived-not-compiled.md`
- `docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md`
