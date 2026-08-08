---
status: accepted
---

# 容量ゲートは「実際に serve する窓」で価格付けする (20260808 19:07)

## Status

Accepted。2026-08-03 のオーナー決定（waired-ai/waired#1056 決定 1、`waired`
`docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`）の「拒否は確実な OOM
だけ」というルールは維持し、**「serve できる」の定義を変える**。結果として
`OllamaCapacityFit` の価格付け対象が、見積もりが縮めた窓から**製品が実際に serve
する窓**へ移る。これは同決定の下で明示的に選ばれていた挙動の反転にあたる。

## Context

macOS の `install+inference` レグが 3 連続で赤（run 31030433108 / 31094256263 /
31164150206）。7 GiB の Apple Silicon ランナーが qwen3.5-4b を渡され、ロードは
成功したうえで最初の生成が `HTTP 500` を返していた（#552）。

### ゲートは構造的に何も拒否できなかった

`OllamaCapacityFit` は `OllamaPlannedWindow` の出力で価格付けしていた。

```go
plan := OllamaPlannedWindow(m, v, h, OllamaKVFactorQ8_0, true)
return ollamaCapacityAtWindow(v, h, plan.ContextLength)
```

見積もりは**収まる最大の窓**を選ぶので、その窓が収まるかを問い直すのは既に yes と
答えられた質問である。**縮めれば必ず通る。**

ユニファイドメモリではさらに退化する。見積もりは
`OllamaVRAMBudgetMB − OllamaVRAMOverheadUMAMB` を使い切るので、必要量は
`OllamaVRAMBudgetMB` そのものに戻り、それを `TotalMemoryMB()` と比べる。すなわち
判定は

```
floor(3R/4)·1024  ≤  (R−2)·1024
```

に還元され、両辺は **5 ≤ R ≤ 8 で等しい**（`UsableVRAMMB` は macOS で
`RAMTotalGB * 3 / 4 * 1024`、整数除算）。7 GiB ランナーの余白は **5 MiB** だった。

### 窓は 2 つしかない

ノードが宣言できる窓は 200,704 か 1,048,576 か 0 のいずれか（waired#1031）で、
`DeclaredContextWindow` は 200,704 未満なら 0 を返す。コーディングセッションは
200k rung 向けに設計されている（#624）。**rung の間の窓は「小さい版の製品」では
なく、メッシュがルーティングできず、コーディングエージェントが仕事をできない窓**
である。

7 GB Mac で qwen3.5-4b を 200,704 で serve するには **7403 MiB** 必要で、見積もり
予算は 4096 MiB。1.8 倍で、届く/届かないの話ではない。54,272 という数字は
「載らないモデルを載せるために窓を削った跡」だった。

## Decision

1. **`OllamaServedWindows(m)` を新設し、容量ゲートをそのはしごで価格付けする。**
   はしごは高い順に `ServingWindow1M`（1M ネイティブのモデルのみ）、
   `ServingWindow200k`、そして rung 未満のモデルについてはモデル自身の窓。
   どの rung にも収まらなければ拒否する。

2. **`OllamaPlannedWindow` にも同じ天井を入れる**（`OllamaCeilingWindow`）。
   262144 ネイティブのモデルを大きなホストで 262,144 で serve するのをやめる —
   `DeclaredContextWindow` が 200,704 を超えて名乗ることは決してないので、差分の
   61,440 トークン（4b の q8_0 KV で約 960 MiB）は消費者のいない窓だった。

   **無料ではない。** ローカルのオーバーフローガードは `ContextWindowFor` 経由で
   適用済みの窓を読むので、ローカル要求が 400 を受けて Claude Code が
   auto-compaction に入る閾値も下がる。それがトレードそのものである — 製品は
   2 つの窓を serve するので、セッションはどちらにせよ rung 向けに設計される。

3. **`computeOllamaTuning` の自動並列ゲートを天井比較に移す。**
   条件は `ctx == m.ContextLength` だった。(2) の後は 262144 ネイティブのモデルで
   決して等しくならず、2 スロット目が黙って消える。

4. **OS 別の扱いは入れない。** `proto/hostfit` には OS が登場せず、分岐は
   `Host.Class()` だけ。全 bundled モデル × ホスト種別のスイープで、判定が動いた
   11 件は CPU 専用 / ディスクリート / ユニファイドの 3 クラスすべてにまたがる。
   Linux・Windows・macOS は同じマシンに同じ答えを返す。

5. **4-5 GB のホストはローカル推論を持たない。** カタログ最小の qwen3.5-0.8b でも
   rung には 3154 MiB 必要で、CPU 専用 6 GB が下限になる。それ未満のホストは
   `BelowFloorModelID` すら提示されない。スペック的に無理なので、どこかで線を
   引く必要がある（オーナー判断 2026-08-08）。#465 の opt-in の道は残るが、その
   ホストでは `waired inference on` の後に `waired models pull` が別途必要になる。

6. **推奨要件のメッセージを実測基準に直す。** `belowRecommendedSpecNeed` は
   手書きの `min_ram_gb` を引いていて、8 GB ホストに
   `needs ≥ 4 GB RAM; this host has 8 GB` という意味の通らない文言を出していた。
   拒否に使ったのと同じ算術で計算する。

## Consequences

スイープで動いた判定は **11 件、すべて `true → false`**（新たに許可されるものは
ゼロ）。全件が「エンジン最小の 32,768 付近まで窓を削って載せていた」ケース。

| ホスト | モデル | 従来の価格付け窓 |
|---|---|---|
| CPU 8 GB | qwen3.5-4b | 35,840 |
| ディスクリート 16 GB + 8 GB カード | qwen3-coder-30b-a3b / qwen3.5-27b / qwen3.6-27b ×2 | 32,768 |
| Mac 8 GB | qwen3.5-4b | 119,808 |
| Mac 24 GB | qwen3-coder-30b-a3b / qwen3.5-27b / qwen3.6-27b ×2 | 32,768〜59,392 |
| Mac 64 GB | gpt-oss-120b | 32,768 |

- **7-8 GB のユニファイドホストと 8 GB の CPU ホストは qwen3.5-2b に落ちる。**
  ウィンドウは 119,808 → **262,144**（ネイティブ全域）に**広がる** — 4b の
  ハイブリッド構成は KV が 32768 B/token、2b は 12288 B/token。
- 24 GB Mac は 27B クラスを失う。37,888 トークンの窓で 27B を持つより、200,704 で
  9b を持つほうが製品として正しい。
- 12 GB 以上で 4b を持っていたホストの判定は変わらない。
- 反転した既存テスト 2 本には、それぞれ本文中に理由を書いた
  （`TestOllamaCapacityFit_PricesAWindowTheProductWouldServe`、
  `TestBundledCatalog_TheSymptomHostsKeepLocalInference` の 8 GB 行）。後者が
  約束していること — waired#1056 の症状である「ローカルモデルが 1 つも無い」に
  戻さないこと — は変わっていない。

### この決定に含まれないもの

- **OS フットプリントの実測。** `OSMemoryAllowanceGB = 2` は 3 OS 共通の推測で、
  macOS ランナーの実測とは合っていない。`hardware.Profile.RAMAvailableGB` は
  3 OS すべてで available 指標（Linux `MemAvailable` / macOS `vm_stat` の
  free+inactive+speculative+purgeable / Windows `AvailPhys`）として取れており、
  どれもプリロードキャッシュを空き側に数えるので `total − available` は OS 実
  フットプリントの下限になる。`max(OSMemoryAllowanceGB, total − available)` に
  すれば OS 別定数を発明せずに済むが、`signer.HardwareSummary` への追加フィールド
  と、スナップショットの揺れを許容するかの判断が要る。**#552 はこの項に依存
  しない** — 7 GB Mac の 4b は OS 取り分を 0 にしても（`have` 7168 < 7403）拒否
  される。別 PR とする。
- **`InstallQualityFloorTier`（30）の扱い。** 品質スコアの数値自体を廃止する方向で
  検討中のため、今回は触らない（オーナー判断 2026-08-08）。

## Refs

- waired-ai/waired-agent#552（起点）、#549（露出させた PR）、#557（診断の修正）
- waired-ai/waired#1056 / `waired` `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`
- waired#1031（2 つの窓）、#624（コーディング窓）、#424（UMA オーバーヘッド
  1024 MiB）、#465（opt-in）、#517（品質スコアの下限）
- コントロールプレーンの `recommendedModel` も同じ `proto/hostfit` を呼ぶため、
  この決定が CP に効くのは `waired` が新しい proto タグに上がってから。
