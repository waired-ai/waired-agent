---
status: accepted
supersedes:
  - docs/decisions/20260801/1318-recommend-on-resident-weights.md
---

# 容量は計算式・推奨は「200k を宣言できるか」 (20260804 19:37)

## Status
Accepted。方針の一次ソースは waired-ai/waired#1056 のオーナー決定コメント
(2026-08-03) と、その決定ログ
`waired` `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`。
本記録は waired-agent 側の実装決定を固定する。実装は
waired-ai/waired-agent#464、トラッカーは waired-ai/waired#1067。

`docs/decisions/20260801/1318-recommend-on-resident-weights.md` の推奨層
(discrete = 重み常駐 / unified = 常駐 + 公称ピーク速度 / CPU-only = 制約なし)
を置き換える。重み常駐の条件だけは残り、他の 2 つは窓の判定に畳まれた。

## Context

waired-ai/waired#1056 が挙げた 4 ホストは、いずれもローカル推論を丸ごと失って
いた。原因は容量ゲートが「載るか」に答えていなかったこと:

- discrete / CPU-only は手書きの `min_ram_gb` だけで判定し、GPU を見ていない
  (qwen3.5-4b は重み 3.4 GB に対し 8 GB を宣言。約 2.3 倍の余裕がデータに
  焼き込まれている)
- unified はカーブアウト読み値だけで判定
- fit も画面表示も KV を 16,384 トークン分しか数えず、200k 実行時と
  約 2.6 GB 乖離する (qwen3.5-4b: 4,915 MiB 対 7,539 MiB)

## Decision

### 1. 容量 = 総メモリに対する計算 (`hostfit.OllamaCapacityFit`)

`重み + そのホストが実際に実行する窓ぶんの KV + オーバーヘッド ≤ 総メモリ`。
総メモリ = `RAM − OS 分 2 GB + 専用 VRAM`。`min_ram_gb` は計算できない
バリアント (重み注記なし) のフォールバックとしてのみ残る。

**窓は「そのホストが実際に実行する窓」**で、コーディング窓固定ではない。
2 GB のホストが 1 GB のモデルをエンジン既定の 32k で動かせるなら、200k の
KV が入らないことは OOM ではない。表示用の値
(`Presentation.RequiredWindowResidentMB`) は逆に常にコーディング窓で価格を
付ける — ユーザーが読むのは「実務でいくら要るか」だから。

### 2. 統合メモリの合算はプラットフォームではなく「出どころ」で決める

#464 は「Apple Silicon だけが例外」と書いているが、例外はもう 1 つある。
Windows の `defaultUMA` は CPU 型番だけで `UnifiedMemory` を立てるため、
レジストリ値が読めない Strix Halo は Apple と同じく `UsableVRAMMB` を
RAM の 75 % から合成する。合算すると 75 % の水増しになる。

したがって判別軸はプラットフォームではなく**その数値が読み取られたものか
合成されたものか**であり、生産側が量そのものを publish する:
`hardware.Profile.CarveOutVRAMMB` → `signer.HardwareSummary.CarveOutVRAMMB`
→ `hostfit.Host.CarveOutVRAMMB`。0 は「別プールなし」であって
「不明だから推測」ではない。

### 3. 推奨 = 「このホストでコーディング窓を宣言できるか」

`hostfit.OllamaRecommendModel` の 3 条件:

1. モデル自身のコンテキストウィンドウが 200k に届く
2. 重み + オーバーヘッドが GPU アドレス可能メモリに常駐する
3. `hostfit.OllamaDeclaresWindow` — serve tuning と同一の窓サイジングが
   コーディング窓に到達する

3 は serve tuning が呼ぶのと同じ関数 (`OllamaPlannedWindow`)。チューナは
自前のコピーを持つのをやめ、これを呼ぶ。カタログ全体 × 13 ホストで両者の
答えが一致することを `TestDeclaredWindowMatchesTheTuner` が固定する。

### 4. 速度による除外は第 1 段階では行わない

`RankModels` の 3 段目 (#229 デコードフロア) の `narrow` を削除。推定値は
`Pick.DecodeEstimate` と `Presentation.Speed` に載り続け、表示はそのまま。

理由は 2 つ。分母が `BandwidthSystemRAMGBs = 60` という母集団定数で、
`ClassCPUOnly` はこの定数から免除されているのに discrete-spilled は除外に
使われていた — 19.96 tok/s のホストを除外し 17.65 tok/s のホストを通す、
向きの逆転が起きていた (`knownSmallCardBreach` が固定していた defect)。
もう 1 つは decode しか価格を付けておらず、コーディングエージェントの負荷
(prefill 約 21:1) に構造的に盲目であること。

速度は実測が入ってから推奨の入力に戻す (waired-ai/waired-agent#466)。
`PickInput.NoSpeedFloor` は**追加しない**: #464 は escape hatch を求めて
いるが、除外しないゲートに hatch は要らず、何も無効化しないフィールドは
無いより悪い。

### 5. 窓サイジングに単調性の床を入れる (`OllamaPlannedWindow` rule 3)

窓の予算はアクセラレータのメモリなので、システム RAM より小さいカードを
挿すと予算が縮む。64 GB + 8 GB カードは、カード無しの同一機が 60 GB から
サイズするところを 8 GB からサイズし、カード有りの方が小さい窓を宣言する。

そこで **アクセラレータを外した同一機が到達する窓を下回らない** ことを
規則にした。上限はコーディング窓 (それ以上は宣言値が変わらず、余分な
スピルを買うだけ)。既に窓に到達しているホストでは発火しない。

`allowSpill` を尊重する: verify の degrade は「一度失敗したサイジングを
再入しない」ためのものなので、rule 2 と同様に降ろす。

## Consequences

- waired-ai/waired#1056 の 4 ホストがローカル推論を保持する。
  `knownSmallCardBreach` フェンスは空になり削除。
- discrete では容量が緩くなる (6 GB RAM + 8 GB カードが 3.4 GB モデルを
  拒否されなくなる)。意図どおり — ハードゲートは確実 OOM のためだけにある。
- **品質スコアは VRAM に対して単調ではなく、それでよい。** 8 GB RAM +
  2 GB カードは 32k しか実行できないスコア 52 のモデルにフォールスルーし、
  同じホスト + 8 GB カードは 200k を実行できるスコア 42 を推奨する。
  大きいカードがスコアを下げ、答えを良くした。
  `TestInstallPickIsMonotoneOnceRecommended` は「推奨されている限り下がらない」
  を固定する。
- CPU-only は常駐条件から免除されたままで、これは既知の非対称。64 GB の
  CPU-only 機がスコア 89 を、同じ機 + 16 GB カードがスコア 52 を提示される。
  解消には CPU-only 側の実測速度が要る (waired-ai/waired-agent#466)。
- 実機 A/B (RTX PRO 4000 Blackwell 24 GB + 121 GB RAM、qwen3.5-35b-a3b):
  rule 3 が窓を 137,216 → 200,704 に広げ、実測スピル 5.8 % → 9.2 %、
  デコード 123.1 → 99.4 tok/s。選定フロア 60 tok/s の 1.6 倍を保つ。
  **このホストが実際に選ぶモデル (qwen3.6-35b-a3b mtp) のチューニングは
  origin/main と 1 バイトも変わらない**（200,704 / 予測スピル 0.1149）。

## Refs
- waired-ai/waired#1056（オーナー決定コメント 2026-08-03 — 方針の一次ソース）
- waired-ai/waired#1067（実装トラッカー）
- waired-ai/waired-agent#464（本実装）、#465（ラッチ廃止）、#466（第 2 段階: 実測速度）
- waired-ai/waired-agent#459（機種分類のプロパティ化 — Windows 合成経路を追記）
- `docs/decisions/20260801/1318-recommend-on-resident-weights.md`（置き換え元）
- `proto/hostfit/window.go`、`proto/hostfit/hostfit.go`、
  `cmd/waired-agent/inference_ollama_tuning.go`、`internal/router/model_picker.go`
