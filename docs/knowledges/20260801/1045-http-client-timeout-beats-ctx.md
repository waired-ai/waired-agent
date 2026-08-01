# http.Client.Timeout は ctx より強く、呼び出し側から予算を伸ばせない (20260801 10:45)

## Issue

waired-agent#316 の調査中、トレイの「Stop inference engine」が必ず
`context deadline exceeded` で失敗する理由を追ったところ、原因は
「呼び出し側で `context.WithTimeout(ctx, 20*time.Second)` を渡せば伸ばせるはず」
という思い込みが**成り立たない**ことだった。

`http.Client.Timeout` はリクエスト ctx とは**別に**適用される実時間上限で、
実効の期限は `min(ctx の期限, Client.Timeout)` になる。したがって
3s の `http.Client` を共有している限り、どの呼び出し側も 3s を超えられない。

## Learnings

- 1 エンドポイントだけ予算を伸ばしたいなら、方法は 3 つしかない:
  1. 長い `Timeout` を持つ **2 本目の `http.Client`** を用意して振り分ける
  2. `Timeout: 0` にして純粋に呼び出し側 ctx へ委ねる（既存の全呼び出し箇所から
     下限が消えるので危険）
  3. 呼び出しごとに `http.Client` を作る
  #316 では 1 を採った（`Client.wcEngine`、20s）。IPC クライアントは
  `DisableKeepAlives` なのでプールを失う損もない。
- 同じ罠が他にもある。実測した固定予算の在庫（origin/main 時点）:
  トレイ read/write 3s、`pollOnce` 全体 4s、CLI の `httpGet` 5s / `httpPost` 10s、
  `observabilityclient` パッケージ変数 3s、doctor プローブ 3s。
  「サーバ側が重い状態」に入ると、これらは軒並み噛む。実際 macOS で
  `waired runtimes status` が 10s で死ぬのに、同じエンドポイントへ 30s 予算の
  直接リクエストは成功する、という所見が出ている。
- クライアント予算はサーバ側の最悪時間より必ず長く取る。#316 ではデーモン側の
  停止予算を 15s に固定し、トレイ側を 20s にして「自分で起こしたタイムアウトを
  障害として表示する」ことを防いだ。テストでもこの不等式を assert している
  （`TestEngineWriteTimeoutOutlastsDaemonStopBudget`）。

## Refs
- https://github.com/waired-ai/waired-agent/issues/316
- docs/decisions/20260801/1030-engine-stop-commit-to-kill.md
