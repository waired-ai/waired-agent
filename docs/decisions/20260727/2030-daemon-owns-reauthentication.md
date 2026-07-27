---
status: accepted
---

# 再認証を daemon に移し、enrollment の分岐をなくす (20260727 20:30)

## Status
Accepted

## Context

#175 は「daemon を経由しない enrollment が、capability を宣言しないデバイスを
作る」欠陥を消す作業。PR-1 (#271) が暗黙のフォールバックを、PR-2 (#290/#291) が
`--google-sa-login` と `--bypass-mode` を消した結果、`routeLocal` に到達する入口は
**再認証だけ**になっていた。

再認証とは、identity.json を持つホストで `waired init` を実行すること
（`waired auth status` が期限切れのときに案内する操作）。daemon 側の
`tokenRefresher` は refresh token が生きている間しかトークンを延長できず、
`ReauthRequiredAt` が立った時点でループを止める。そこから先に進む手段が
local enrollment しかなかった。

Control Plane 側は調査の結果、既に対応済みだった: `EnrollDevice` は
machine key で既存デバイスを引き当てて `renewDeviceTx` で更新し
(`#115 Phase C`)、デバイス数上限からも再認証は除外されている。
つまり必要なのは agent 側の配線だけで、proto 変更も CP のデプロイも要らない。

## Decision

`LoginStartRequest` に `reauth` を足し、daemon の `loginController.Start` が
それを見たときだけ「登録済みなら何もしない」早期 return を迂回する。
再認証後は live session が古い資格情報のまま動いているので、
node key rotation が使っていた teardown→activate の再構築を関数として切り出し
(`rebuildSession`)、rotation と re-auth で同じ mutex を共有させる。

これで `chooseEnrollRoute` から `routeLocal` が消え、判断は
「agent がいるか」だけになった。`enrollFacts` も `serviceInstalled` 一つになる。

古い agent は `reauth` を無視して no-op ステータス（active・session id なし）を
返すため、CLI はそれを成功と読まずに「サービスが古いので更新できない」と
名指しで失敗する。

あわせて、この移行が CI で初めて踏んだ既存欠陥を直した:
`waitForBenchmark` が `disabled` / `stopped` を「エンジン起動済み」と誤読し、
来ないモデルを 10 分待っていた（`waitForBundledModel` は同じ状態を既に終端として
扱っている）。installtest の各レグが 10 分ずつ無駄にしていた。

## Consequences

- **daemon が動いていないホストでは再認証できなくなる。** これは #175 の設計判断
  そのもので、`routeAgentDown` / `routeAgentAbsent` がサービスの起動方法を案内する。
- local enrollment の実装（`cmd/waired/main.go` の後半、`internal/setup/init.go`、
  `deploy.go` の一部、`waired init` のフラグ 6 本）は到達不能になった。
  削除は次の PR で行い、この PR は振る舞いの変更だけに保つ。
- `--auth-key` を登録済みホストで実行したときの「黙って成功を出す」挙動も
  同時に直った。

## Refs
- https://github.com/waired-ai/waired-agent/issues/175
- docs/decisions/20260727/1900-auth-key-headless-enrollment.md
