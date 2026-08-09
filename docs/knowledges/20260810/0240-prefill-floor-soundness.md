# プレフィルのみの下界がなぜ健全か (20260810 02:40)

## Issue

waired-agent#579 Stage 3 で、`hostfit.TurnFloorSeconds` = `P/prefill` を
「このホストは推奨要件未満」の根拠として使う。次にこのコードを触る人が
「浅い測定で結論していいのか」を §11 やコミット履歴から再導出しなくて済む
ように、根拠を 1 か所に残す。

## Learnings

### 1. 片側でしか使わない

```
floor >  閾値  ⇒  turn > 閾値      健全。これが判定
floor <= 閾値  ⇒  何も言わない      フル計測に落ちる
```

逆向き（`floor <= 閾値 ⇒ turn <= 閾値`）は**偽**であり、どこでも使っていない。
下界は上界を持たないので、ホストを**通す**判断には使えない。

### 2. 健全性は独立な 2 つの事実の積み上げ

**(a) decode 項を落とす。** 判定量は

```
turn = P/prefill + (P/21)/decode        （P = HostCutoffProbeDepthTokens = 21000）
```

decode 項はデコードするホストなら厳密に正なので、落とせば必ず小さくなる。

**(b) 浅い深度で測る。** プレフィル率は深度に対して単調非増加。本リポジトリの
実測（`cmd/waired-agent/host_cutoff_probe.go` の `hostCutoffCalibrationLines`
のコメント）で、参照機は

- 68% 深度: **833 tok/s**
- 全深度: **671 tok/s**

つまり `prefill@2.8k ≥ prefill@21k`。浅い測定は率を**過大**評価し、過大評価で
割れば時間は**過小**評価される。

2 つとも同じ向き（下界を小さくする向き）に働くので、積み上げても向きは
変わらない。

### 3. 発火線が 45 秒ではなく 67.5 秒である理由

`HostCutoffTurnBudgetSeconds × hostCutoffScreenMargin = 45 × 1.5 = 67.5`。

理由は 1 つだけ: **参照機の実測ターン 66.6 秒より上に置く**こと。参照機は
cutoff が正しく落とす機体（同じ機体で実モデルを回すと tier 89 で 227 秒）
なので、この線より上で発火する screen は「既に使えないと実証済みの機体より
さらに遅いホスト」についてしか結論しない。

45 秒と 67.5 秒の間は、下界と実測が「45 秒のどちら側か」で食い違い得る帯で、
そこはフル計測が決める。

数字で見ると:

| ホスト | prefill@screen | floor | 発火? |
|---|---|---|---|
| 参照機 (CPU-only, 66.6 s turn) | 833 tok/s | 25.2 s | しない（線の 1/2.7） |
| GitHub macos-14 ランナー | ~130 tok/s | ~161 s | する（線の 2.4 倍） |
| 実機 24GB Blackwell | 19700 tok/s | 1.1 s | しない |
| 実機 Apple 16GB | 1269 tok/s | 16.5 s | しない |
| 実機 RTX 4070 Laptop | — | — | しない |

実機3台は rc8 検証（edge c0e2a1f）で並行セッションが実測したもの。いずれも
線の 1/4 以下で、誤発火の余地がない。

### 4. 弱い証拠は前提条件の強さで埋める

下界は実測より弱い証拠なので、結論するのに 4 つの前提を課している。**うち
1 つは実装の順序から要求されたもので、見落としやすい**:

`EngineQuiet` は**待つ**のであって 1 回見るだけではない。screen の直前は
`ensureHostCutoffProbeModel` で、新規インストールでは ~1 GB を引く。
`endPull` はモデルが landed すると serve reconcile を発火し、reconcile は
エンジンを停止・再起動する。したがって **screen が最初に走れる瞬間は、
しばしばエンジンが再起動されている最中**である。1 回チェックするだけの実装は
そこで「busy」と答え、**screen が存在する理由そのものである install 経路で
screen を降ろす**。

`hostCutoffScreenQuietWait = 30s` はそのための待ちで、大きさは実測ではなく
窓の分割から来ている（`30 + 180 + 60 = 270 ≤ hostspeed.InstallWindow 300`）。

### 5. 保存則: `turn_seconds` は実測だけを指す

下界は `signer.HostSpeed.TurnFloorSeconds` に載り、`TurnSeconds` は 0 のまま
（オーナー裁定 2026-08-09、#620）。副作用として `HostProbe.Measured()` が
false になり、**wire から素朴に `HostProbe` を組み直した consumer は判定を
降りる**。これは事故ではなく設計で、教えられていない consumer が下界を実測と
読んで「このホストは速い」と誤解するより望ましい。

同じ理由で `hostSpeedStillApplies` に method を教える必要がある。教えないと
下界レコードは `Measured()` false なので毎起動 re-screen される。

## Refs
- https://github.com/waired-ai/waired-agent/issues/579
- https://github.com/waired-ai/waired-agent/pull/617
- https://github.com/waired-ai/waired-agent/pull/620
- docs/decisions/20260810/0228-prefill-floor-screens-below-spec-hosts.md
- docs/decisions/20260805/1620-host-cutoff-is-a-measured-probe.md
