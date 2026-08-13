---
status: accepted
supersedes:
  - docs/decisions/20260727/1830-ollama-vram-pool-nvidia-only.md
---

# ollama の VRAM 予算は total ではなく free で測る。床の基準も free に移す (20260813 11:20)

## Status
Accepted

`docs/decisions/20260727/1830-ollama-vram-pool-nvidia-only.md` の **§4（安全性は
accessor の床で担保する）だけ**を置き換える。同決定の §1〜§3（プールを proto 側で
導出する / 合算対象は当面 NVIDIA のみ / 控除はデバイス増分あたり）はそのまま有効で、
1830 は `accepted` のまま残る。

## Context

`hostfit` はこれまで VRAM 予算を **total** で測っていた。一方 ollama の
スケジューラが見るのは **free** である。`docs/knowledges/20260727/1830-ollama-multi-gpu-placement.md`
が固定エンジン（0.31.1）のソースを読んで記録している既知のずれ、逐語:

> スケジューラが見るのは **free** メモリ、こちらが合算するのは **total**。1 枚の
> ときからある差だが、枚数分だけ拡大する（#69）。しかも
> `bestGPUGroupByAvailableMemory` には `bestSingleGPUFit` のような 80% の割引がない。

害は具体的である。ディスプレイを駆動している 8 GB カードは、コンポジタと他プロセスが
数百 MB〜数 GB を保持したまま「8 GB 使える」と値付けされる。モデルとコンテキストは
その楽観的な数字で選ばれ、実際にロードすると spill し、#621 の post-load verify が
気づいてコンテキストを縮め、エンジンを 1 回再起動する。**選定は直らない** — verify が
直せるのは窓であってモデルの選択ではない。

1830 §4 はこの床を置いた:

> `OllamaVRAMBudgetMB()` は (a) unified ホストでは `UsableVRAMMB` を必ず優先し、
> (b) **単体デバイスの値を下回らない**。……これがあるので `OllamaVRAMPoolMB` が
> どう間違っていても今日の挙動より悪くならない。

当時それは正しかった。**当時直していた誤りが「複数枚での過小評価」だけだったから**である。
床は「プール計算がどう間違っても 1 枚分は必ず確保される」ことを保証し、
waired#942 の逆向き（動くものを断る）に転ぶのを構造的に防いでいた。

**#69 が直す誤りは向きが逆で、同じ床がそれを丸ごと飲み込む。** 1 枚のホストでは
`VRAMPoolMB == 0` なので予算は `EffectiveVRAMMB`（= total）に落ち、free は式に
現れない。複数枚でも、free 合算が 1 枚の total を下回った瞬間にクランプされる。
つまり **床を total に置いたまま free を導入しても、#69 の主症例には何も起きない。**

## Decision

1. **デバイス単位の貸出可能量を `free`（測れたとき）とする。** `Device.lendableMB()` は
   `VRAMFreeMB > 0 && VRAMFreeMB < VRAMTotalMB` のときだけ free を返し、それ以外は
   total。`OllamaVRAMPoolMB` はこれを合算する。**de-rate は合算の前・デバイスごと**に
   効く（1830 の Consequences が #69 の担当範囲として明示的に残した形）。

2. **床の基準を「1 枚の total」から「1 枚の free（測れたとき）」に移す。**
   `Host.ollamaSingleDeviceMB()` を新設し、`OllamaVRAMBudgetMB` はそれを床に使う。
   床という**構造は残す** — 変わるのは何に対する床かだけである。

3. **`EffectiveVRAMMB` は動かさない。** 1830 が「`min_vram_mb`、エンジン選択、vLLM の
   TP=1 フォールバックは全て『1 枚の値』として書かれている」と述べたとおりで、
   その判断は今も有効。free 対応は ollama 予算の経路にだけ入れる。

4. **unified-memory ホストは対象外。** `UsableVRAMMB` が既に共有プールから GPU が
   wire down できる正直な上限であり、出荷済みのどの検出器も UMA デバイスの free を
   報告しない。改善する余地がなく、フォールバックは推測になる。

5. **測定は 1 回きり、自分のエンジンが weights を持つ前。** ハードウェアプロファイルは
   TTL で再サンプルされるので、ロード後に取った free は**自分の重みを除外する** —
   再 tune のたびに空きが減って縮み続ける螺旋になり、ホストが自分の serve している
   モデルを自分に課すことになる。`RAMAvailableGB` が #568 で同じ危険を同じ言葉で
   名指しし、同じ答え（1 回測って永続化・ライブでは読まない）を採っている。それに倣う。

6. **`0` は「free 未測定」であって「空きゼロ」ではない。** total にフォールバックする。
   free を報告しないドライバも、フィールドを知らない旧 agent も、ここに着地して
   今日の予算を保つ。これが 5 と合わせて、**測っていないホストを de-rate しない**
   ことを構造的に保証する。

## Consequences

- **1830 §4 の「今日の挙動より悪くならない」保証は失効する。** それが目的である。
  予算は測定された free の分だけ**下がりうる**。代わりの保証は 6 の
  「測っていなければ動かさない」— 悪化しうるのは、ドライバが実際に空きを報告した
  ホストに限られる。
- `TestOllamaBudgetNeverShrinksTheHost`（`OllamaVRAMBudgetMB() >= EffectiveVRAMMB()` の
  全数掃引）は**意図的に反転する**。新しい不変条件は
  `OllamaVRAMBudgetMB() >= ollamaSingleDeviceMB()` かつ
  `ollamaSingleDeviceMB() <= EffectiveVRAMMB()`。
- 過小評価に転ぶ経路が新しく開く（測定時に他プロセスが一時的に VRAM を掴んでいた
  場合）。5 の「静かな瞬間に 1 回測る」がその窓を狭める唯一の手段であり、
  #568 が RAM について既に受け入れたのと同じトレードオフである。
- エンジンとの整合は改善する。`bestSingleGPUFit` は free の 80% で判定するので、
  free で測る予算は依然としてエンジンより**楽観的**なまま — つまりこの変更は
  エンジンより厳しくなる方向には行き過ぎない。
- 1830 の Consequences が挙げた残件のうち、`#266`（カード別帯域テーブル）と
  「`EffectiveVRAMMB` が列挙順の `GPUs[0]` を信じている件」（#264 調査項目 6）は
  **未着手のまま**。この決定はどちらにも触れない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/69
- https://github.com/waired-ai/waired-agent/issues/264
- https://github.com/waired-ai/waired-agent/issues/568 — 同型の「1 回測って永続化」
- `docs/decisions/20260727/1830-ollama-vram-pool-nvidia-only.md`（§4 を置換）
- `docs/decisions/20260727/1240-host-fit-single-source-proto-hostfit.md`
- `docs/knowledges/20260727/1830-ollama-multi-gpu-placement.md`
