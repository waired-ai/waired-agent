---
status: accepted
---

# 再認証は「保存済みで未適用」のコーディングツール指示を仕上げる (20260822 18:11)

## Status
Accepted

## Context

`waired init --force-reauth` を enroll 済みの端末で走らせると、資格情報は
更新され、host-speed も再計測され、ベンチマークも走るのに、**コーディング
ツールの行だけが `failed / executor_gone` で残る**(waired-agent#987)。

原因は 2 つの重なりだった。

1. `cmd/waired/main.go` は `reauth && renewing` のとき `*skipIntegration` を
   立てていた。端末の質問と `reportTerminalIntegrations` はこれで消える。
2. ウィザード側の適用 `runWizardIntegrations` のゲートは `setupActive`
   (= `Active && !DesiredStale`)だった。run の開始**前から**保存されていた
   指示は、daemon がその変化を見ていない以上つねに stale なので
   (`desiredStaleLocked`)、このゲートも通らない。

一方 executor 自体は attach するので daemon 側は `everSeen=true` になり、
`defer sess.Release()` で lease が消えると `integrationStep` は
「居たのに行に来ずに消えた」= `executor_gone` を出す。書き手が誰もいない
のに「書き手が去った」と報告される形である。

`setupActive` は本来「いまブラウザがこの run を駆動しているか」という
**引き継ぎ**の問いで、それを「保存済みの指示を書いてよいか」の問いに
流用していたのが混同の実体だった。なお、この run が他のすべて
(エンジン・計測・ベンチ)をやり直すこと自体はオーナー裁定
waired-agent#599(2026-08-09)の「configured なホストの再実行は全処理を
冪等にやり直す」に沿っている。コーディングツールだけが例外だった。

## Decision

- `daemonInitOpts` に **`AuthOnlyRefresh`** を足し、`--force-reauth` が
  `--skip-integration` を立てるのをやめる。フラグは「私のコーディング
  ツールに触るな」というオペレーターの明示指示として意味を保つ。
- daemon が **`integrations_pending`** を答える(`SetupStateResponse`)。
  「指示に 1 件以上あり、`state.SetupIntegrations` の記録がそれを
  カバーしていない」— コーディングツール行の第一分岐と同じ規則を、
  同じ場所で 1 回だけ判定する。
- 再認証 run は `integrations_pending` が真のときだけ、**質問せずに**
  指示を冪等適用し、行を報告する。適用先の位置は従来どおり(エンジン
  導入とモデル DL の間、waired-agent#311 の前倒し)。
- 適用しなかった run では従来のヒント行(`waired link <agent>`)を残す。
  適用した run では出さない — 直したばかりの作業の修復手段を案内する
  ことになるため。

**範囲は再認証 run に限る。** 通常の端末 run は今も「質問 → 適用 →
報告」で行を閉じており、壊れていない。そこまで自動適用に変えると
既存の対話体験と固定テストを動かすことになるので、報告された欠陥だけを
閉じる(オーナー裁定 2026-08-22)。

## Consequences

- `waired unlink` で意図的に外した統合が再認証で復活することはない。
  unlink は daemon の記録を消さないので、記録がカバーしている限り
  `integrations_pending` は偽になる。
- 「古い指示はブラウザの引き継ぎではない」(waired-agent#308)は不変。
  `setupActive` の意味も、それに依存する既存テストも変えていない。
- 旧 daemon(フィールドを知らない)に対しては `integrations_pending` が
  false に落ちるので、新 CLI の挙動は今日と同じになる。ゲートは
  入れない(リリース前)。
- 残る穴 1 つ: 非 TTY の `waired init`(resume)は "Run setup again? [y/N]"
  が No に倒れて executor に届かない。これは再実行ゲートの設計どおりで、
  別の問いなのでここでは触らない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/987
- https://github.com/waired-ai/waired-agent/issues/599 (再実行は全処理を冪等にやり直す、オーナー裁定)
- https://github.com/waired-ai/waired-agent/issues/308 (leftover desired state はブラウザ引き継ぎではない)
- waired#935 (統合の書き込みは昇格 CLI が行う), waired#1265 (発見の経緯)
