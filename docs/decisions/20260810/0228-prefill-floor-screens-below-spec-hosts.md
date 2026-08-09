---
status: accepted
supersedes:
  - docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md
---

# 推奨要件未満は、プレフィルのみの下界で結論してよい (20260810 02:28)

## Status

Accepted。`20260805/1620-host-cutoff-is-a-measured-probe.md` の**決定 4 を
部分的に狭める**（覆さない）。同決定の 1〜3 と 5〜8 はそのまま有効で、
とくに「canonical 深度で測ったターン時間だけが `turn_seconds` を名乗れる」は
本決定でむしろ強化される。

## Context

waired-agent#579 の後半。Stage 2 で計測に窓（`hostSpeedMeasureDeadline` 16 分、
install 経路は `hostspeed.InstallWindow` 5 分）を掛けたが、**窓に収まらない
ホストは判定不能のまま `applyHostCutoff` が true を返し、20〜45 GB の
ダウンロードが始まる**という穴が残った。cutoff は、それが存在する理由その
ものであるホストで発火しない。

コストの実測（GitHub macos-14 ランナー、3 vCPU M1 / 7 GB — install+inference
レグの恒久ハードウェア）:

- calibration 1 本 **2 分 17 秒**
- フルサンプル 1 本 **7 分 12 秒**
- 合計 9 分 29 秒。その 1 秒後に bundled model の pull が dispatch され、
  **ダウンロード自体は 11.5 秒で完了**した。遅かったのではなく、始まって
  いなかった。

一方で calibration リクエストは既に本物のエンジンカウンタ
（`prompt_eval_count` / `prompt_eval_duration`）を返しており、**その timing を
捨てていた**。

## Decision

1. **プレフィル率だけから下界を出し、それが十分大きければフル深度を払わずに
   「推奨要件未満」と結論してよい。**

   ```
   floor = HostCutoffProbeDepthTokens / prefill   （proto/hostfit.TurnFloorSeconds）
   ```

   片側でのみ使う。`floor > 閾値 ⇒ turn > 閾値` は健全、逆向きは何も言わない。
   健全性は独立な 2 つの事実が同じ向きに積み上がる:

   - `TurnSeconds = P/prefill + (P/21)/decode`。decode 項は正なので落とせば
     必ず小さくなる。
   - プレフィル率は深度に対して単調非増加。本リポジトリの実測で参照機は
     68% 深度 833 tok/s に対し全深度 671 tok/s。浅い測定は率を過大評価し、
     過大評価で割れば時間は過小評価される。

2. **発火線は `HostCutoffTurnBudgetSeconds × 1.5 = 67.5 秒`。** 45 秒（決定 3）
   ではなく、その 1.5 倍に置く理由は 1 つだけ: **参照機の実測ターン 66.6 秒の
   「上」に置く**ため。参照機は cutoff が正しく落とす機体なので、screen が
   結論するのは「既に使えないと実証済みの機体よりさらに遅いホスト」だけに
   なる。45 秒と 67.5 秒の間では下界と実測が「45 秒のどちら側か」で食い違い
   得るので、そこは実測が決める。

3. **`turn_seconds` は依然として実測だけを指す。** 下界は
   `signer.HostSpeed.TurnFloorSeconds` という独立フィールドに載り、
   `Method = ollama_prefill_floor` が併走し、`TurnSeconds` は 0 のまま
   （オーナー裁定 2026-08-09、waired-agent#620）。これは決定 4 を狭めた結果
   として**必要になった**規律である: 深さ付きでない計測が存在するように
   なった以上、どのフィールドが深さ付きかを wire 上で言えなければならない。

   結果として、この形を知らない consumer は「何も計測されていない」と読んで
   判定を降りる。それが正しい既定である。

4. **誤発火を防ぐ前提条件を 4 つ課す。** 下界は実測より弱い証拠なので、
   証拠の弱さを前提条件の強さで埋める:

   - **2 回読み、両方が線を超えること。** 詰まったエンジンは求める形を 1 回
     だけ出す。
   - **同一エンジンプロセスであること**（`EngineGen` 不変）。自分が命じた
     再起動は裸の EOF として出るので、世代カウンタ以外に見分ける手段がない
     （#359/#582 と同じ論法）。動いていたら無charge で読み直す。
   - **ホストが暇であること**（`EngineQuiet`）。**待つ**のであって見るだけでは
     ない: 直前の `ensureHostCutoffProbeModel` が新規インストールでは ~1 GB を
     引き、`endPull` が serve reconcile を発火してエンジンを再起動するので、
     screen が最初に走れる瞬間はしばしば「エンジンが再起動されている最中」
     である。1 回見るだけの実装は、**screen が存在する理由そのものである
     install 経路で screen を降ろす**。
   - **読みが truncate されていないこと**（`prompt_tokens ≥ 1500`）。短い
     プレフィルは固定オーバヘッド比が高く率を過小評価する = ホストを遅く
     見せる唯一の向き。フル計測の深度リードバックを、この深度でやり直した
     もの。

5. **2 回目の読みは 1 回目が発火したホストだけが払う。** 通過するホストの
   コストは calibration 1 本のままで、これまでと 1 リクエストも変わらない。
   1 回目は `keep_alive` でモデルを常駐させ、2 回目はロードを払わずに読んで
   から unload する。

6. **窓の分割を CI で固定する。**
   `hostCutoffScreenQuietWait 30s + hostCutoffCalibrationTimeout 180s +
   hostCutoffScreenConfirmTimeout 60s = 270s ≤ hostspeed.InstallWindow 300s`。
   プローブモデルが既にディスク上にあるホストは、ダウンロードが待っている窓
   の中で必ず判定に到達する。定数のどれを動かしてもテストが「窓が閉じない」と
   言う。

## Consequences

- macOS ランナー級のホストは 9 分 29 秒ではなく**2 本の短いリクエスト**で判定に
  到達し、bundled pull は始まらない。Stage 2 で残った穴が閉じる。
- 参照機（下界 25.2 秒、線の 2.7 分の 1）は発火せずフル計測に落ちる。閾値を
  較正した機体について screen は何も言わない。
- **残る穴**: プローブモデルのダウンロードが窓を食った場合、screen も走れない。
  その場合は従来どおり判定不能で、install 経路は変わらない。窓を共有する以上
  これは避けられず、「今日の挙動のまま」であって退行ではない。
- **残る穴 2**: エンジンの外からの負荷（同一ホストで走る無関係なジョブ）は
  `EngineQuiet` から見えない。誤発火するには、通過するはずのホストが 2 回連続
  で 4 倍以上遅く読まれる必要がある。誤発火の代償は #465 のオプトイン付き
  デフォルト off であって拒否ではない（waired#1056）。ばらつきを判定に反映
  させる話は waired-agent#622 で別に追う。
- `hostSpeedStillApplies` は method を見るようになった。下界レコードは
  `HostProbe.Measured()` が false なので、これを教えないと**遅いホストは毎起動
  screen し直す**。
- `ensureHostSpeedMeasured` の戻り値が `hostfit.HostProbe` から
  `hostSpeedVerdict` になった。判定の形が 2 つになった以上、各呼び出し元で
  再導出させると screen の判定はどこでも「未計測」と読まれる。

## Why not the alternatives

- **フル計測の予算を増やす**: 予算は既に 12 分あり、install 経路が使えるのは
  5 分。増やせる先がない。
- **下界を `turn_seconds` に載せる**: additive で安全ではあったが、#579 の
  本質が「1 つの数字が場所によって違う意味を持つ」ことなので、その欠陥を
  訂正不能な wire に移すことになる。オーナー裁定で退けた（#620）。
- **サンプル数を 1 に減らす**: 中央値を捨てることになり、決定 3 が余裕の根拠に
  している「競合下の +21%」を吸収できなくなる。
- **screen だけにしてフル計測をやめる**: 下界は上界を持たないので、通過判定に
  使えない。通過するホストには実測が要る。

## Refs

- https://github.com/waired-ai/waired-agent/issues/579
- https://github.com/waired-ai/waired-agent/pull/617 — `hostfit.TurnFloorSeconds`
- https://github.com/waired-ai/waired-agent/pull/620 — 独立フィールドとするオーナー裁定
- https://github.com/waired-ai/waired-agent/issues/622 — spread_pct を誰も読んでいない
- docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md
