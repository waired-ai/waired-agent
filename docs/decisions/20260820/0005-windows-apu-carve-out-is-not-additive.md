---
status: accepted
supersedes:
  - docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md
---

# Windows の APU カーブアウトは加算されない — 実測で分かれた出どころの解釈 (20260820 00:05)

## Status
Accepted。`docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md`
の決定 2 を**部分的に**改める。判別軸を「プラットフォームではなく数値の
出どころ」に置く方針は据え置き、**Windows のレジストリ読み値
(`HardwareInformation.qwMemorySize`) を「OS が RAM から除外した、モデルが
追加で占有できるメモリ」に分類していた点**だけを取り消す。同決定の 1
(容量 = 計算式)・3 (推奨 = 200k 宣言) は不変。

private 側の `waired` `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`
Context §1 は「Strix Halo は報告 RAM がカーブアウト控除後なので合算が
ちょうど物理プール」と書いており、Windows についてはこの実測が否定した。
リポジトリを跨ぐ supersede はリンクにできないため散文で記す。

## Context

`sv-evox2` 相当のホスト (Ryzen AI Max+ 395 / 128 GB) で、AMD Variable
Graphics Memory の値だけを変えて同じモデル (`qwen3.5-122b-a10b` q4、
76.3 GB) を同じエンジンでロードした
(waired-ai/waired-agent#863)。

| | カーブアウト 96 GB | カーブアウト 512 MB |
|---|---|---|
| OS 可視 RAM | 31.65 GB | 127.15 GB |
| ロード | 失敗 (27.9 分) | 15.0 秒 |
| decode | 到達せず | 26.32 tok/s |
| runner 実常駐 | 0.00–0.53 GB | 76.95 GB |
| GPU dedicated / shared | 74.61 / 0.80 GB | 0.33 / 75.05 GB |
| ページファイル使用 | 66 % | 0 % |

Windows のグラフィックスアロケーションはページ可能で、ビデオメモリ
マネージャが退避できる必要があるため、確保時に同量のシステムメモリの
コミット (backing store) が課される。カーブアウト 96 GB では重みが
カーブアウトに入りつつ 74.8 GB のコミットが 31.65 GB しかない OS 側に
課され、常駐できずページファイルへ落ち、続いてカーブアウトからも退避
された (dedicated 74.61 → 1.33 GB)。以後はページフォルトの往復になる。

ストレージは別に否定した: 同じ 75.8 GB の blob を unbuffered で読むと
3,636 MB/s (21.3 秒) 出る。失敗時の 3〜15 MB/s はページングである。

## Decision

1. **Windows の Strix Halo では、カーブアウト読み値を予算にも加算値にも
   使わない。** `internal/hardware.strixHaloUMA` は `GOOS` を取り、Windows
   では `UsableVRAMMB = OS 可視 RAM − OS 取り分`、`CarveOutVRAMMB = 0` を
   返す。OS 取り分は `hostfit.Host.OSMemoryDeductionGB()` をそのまま使い、
   容量ゲートと二重の見解を持たない。
2. **`proto/hostfit` は変えない。** `TotalMemoryMB` の `ClassUnified`
   加算は `CarveOutVRAMMB > 0` でのみ発火するので、生産側が 0 を publish
   すれば算術は正しくなる。#863 が挙げた「`hostfit.Host` が GOOS を
   持たない」問題は、**持たせないことで**解ける — 1937 決定 2 の
   「出どころで判別する」がそのまま働く。
3. **予算は 75 % ヒューリスティックではなく「RAM − OS 取り分」。**
   Windows には macOS の wire-down 上限に相当する制限が無く、GPU が
   確保できる上限と総メモリの上限は同じ量になる。75 % を採ると 96 GB
   構成の予算が 24.96 GB になり、6 月のホスト裁定で既定に選ばれ実測
   74.27 tok/s で動いていた `qwen3.6-35b-a3b` (200k 窓で所要 25.73 GB) が
   推奨から落ちる。実際に動いていたものを落とすのは訂正ではなく退行。
4. **Linux は変えない。** amdgpu は GTT でシステムメモリへ届き、恒久予約が
   無い。AMD 公式は「BIOS 予約は小さく、GTT を大きく」を推奨しており、
   本コードは GTT をどこも読んでいない。Windows の実測を根拠に Linux の
   算術を動かさない。未検証であることは waired-ai/waired-agent#868 に
   起票して残す。
5. **記録であって規約ではない。** 1 台の実測と、WDDM についてのみ文書化
   された機構に基づく。コード・テストのコメントは「今日の挙動の記録」
   として書き、プラットフォーム契約とは書かない。

## Consequences

- カーブアウトを大きく取った Windows ホストは、載らないモデルを Fits と
  言わなくなる。拒否ではなく `insufficient_memory` として既存の
  「警告 + 明示確認」経路に乗る (waired#1067 /
  `waired` `docs/decisions/20260808/2325-capacity-warns-and-asks-not-refuses.md`)。
  新しい理由コードは要らない。
- カーブアウトを小さく取った Windows ホストは、嘘のスピル警告
  (「about 100% of the model is expected to sit in system RAM」) を出さなく
  なり、`OllamaResident` が `insufficient_vram` を返さなくなる。
- そのスピル判定が `RecommendedMaxParallel = 1` の早期 return を踏んで
  隠していた waired-ai/waired-agent#846 が再び到達可能になる。同じ変更で
  エンジンの観測値 (`ObservedNumParallel`, #763) を次の見積もりへ戻し、
  一度断られたスロットを再要求しないようにした。
- 制御プレーンは `signer.HardwareSummary.CarveOutVRAMMB` を 0 で受け取る
  ようになるため、エージェント更新後は CP 側の判定も同じ値に揃う。
  CP のコード変更は要らない。
- レジストリ読み値は `GPUs[0].VRAMTotalMB` に残る。運用者にこのホストの
  挙動を説明する事実そのものだからである。
- `proto/hostfit` の `CarveOutVRAMMB` doc と
  `proto/hostfit/capacity_test.go` のコメントは「Windows の qwMemorySize が
  設定する」と述べており、本決定後は Linux のみになる。proto は単独 PR
  必須なので追随は分けた。

## Refs
- https://github.com/waired-ai/waired-agent/issues/863
- https://github.com/waired-ai/waired-agent/issues/837
- https://github.com/waired-ai/waired-agent/issues/846
- https://github.com/waired-ai/waired-agent/issues/868
- docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md
- https://learn.microsoft.com/en-us/windows-hardware/drivers/display/sharing-backing-store-with-kmd
