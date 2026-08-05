---
status: accepted
supersedes:
  - docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md
---

# executor リースは「ウィザードが駆動中」の証拠ではない (20260805 17:21)

## Status

Accepted。20260805 1202 の決定の **§2「待つ条件」の片方の論理和項だけ**を差し替える。
保留そのもの（キャンセルを採らない、sticky な stand-down、決定は同期・ディスパッチは
非同期）は全て有効。

## Context

`waired init` は非対話でも約 20 分停止し `Model not ready in time` を出して終了し、
その**直後**にモデルのダウンロードが始まる。ダウンロード自体は実測 28.5 秒 / 32.5 秒で、
手前の 10 分×2 は pull が 1 本も走っていない空白時間だった（#540）。

循環待ちだった:

- `waired init` は `attachSetupExecutor` (`cmd/waired/login_client.go`) で executor
  リースを取り、**プロセス終了まで**保持する。エンジンインストールの後に来るモデル
  待ちも、その保持期間の内側にある。
- リコンサイラはリースが生きていること自体を `driving` の根拠にしていた
  (`Apply` の `leaseLiveLocked()`)。
- 保留はその間 bundled モデルの事前 pull を握り止める。

つまり保留は、保留を待っている当のプロセスを待っていた。解放経路は 3 つしかなく
（30 秒の frame grace、60 分の ceiling、`driving=false` のフレーム）、実測では
init 終了の 17 秒後 / 49 秒後 / 50 秒後に pull が始まっている。`waired pause`/`resume`
はプロセスを再起動しないので `engineBootstrapOnce` は latch したままであり、その pull を
出せたコードは保留のゴルーチン以外に存在しない。独立した 3 実行で同じ形で、うち 1 本は
エンジンの入れ方が state dir 方式に変わった #492 のブランチ（run 31024997566）。

### 1202 の §2 が書かれた時点との差

`leaseLiveLocked()` を入れた理由は「CP が desired を 1 つも書く前にウィザードが駆動して
いることがある — elevated executor はエンジンインストールの間ずっとハートビートする」
だった。waired が管理するエンジンでしか serve しない方針（#488、オーナー決定 20260804）で
この前提が崩れた:

- **ブラウザ経路は desired state 無しにエンジンインストールへ入れない。**
  `setupEngineInstallWanted` (`cmd/waired/setup_install.go`) が executor のインストールを
  `setupDriving` = `st.Active` で門番している。ウィザードのためにエンジンが入る時点で
  フレームは指示を運んでおり、`desiredStaleLocked` だけで答えが出る。
- **リースを取るのも、エンジンを入れるのも `waired init` だけ。**
  他に何も無いリースはターミナルであり、それは保留が渡さずにいるモデルを待っている。
- 起動時に既にエンジンがあるケースは、#494 が入れば「前回の `waired init` が state dir に
  入れた」場合だけになる。そのホストは同じ init が bundled モデルも入れているので、
  通常は `activateBundledIfReady` で保留に入らない。

## Decision

`Apply` の `driving` から `leaseLiveLocked()` を外す。ゼロ値フレームの fast path は
`false` を報告し、通常経路は `!desiredStaleLocked()` だけを使う。

保留を可視化する: `awaitPrePullRelease` の入口と、唯一無言だった
`seen && !driving` の解放 arm に Info ログを足す。ハーネスの not-ready arm は
`waired logs` で daemon ログと engine ログを 3 OS 共通に出す。

## Consequences

* **非対話インストールの 20 分停止が消える。** `waired init` が前景でダウンロードを
  表示し切り、benchmark に到達する。#382 の sub-mode B と、#300 / #505 の同種の赤も。
* **#379 のブラウザ保護は維持される。** ウィザードがエンジンだけ書いてモデルをまだ
  書いていない窓では `!desiredStaleLocked()` が真になり、保留は続く
  (`TestApplyReportsDrivingWhileTheWizardHasNamedOnlyAnEngine`)。
* **放棄する窓が 1 つある。** 「エンジンはあるがモデルが無いホストで 2 回目の
  `waired init` を回し、かつウィザードがまだ何も書いていない」場合、bundled fallback が
  出る。そこでウィザードが別モデルを指名すれば二重ダウンロードになる。
  その状態自体が #540 の残骸であることが多く、ターミナル側に driver を名乗らせる案
  （CLI + daemon の 2 面、CP にフレームが増える）よりこちらを採った。
* **リース失効の副作用は遅れない。** `leaseLiveLocked` は死んだリースの
  `executorAttached` / `installClaimed` / `executorDriver` を落とすが、`snapshot()` が
  push ループで `setupPushInterval`(2s) ごとに、`SetupState` が executor のポーリング
  ごとに走らせる。push ループはこのリコンサイラと同じ場所で起動されるので、`Apply` が
  走るホストでは必ず走る。
* **テストの形を 1 つ変えた。** `inference_prepull_hold_test.go` は
  `setupNoteDesired` を直接叩いており、リースの解釈をまたげていなかった。本物の
  リコンサイラを繋いだ回帰テストを足した。

## Refs
- https://github.com/waired-ai/waired-agent/issues/540
- https://github.com/waired-ai/waired-agent/issues/488
- https://github.com/waired-ai/waired-agent/issues/382
- https://github.com/waired-ai/waired-agent/issues/379
- https://github.com/waired-ai/waired-agent/issues/308
- docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md
