---
status: accepted
supersedes:
  - docs/decisions/20260809/0110-serve-at-the-rung.md
---

# 生成バッチの強制をやめ、エンジンの判断に返す (20260828 19:00)

## Status
Accepted

## Context

waired#642 は、スピルしている discrete GPU ホストで生成 ubatch を 2048 に
強制していた。根拠は「ollama の自動バッチ選択はスピルしたホストで 512 に
落ちるので、上げればプレフィルが +38〜44% 速くなる」という 2026-07 の実測
(pin は 0.31.1)。`num_batch` はサーバレベルの環境変数を持たないため、
`<base>-wb<batch>` という派生モデルを `/api/create` で作って配信する形で
届けていた。

`inference_ollama_derived.go` は、この前提が **#823 の pin 移動時に再確認
されていない**ことを自分で記録していた。再確認した結果、前提は成立して
いなかった。

**ollama 0.32.12 以降(現 pin 0.32.15 も floor 0.32.13 も)、エンジン自身が
生成バッチをサイジングしている。** `server/sched.go` を逐語で:

- `generationBatchForContext` — 窓 > 32768 なら **2048**、> 4096 なら 1024、
  それ以外 512。つまり #642 が強制するのと同じ値をエンジンが自分で選ぶ
- `automaticGenerationBatch` — `generationBatchFits` が否なら
  2048 → 1024 → 512 と**自分で降りる**(2048 は予測 VRAM ≤ 空きの 60%、
  1024 は ≤ 75%)
- `generationBatchSurcharge` — 2048 なら 2 GiB を予測に上乗せする
- `routes.go` の `usesAutomaticNumBatch` — **モデルに `PARAMETER num_batch`
  があると `numBatchAuto = false`** になり、`sched.go` の
  `applyAutomaticGenerationBatch` が早期 return する

派生モデルはまさに `PARAMETER num_batch` を焼き込むので、**強制した瞬間に
エンジンの段下げが丸ごと無効になる**。

### 実測 (sv-mag、ollama 0.32.15、qwen3.8-27b mtp-q4、ctx 200704)

| 構成 | runner の argv | ロード後の空き | ~2k | 26k | **171k** |
|---|---|---|---|---|---|
| 素タグ(エンジンの選択) | `-b 512 -ub 512` | 506 MiB | — | OK 744 tok/s | **OK 395 tok/s** |
| `-wb2048`(強制) | `-b 2048 -ub 2048` | 52 MiB | **OOM** | OOM | OOM |

エンジンが選んだ 512 は **171,449 トークンを通し**、強制した 2048 は
**2,000 トークンを通せない**。sv-evox2(Strix Halo / Windows / 同版 / 同窓)
でも素タグは `-b 512 -ub 512` で、2 台・2 世代・2 OS で同じ挙動。

つまり waired-agent#1038(FIT は収まると言うのに実機が OOM する)の真因は
このオーバーライドであり、**0.32.15 への pin 移動は fit を一切改善して
いなかった**(waired-agent#1054 の記録はこの点で誤っており、訂正済み)。

## Decision

1. **waired#642 のオーバーライドを廃止する。** 生成バッチはエンジンが窓と
   自分のメモリ予測から決める。エージェントは `OLLAMA_*` にバッチ変数を
   一切出さない。
2. 付随して消えるもの: `inference_ollama_derived.go`(派生モデルの作成)、
   `ModelState.BaseOllamaTag` / `ForcedBatchRefusedAt`、
   `ollamaObservedServe.ForcedBatchRefused`、ラダーの `stepTag` 段と
   `ollamaVerifyDeps.ApplyStep`、`ModelTuning.NumBatch` と
   `/inference/status` の `num_batch`、`advertisedEngineTag`
   (派生タグをピアから隠すためだけに存在していた)。
3. **ロード後の空き VRAM 下限 (768 MB) も廃止する。** 較正点が全て
   「もう生成されない構成」のものであり、置き換わった構成は同じホストで
   **506 MB** の空きで 171k トークンを捌く。つまりこの下限は、失敗例を
   1 つも持たないまま、既知の誤検出を 1 つ持つ。空き VRAM は
   `ModelTuning.PostLoadFreeVRAMMB` に**証拠として記録**するだけとし、
   降格は判断しない。
4. **3 OS すべてで安全**であることを確認した:
   - `server/sched.go` に build tag も `runtime.GOOS` 分岐も無い
   - linux(sv-mag)/ windows(sv-evox2)で自動サイジングの発火を実測
   - **darwin では #642 はそもそも発火しない** — `Host.Class()` は
     `UnifiedMemory → ClassUnified` / `GPUCount > 0 → ClassDiscrete` で、
     arm64 Mac は `defaultUMA` が `UnifiedMemory` を立て、Intel Mac は
     `detectApple` が arm64 以外で nil を返し NVIDIA/AMD 検出器も macOS
     では何も見つけないので `GPUCount = 0`。どちらも `ClassDiscrete` に
     ならない

## Consequences

- **スピルしているホストは、エンジンが選んだバッチで動く。** 実測では
  それが動く唯一の構成だった。#642 が買っていた「+43%」は、エンジンが
  意図的に残した余裕を使い切って買っていたものであり、その構成は比較
  できる深さに到達できないので利得自体が測定不能。
- **waired-agent#1064 は issue ごと消える** — 報告すべき intent が無くなる。
- **ラダーは窓段 1 つだけになる。** 262144 ネイティブなモデルは 1 段
  ラダーなので実質「降り先なし」に戻るが、**降りる必要のある構成が
  生成されなくなった**のがこの決定の要点。エンジンは自分の予測に対して
  自分で段を下げる。
- 残る防御線は 3 本: spill 率チェック、深度ベンチの OOM 判定
  (waired-agent#1058)、リクエスト時 OOM (`onEngineFitFailure`)。
- 深度ベンチのキャッシュキーからバッチ項が消えるので、既存の深度計測は
  1 度ミスして測り直す。旧構成で採った値なので、それが正しい結果。
- リリース前のため、既存ホストの移行は行わない(オーナー裁定)。
- 20260809/0110 の §6「#642 バッチ強制は discrete 限定」は、その決定の
  他の部分(rung 固定起動、宣言ゲート、サイジング予算の統一、verify の
  降段と latch)を残したまま、この決定に置き換わる。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1079
- https://github.com/waired-ai/waired-agent/issues/1038
- https://github.com/waired-ai/waired-agent/pull/1054
- docs/knowledges/20260827/1330-qwen38-on-a-24gb-card.md
