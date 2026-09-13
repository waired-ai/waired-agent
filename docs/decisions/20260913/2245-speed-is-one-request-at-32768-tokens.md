---
status: accepted
supersedes:
  - docs/decisions/20260820/1130-step-down-walks-the-selection-ladder.md
  - docs/decisions/20260821/1130-first-token-is-shown-not-judged.md
  - docs/decisions/20260822/1935-measurement-returns-to-the-ranking.md
  - docs/decisions/20260829/1740-speed-is-measured-at-fixed-depths.md
  - docs/decisions/20260830/0230-measure-once-per-selection-not-on-a-schedule.md
  - docs/decisions/20260904/0000-retire-the-long-context-sweep.md
  - docs/decisions/20260905/0000-a-notice-is-published-and-lapses.md
---

# 速度は 32,768 トークン 1 リクエストで測り、1 リクエストあたりの秒数で判定する (20260913 22:45)

## Status
Accepted。オーナー裁定 2026-09-13。一次記録は private monorepo の waired-ai/waired#1381 とその決定記録 `docs/decisions/20260913/2230-speed-verdict-is-seconds-per-request.md`（番号と path のみ。リポ間の supersede は guard が解決できないので散文で指す）。実装は waired-ai/waired-agent#1341（L115）と #1342（L117）、コントロールプレーンと NAVI は waired-ai/waired#1382（L116）。

次の記録を**部分的に狭める（覆さない）**。各記録の `## Status` に鏡の一文を置いた。

- `docs/decisions/20260829/1740-speed-is-measured-at-fixed-depths.md`: 段の数、繰り返し、予算、速度キーの量。
- `docs/decisions/20260821/1130-first-token-is-shown-not-judged.md`: 固定深さの実測を定数と比べることを許す。実トラフィックの `first token:` 行は据え置き。
- `docs/decisions/20260905/0000-a-notice-is-published-and-lapses.md`: `BetterModel` を外す。`LighterModel` は線で発火する。
- `docs/decisions/20260830/0230-measure-once-per-selection-not-on-a-schedule.md`: 鍵と寿命。永続化。
- `docs/decisions/20260904/0000-retire-the-long-context-sweep.md`: 「対話床の判定は浅い decode だけ」。「深い段を足さない」は継承する。
- `docs/decisions/20260822/1935-measurement-returns-to-the-ranking.md`: 比較量と `BelowFloor` / `FloorTokps`。台帳は永続化の器。
- `docs/decisions/20260820/1130-step-down-walks-the-selection-ladder.md`: 承認済み文面の「interactive floor」の句。

不触: `docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md`（足切り。決定 2 の式を製品全体の定義にし、決定 7 を確認する）、`20260810/0228`（上界の型を再利用）、`20260812/0245`、`20260809/1726`・`20260812/0331`・`20260830/0235`・`20260912/1130`（継承する規律）、`20260822/0218`、`20260829/1735`、`20260805/1527`、`20260807/1700`。

## Context

`20260805/1620` は決定 2 で「判定量はプローブ自身の 1 ターン所要時間 `P/prefill + (P/21)/decode`」と定め、決定 7 で「デコード tok/s は判定器として誤り」と裁定した。しかしそれは足切り（代役モデル、21k、45 秒）にしか適用されておらず、提供中モデルの判定は今も浅い decode tok/s で行われている。

### 今の 4 計測（2026-09-13、`f51a0e04`）

| 計測 | 何を | 閾値 | 読み手 |
| --- | --- | --- | --- |
| 足切り `measureHostCutoff` | 代役 0.8B、21k 入力 + 200 生成 → `hostfit.TurnSeconds` | 45 秒 | ローカル推論の既定オン / オフ |
| ブートベンチマーク `RunBootBenchmark` | 浅いコンテキストの decode tok/s、3 本の中央値 | 60（tray / doctor / CLI / お知らせ / 実測の narrow）、20（コントロールプレーン経由でセットアップ完了画面） | 軽いモデルの提案、上位モデル推奨、セットアップ完了画面 |
| prefill の段 `MeasurePrefillRate` | 4,096 / 8,192 / 32,768 の prefill tok/s、予算 3 分 | なし | ピア選択だけ（`/healthz`） |
| 実トラフィックの TTFT | `RequestEvent.TTFTMs` | なし（`20260821/1130`） | `first token:` 行、ピア選択の補正 |

### 実測（Apple M5 Pro 48 GB のラップトップ、ollama 0.33.3、2026-09-13。waired-ai/waired#1380）

| | qwen3.8-27b mtp-q4 | qwen3.6-35b-a3b mtp-q4 |
| --- | --- | --- |
| 浅い decode（ブートベンチマークと同条件） | 21.64 tok/s | 66.02 tok/s |
| prefill 32,768 | 252.9 tok/s（129.6 s） | 901.3 tok/s（36.4 s） |
| 32,768 深さでの decode | 15.8 tok/s（−27%） | 45.7 tok/s（−31%） |
| `TurnSeconds`（N = 1,560） | 228 s | 70 s |
| 34k システムプロンプト cold の TTFT | 139.9 s | 39.7 s |

- 同じ実測に 2 つの床が逆の助言を出す（27B の 21.6 tok/s: 20 は無警告、60 は「切替」）。
- どの切替判定も prefill を読まない。コーディングエージェントの初回ターンの待ち（上の TTFT）はどちらの床にも入らない。
- 浅い decode は深さでの decode を 27〜31% 過大評価する。
- 上位モデル推奨の予測は decode の重み比だけで、prefill を含められない。
- 深さでの 3 標本の幅は 1% 未満（32,768 の prefill: 251.3 / 252.9 / 253.3 tok/s）。

参照値（hosted Claude Opus 5、Artificial Analysis 2026-09-13 取得）: decode 約 52 tok/s、最初の回答トークンまで high 10.5 s / xhigh 29 s（入力 10k）。オーナーは上のラップトップ × 27B を「使える限界」と評価し、切替の線はそれより少し厳しい値と決めた。

## Decision

1. **計測は 1 本。** 提供中モデルに対し、**32,768 トークン入力 + 生成 128 本の 1 リクエスト**（ウォームアップ 1 本を捨てた後）。ollama は `prompt_eval_*` / `eval_*`、vLLM は stream の TTFT + `usage`。生成 64 本未満の標本は捨てる。標本は 1 本で、`TurnSeconds` が線の ±10% に入ったときだけ 2 本目を取る。ブートベンチマークと 4,096 / 8,192 の段は廃止する。段は 32,768 だけで全ホスト共通（`20260829/1740` の「深さは定数」は維持）。エンジンの静穏ゲート（`20260809/1726`）、排他 claim（`20260812/0331`、`20260830/0235`）、bounce の待ち（`20260912/1130`）は継承する。
2. **指標は 1 リクエストあたりの秒数** `TurnSeconds = 32768 ÷ prefill + N ÷ decode`。`proto/hostfit` の `TurnSeconds` と同じ式で、N = 1,560 = 32,768 を `HostCutoffPromptCompletionRatio`（21）で割った値。製品内の「1 リクエスト」の定義はこれ 1 つ。
3. **切替推奨の線は秒数 1 本、190 秒。** `proto/hostfit` に 1 定数として置き、`HostCutoffTurnBudgetSeconds`（45）と並べる。判定は `TurnSeconds > 線`。20 tok/s と 60 tok/s は実測の判定から退場する: `resolveInteractiveFloor` と設定 `interactive_floor_tokps` は廃止、`hostfit.DecodeFloorTokps`（20）はダウンロード前の予測の注記にだけ残り、`router.CodingAgentSelectionFloorTokps`（60）はスピルキャップ導出の anchor（`internal/router/coding_floor.go`）としてだけ残る。tray・doctor・CLI・お知らせ・`waired status`・`/healthz`・コントロールプレーンは同じ定数を読む（線はワイヤで配る）。利用者向け文面は「N s per request (target: M s or less)」の形（`docs-site/TRANSLATION.md` の per request / target）。
4. **タイムアウトは全体で 1 本、線と同じ秒数。** 計測開始からそれを超えた時点で（prefill 中でも生成中でも）切替を推奨し、計測は止めない。モデル切替と明示の停止だけが計測を止める。
5. **完了は計測の完了。** 線超過は `over_budget` として公開する（失敗ではない）。`waired init` は待ち続け、線超過で「別のモデルに切り替えますか」を出し、No でも done まで待って完了の箱を出す。コントロールプレーン側の第 3 状態と NAVI は private の記録に従う。
6. **上位モデル推奨は廃止。** `UpgradeCandidate` / `BenchmarkUpgrade` / `notice.BetterModel` / tray と CLI の「Better model available」。軽いモデルの提案は残り、線で発火する（waired-ai/waired-agent#1342）。
7. **計測結果は永続化し、再利用する。** 鍵は（variant SHA, エンジン版, GPU）。モデル切替・切替の取り消し・デーモン再起動で鍵が一致すれば再計測しない。利用者の再実行はその鍵を上書きし、`/healthz` の公開値も置き換わる。`failed` は永続化しない（メモリ内の判定のまま。永続化するのは計測値と上界）。台帳は `20260822/1935` の `MeasuredVariants`（variant SHA 鍵）。足切りのキャッシュ（`20260807/1700`。鍵はエンジン build + agent 版、対象は代役モデル）は別物のまま。bench cache のスキーマ版は上げる — 「別の量」なので `20260904/0000` の条件に合う。
8. **段下げの「Remove A?」の既定は No。** 戻すときに再ダウンロードしないため（決定 7 と対）。非対話は従来どおり keep。
9. **ピア選択は同じ秒数を読む。** slowness は `(capacity_used + 1) × TurnSeconds`。25% の帯（`speedBucketRatio`）と「未計測は最良の帯」は維持。線内に終えられなかったピアは上界（`TurnFloorSeconds` と同じ型。nil ではないので `20260822/0218` の同点規則に触れない）を公開し、計測済みピアの下に置く。依頼側の観測記録（`PrefillWindow`、15 分）は「新しい計測が古い計測を置き換える」に狭める（`keepBestLocked` を外す）。段が 1 つなのでラウンドごとの深さの決定は自明になる。

### 補足: 深い文脈の崩落は配置の助言で扱う

24 GB カード × 27B MTP-Q4 は浅い decode 43.67 tok/s が 176k で 1.54 tok/s（13/66 層が CPU に落ちるため。waired-ai/waired#1357）。原因はロード時に決まる配置なので、この計測では拾わず、エンジンログの `offloaded N/M layers` を読む別の助言（waired-ai/waired-agent#1337、L113）で出す。32,768 より深い段は足さない（`20260904/0000` の裁定を継承）。

## Consequences

- 7 記録の失うもの / 保つものは各記録の `## Status` に書いた（上の一覧）。
- installtest の `waired init` transcript に `NUMBER tok/s` を要求する正規表現と `harness-failure-strings-guard.sh`（`20260805/1527`）は、CLI の数字が秒になる L115 / L116 の PR で同時に変える。
- `TestInteractiveFloorVerdict_RestsOnTheShallowRateAlone`（`20260904/0000` が pin）は L115 で置き換える。
- 順序: 本記録 → proto の additive 小 PR（`turn_seconds` / `over_budget` / 経過秒。public-first、`20260719/0000`）→ コントロールプレーン（受け側が先）→ agent。
- 量子化は秒数に decode 側からだけ入る（27B の 4 ビルドで 32,768 の prefill は 9% 幅、decode は 62% 幅。waired-ai/waired#1357）。
- 用語: 「1 リクエストあたりの秒数」/ `TurnSeconds` / 「目標」/ 「切替の線」。「往復秒数」は使わない。

## Refs
- waired-ai/waired-agent#1341（L115）/ #1342（L117）/ #1335 / #1337（L113）/ #1327 / #1329
- waired-ai/waired#1381 / #1382 / #1380 / #1361（private monorepo、番号のみ）
- 狭めた記録: 上の 7 本
- `docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md`、`docs/decisions/20260810/0228-prefill-floor-screens-below-spec-hosts.md`、`docs/decisions/20260822/0218-residency-breaks-a-tie-not-a-ranking.md`、`docs/decisions/20260829/1735-capacity-is-warm-conversations.md`、`docs/decisions/20260809/1726-benchmark-yields-to-engine-restarts.md`、`docs/decisions/20260812/0331-one-exclusive-engine-measurement.md`、`docs/decisions/20260830/0235-the-engine-claim-is-engine-agnostic.md`、`docs/decisions/20260912/1130-a-bounce-we-chose-waits-for-the-turns-on-it.md`、`docs/decisions/20260805/1527-benchmark-assert-stays-blocking.md`、`docs/decisions/20260807/1700-host-speed-is-an-install-time-step.md`、`docs/decisions/20260719/0000-concurrent-proto-development.md`
- `docs-site/TRANSLATION.md` の `per request` / `target` の行
