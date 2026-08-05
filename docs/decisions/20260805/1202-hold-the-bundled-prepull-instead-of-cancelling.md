---
status: accepted
---

# ブート時の bundled pre-pull は、キャンセルではなく setup の発話まで保留する (20260805 12:02)

## Status

Accepted。#379 が提案した supersede（in-flight pull のキャンセル）は**採らない**。

## Context

エンジンが既にインストール済みの状態でデーモンが起動すると
（通常の再起動、再認証、再インストール、サービス起動前にエンジンを置くインストーラ）、
`bootstrapAfterEngineStart` が 1 秒ほどで走る。preferred-model はまだ無いので
`else if cfg.PullOnStartup` → bundled モデルの pre-pull が始まる。ウィザードの選択は
数分後に `setupApplyModel` → `SwapPreferredModel` → `PullModel(chosen)` として届く。
in-flight registry は **model_id キー**（#305: タグでキーすると同一モデルの
16.3GB と 18.0GB が同時に落ちるため、意図的にそうしてある）なので、別 id は
重複判定にかからず 2 本目が並走する。

#306 / PR #378 はブートドライバの順序で**同時ディスパッチ**を潰したが、
「先に始まって後から追い越される」この窓は意図的に残されていた。

## Decision

### 1. キャンセルではなく予防

#379 の前提条件（「CLI を殺してサーバ側の転送は止まるか」）は検証した:
ピン版 0.31.1 では**止まる**（`blobDownload` の参照カウントが 0 になると
ダウンロード自身の ctx が切れる。詳細は
`docs/knowledges/20260805/1202-ollama-pull-client-disconnect.md`）。
つまり #379 が恐れた最悪ケース（止まらないのに failed を記録する）は成立しない。
それでもキャンセルを採らない理由:

- **upstream の内部実装であって契約ではない。** `ollama_version.go` の
  `// renovate:` 行が示すとおりピンは自動 bump される。依存すれば静かに壊れる
- **角が残る。** 進捗送信がバッファなしチャネルで、切断と tick が重なれば
  `release()` に到達しない。ディスパッチ直後のキャンセルは nil `CancelFunc` で
  `ollama serve` を panic させうる
- **コストが大きい。** #379 自身が列挙した 5 点（`pullJob` に provenance が無い、
  `errors.Is(context.Canceled)` は使えない（`DefaultRunner.Run` は `cmd.Wait()` を
  返すので殺された子は `*exec.ExitError`「signal: killed」で OOM kill と区別が付かない）、
  `blockingRunner` で素朴なテストが空回りする、呼び出し順が両方向とも自明でない、
  新しい終端状態が要る）は今も全て有効
- **そもそも始める理由が無い。** 間違いだと告げられる直前のダウンロードを
  始める利得はゼロ

### 2. 待つ条件は「setup が発話したか」であって、時間ではない

リコンサイラが `Apply` で毎フレーム `setupNoteDesired(modelID, driving)` を
provider に報告する。ブートの待機は 4 分岐:

| 観測 | 動作 |
|---|---|
| setup がモデルを名指しした | **恒久的に降りる**（sticky） |
| フレームが来た & 誰も駆動していない | 今すぐ pre-pull |
| `prePullFrameGrace`(30s) 経過してもフレーム無し | pre-pull |
| ウィザードが駆動中 | 保留。`prePullHoldMax`(=`setupDesiredFreshWindow`) で打ち切り |

**sticky が load-bearing**: `Apply` は一度 active になると空フレームも折り込んで
報告するので、CP が desired を消す（setup 完了、ページを閉じた、チケット失効）と
`("", false)` が名指しの直後に届く。ここで再武装したら、遠回りで同じ二重
ダウンロードになる。

`driving` に新しい述語を発明せず、既存の 2 つの論理和にした:
`leaseLiveLocked()`（elevated executor がハートビート中 = エンジンインストール中）と
`!desiredStaleLocked()`（#308: 見ている前で desired が書き換わった）。

### 3. 決定は同期、ディスパッチだけ非同期

`bootstrapBundledModel` を `bundledPrePullTarget`（設定 / カタログ / **既に ready なら
activate して skip**）と `dispatchBundledPrePull` に割り、ブートは前者を
**同期で**走らせる。後者だけを待機ゴルーチンへ回す。
`activateBundledIfReady` はブート経路で `activateBundledIfUnset` を呼ぶ唯一の場所なので、
これを遅らせると重みがディスクにあるホストで `Active` が nil のまま —
`EngineReady()` false、ブートベンチマーク 400、`/inference/benchmark` 425、
Status が awaiting_model — の時間が生まれる。

待機ゴルーチンは `spawnPull` ではなく `pullsWG` に直接登録する。`spawnPull` は
取っていない in-flight スロットを `endPull` で解放し、完了した pull が持つ
deferred reconcile を撃ってしまう。

## Consequences

* **ブラウザから設定されるホストで、二重ダウンロードが起きなくなる。**
  ウィザードのモデルが帯域を独占する
* **リリース直前に決定を取り直す。** 保留中に operator がトレイでモデルを
  切り替えていれば（`preferredOverride` / `pendingSwapModel`）、fallback は降りる
* **CP を持たないホストは 30 秒遅れて pre-pull する。** 未登録・オフライン・
  push クライアント無効のビルド。ダウンロードそのものより桁で短い
* **カバーしないケースが 2 つ残る。** 保留が解けた後に選択が来た場合と、
  ダウンロード中にトレイで乗り換えた場合。どちらもキャンセルでしか解けず、
  #379 の元案の射程に残る
* **残る細い窓**: デーモンが setup の途中で再起動し、その**最初の**フレームが
  「エンジン指示はあるがモデルはまだ」で、かつ executor リースも切れている場合、
  #308 の freshness 判定では stale 扱いになり `driving=false` で保留が解ける。
  ここで第 3 の freshness 規則を発明する方が害が大きいと判断した
* **`-race` は開発機で通した**（`./...` 全 81 パッケージ、`cmd/waired-agent` は
  新規テストの 5 回反復も）。ただし**このリポジトリの CI には race レーンが無い**ので、
  以後の回帰を捕まえるものは無い。`-count=20` の反復も併用した

## Refs
- https://github.com/waired-ai/waired-agent/issues/379
- https://github.com/waired-ai/waired-agent/issues/306
- https://github.com/waired-ai/waired-agent/issues/305
- https://github.com/waired-ai/waired-agent/issues/308
- https://github.com/waired-ai/waired-agent/issues/359
- docs/knowledges/20260805/1202-ollama-pull-client-disconnect.md
