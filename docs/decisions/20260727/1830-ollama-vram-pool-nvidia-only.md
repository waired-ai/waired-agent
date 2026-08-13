---
status: accepted
superseded_by:
  - docs/decisions/20260813/1120-ollama-budget-sized-on-free-vram.md
---

# ollama の VRAM プールは proto 側で導出し、当面 NVIDIA に限る (20260727 18:30)

## Status
Accepted（一部置換）

**§4「安全性は数値の精度ではなく accessor の床で担保する」だけ**が
`docs/decisions/20260813/1120-ollama-budget-sized-on-free-vram.md` に置き換わった。
床という構造は残り、**何に対する床か**が「単体デバイスの total」から
「単体デバイスの free（測れたとき）」に移っている。§4 の「今日の挙動より
悪くならない」保証はそこで意図的に失効する — この決定が直していた誤りは
複数枚での**過小**評価だけであり、#69 が直すのは 1 枚での**過大**評価で、
total に置いた床は後者を丸ごと飲み込むため。

§1（プールを proto 側で導出し wire に新フィールドを足さない）、§2（合算対象は
当面 NVIDIA のみ）、§3（控除はデバイス増分あたり）は**そのまま有効**。
Consequences が「#69 の担当範囲として残す。混ぜると両方おかしくなる」と
指定した分離も、そのとおりに守られている（de-rate は合算の前・デバイスごと）。

## Context

`proto/hostfit.Host` は GPU 側を `GPUCount` と `VRAM0MB`
(= `GPUs[0].VRAMTotalMB`) だけで表しており、`EffectiveVRAMMB()` は
discrete ホストでその生の値を返す。2×24 GB のホストが 24 GB として
値付けされる（#264）。

害はカード上の数字が違うことではない。#229 以降、
`EstimateOllamaDecode` の discrete-spilled 分岐だけが
`Estimate.UpperBound` を立て、それがモデルを**除外**できる唯一の
根拠になっている。VRAM を過小に数えるとその条件を人工的に作り出し、
2 枚に載りきるモデルが spilled と判定されて推薦から落ちる。
waired#942（動かないものを勧める）の鏡像で、こちらは**動くものを
断る**。

前提だった「ollama は分散しない」は誤りだった。ピン留めしている
0.31.1 のスケジューラは、1 枚に載らないときに `ml.ByLibrary`
グループを丸ごと使い、その空きメモリを合算する。詳細は
`docs/knowledges/20260727/1830-ollama-multi-gpu-placement.md`。

## Decision

### 1. プールは wire に新フィールドを足さず、proto 側で導出する

issue の当初案は agent が `vram_pool_mb` を publish するものだった。
採らない。`signer.HardwareSummary.GPUs` は**全デバイス**の
`{Model, VRAMTotalMB, ComputeCap, Vendor}` を最初から運んでおり、
捨てていたのは両側の adapter だけだったから。

結果として:

- 新しい wire フィールドが無い → producer を後続 PR に負債として
  残す仕組み（protoconsumer の exemption + `notPublishedByAgent`）を
  使わずに済む。#251 でその仕組みは実際に落ちている（#272 が
  feature branch に merge され main に来なかった）。
- CP が proto タグを上げた瞬間に、**既に配布済みの全 agent** の
  判定が直る。publish 方式なら各ホストの更新を待つことになる。
- ルールは 1 つ、adapter は各サイド 1 つずつ
  （`docs/decisions/20260727/1240-host-fit-single-source-proto-hostfit.md`
  がまさにこれを約束している）。

`Host` は `!=` で比較されるのでデバイス列は載せられない。`Device` と
`OllamaVRAMPoolMB` を hostfit に置き、両 adapter が同じ関数を呼ぶ。

### 2. 合算対象は当面 NVIDIA デバイスのみ

`filterIntegratedGPUs` は integrated を落とすが
`integratedGPUAllowedByDefault` が `"CUDA"` を**常に許可**する。
NVIDIA については integrated / discrete の区別がそもそも判定に効かない
ので、`hardware.GPU` に `Integrated` を足さずに正しくいられる。
`ml.ByLibrary` がベンダをまたがないことと合わせて、issue の調査項目
3（ベンダ混在）と 4（iGPU 除外）は「推測した」ではなく「問題に
ならない範囲を選んだ」で閉じる。

AMD へ広げるには `Integrated` が**検出された事実**として要る。モデル名の
正規表現（`internal/runtime` の `amdIsIntegratedModel`）を contract
モジュールに複製するのは、この package が防ぐために存在する drift
そのものなので採らない。

### 3. 控除はデバイス「増分」あたり

`sum - (n-1) * OllamaVRAMOverheadBaseDiscreteMB`。base 項は
カードごとのデバイスコンテキストなので枚数分繰り返す。一方
`OllamaVRAMOverheadPerWeightGBMB` の傾きは compute/scratch で
レイヤと一緒に分割されるので繰り返さない。`OllamaVRAMOverheadMB` が
その上で base + 傾きを 1 回引くので、合計は `n*base + 傾き` になる。

### 4. 安全性は数値の精度ではなく accessor の床で担保する

`OllamaVRAMBudgetMB()` は (a) unified ホストでは `UsableVRAMMB` を
必ず優先し、(b) **単体デバイスの値を下回らない**。(b) は
`router.VLLMVRAMBudgetMB` の床と同じ形で、これがあるので
`OllamaVRAMPoolMB` がどう間違っていても今日の挙動より悪くならない。
過小評価を直しながら過大評価（waired#942 の向き）に転ぶことが
構造的に起きない、というのが採用の決め手。

`EffectiveVRAMMB` は動かさない。`min_vram_mb`、エンジン選択、vLLM の
TP=1 フォールバックは全て「1 枚の値」として書かれている。

## Consequences

- `Host.GPUCount` と `VRAMPoolMB` は食い違ってよい。前者は検出した
  アクセラレータ数、後者は 1 つのエンジンが実際に広げられる範囲。
- 1 枚のホストは `VRAMPoolMB == 0` になり、挙動は完全に据え置き。
- #69（total ではなく free を見るべき）の誤差が枚数倍になる。プール
  する側で相殺せず、#69 の担当範囲として残す。混ぜると両方おかしくなる。
- discrete-resident 分岐の「載るなら十分速い」は**デバイス単位**の
  論拠で、プールはそれを弱める（レイヤ分割は容量をほぼ倍にする一方
  帯域は据え置き）。この分岐は `UpperBound` を立てないので過大注釈
  止まりで除外はしないが、discrete のカード別帯域テーブル（#266）が
  最初に効くのはここ。**この PR では実装しない**（issue 調査項目 2・5）。
- `EffectiveVRAMMB` が列挙順の `GPUs[0]` を信じている件（調査項目 6）
  は据え置き。ollama 経路はプール経由で列挙順から独立するが、
  `PrimaryGPUVendor` / `PrimaryGPUModel` は依然 `GPUs[0]` で
  バックエンドを決める。別 issue。

## Refs

- https://github.com/waired-ai/waired-agent/issues/264
- https://github.com/waired-ai/waired-agent/issues/229
- https://github.com/waired-ai/waired-agent/issues/266
- https://github.com/waired-ai/waired-agent/issues/69
- `docs/knowledges/20260727/1830-ollama-multi-gpu-placement.md`
- `docs/decisions/20260727/1240-host-fit-single-source-proto-hostfit.md`
