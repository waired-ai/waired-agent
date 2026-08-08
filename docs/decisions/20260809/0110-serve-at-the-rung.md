---
status: accepted
---

# エンジンは rung で起動する — sub-rung 切り詰めの撤廃 (20260809 01:10)

## Status
Accepted

## Context

waired-ai/waired#1067 の 2026-08-08 オーナー裁定(R4)は、コンテクスト
ウィンドウの起動時切り詰めを完全撤廃し、エンジンには
`OllamaServedWindows` の rung(200,704 / 1M、sub-200k ネイティブは自身の
窓)を固定で渡すと定めた。同裁定は容量ゲート 2 GB / サイジング 4 GB の
控除非対称の解消(双方とも実測 OS 控除 `Host.OSMemoryDeductionGB` に統
一)も含む。前提となる実測控除は #593(#568)で着地済み。追跡 issue は
waired-ai/waired-agent#587。

rung 間の窓は「小さい版の製品」ではない: mesh はルーティングできず、
コーディングセッションは収まらない。従来の連続サイジング
(`OllamaPlannedWindow`)はそうした窓を選んで起動していた。

## Decision

1. **rung 固定起動**: 新 `hostfit.OllamaPlannedRung(m, v, h, kvFactor,
   ceiling)` が唯一のサイジング。ラダーの最上位から、(1) 予算内で丸ごと
   収まる、(2) discrete のみ・有界 spill(上限
   `OllamaMaxExpectedSpillFraction`)、(3) discrete のみ・カード非搭載
   時に到達可能(rule 3、単調性維持)の3規則で到達可否を判定し、合格す
   る最上位 rung を返す。どの rung も不合格なら **最下位 rung を
   `Fits=false` で返し、それでも起動する**(sub-rung 窓は存在しないた
   め)。spill は「選択」から「rung での予測・報告」に変わった。
2. **宣言ゲート**: `ModelTuning.WindowFits`(= plan.Fits)を新設し、
   `DeclaredContextWindow` は WindowFits=false の窓を宣言しない。強制
   rung は自機用に serve されるが mesh には載らない(waired#1031 の窓
   契約)。<200k→0 は安全網として残置。
3. **サイジング予算の統一**: `OllamaSystemRAMBudgetGB` は
   `RAMTotalGB − OSMemoryDeductionGB`。`OllamaSizingBudgetGB` の RAM
   側もアクセラレータ側と同じくエンジン留保
   (`OllamaVRAMOverheadMB`)を明示控除(旧 4 GB 定数に内包されていた
   留保の明示化)。`OllamaCPUOnlyRAMHeadroomGB` は Deprecated 残置。
4. **verify の降段と latch**: spill 判定の縮小再起動は「rung を1段降り
   て1回だけ再起動」に変更。最下位 rung では **再起動せず latch** —
   警告を記録し、WindowFits を落として宣言を止め、エンジンは serve を
   続ける。再起動後もなお degraded の場合も同じ latch。
   **挙動変更**: 従来は最下位でも no-spill 窓へ縮小再起動していた。
   可用性(serve 継続)は上がり、宣言は誠実側に倒れる。
5. **Deprecated 残置**(protoguard により削除不可):
   `OllamaPlannedWindow`(挙動は共有予算に追随するため凍結ではない)、
   `OllamaMaxContextAtSpill`、`OllamaMinContextTokens`、
   `OllamaCPUOnlyRAMHeadroomGB`。`OllamaMaxExpectedSpillFraction` は
   rule 2 が使う現役定数。
6. **#642 バッチ強制は discrete 限定**: 強制 rung で UMA にも spill 予
   測が付くようになったが、num_batch=2048 の根拠測定は discrete カード
   のもの。

## スイープ(bundled カタログ 13 モデル × 21 ホスト、273 組)

- **capacity(拒否ゲート): 移動 0 件** — ゲートは不変。
- **recommendation / declaration: 移動 1 件** — qwen3.5-2b @ CPU 7 GB
  が false→true。旧 4 GB ヘッドルームで 178,176 止まりだった予算が
  控除統一(2 GB + エンジン留保 ~1.5 GB)で rung に到達。裁定が意図し
  た緩和方向の唯一の移動。
- **serve 窓: 移動 118 件** — 全件が「切り詰め窓・32k フロア・未チュー
  ニング(0)」→「rung 固定」への正規化。到達済(旧 fits 相当)の行の
  窓幅が変わった例は 0 件。全表:

| model/variant | host | serve(旧) | serve(新) |
|---|---|---|---|
| gpt-oss-120b/mxfp4-gguf | 16gb+2gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | 28gb+24gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | 32gb+8gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | 64gb+16gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | 64gb+8gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | 8gb+2gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | cpu-16gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | cpu-32gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | cpu-4gb | 0 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | cpu-64gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | cpu-7gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | cpu-8gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | mac-16gb(macmini) | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | mac-24gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | mac-32gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | mac-64gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | mac-8gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-120b/mxfp4-gguf | sv-xps15(32+8) | 32768 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | 16gb+2gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | 8gb+2gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | cpu-16gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | cpu-4gb | 0 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | cpu-7gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | cpu-8gb | 32768 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | mac-16gb(macmini) | 32768 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | mac-24gb | 114688 (未達) | 131072 (強制) |
| gpt-oss-20b/mxfp4-gguf | mac-8gb | 32768 (未達) | 131072 (強制) |
| qwen3.5-0.8b/q8-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | 16gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | 28gb+24gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | 32gb+8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | 64gb+16gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | 64gb+8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | cpu-16gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | cpu-32gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | cpu-64gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | mac-16gb(macmini) | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | mac-24gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | mac-32gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | mac-64gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-122b-a10b/q4-gguf | sv-xps15(32+8) | 32768 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | 16gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | cpu-16gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | mac-16gb(macmini) | 32768 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | mac-24gb | 37888 (未達) | 200704 (強制) |
| qwen3.5-27b/q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-2b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.5-2b/q4-gguf | cpu-7gb | 178176 (未達) | 200704 (証明済) |
| qwen3.5-35b-a3b/q4-gguf | 16gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | 28gb+24gb | 148480 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | cpu-16gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | mac-16gb(macmini) | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | mac-24gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | mac-32gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-35b-a3b/q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-4b/q4-gguf | 8gb+2gb | 35840 (未達) | 200704 (強制) |
| qwen3.5-4b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.5-4b/q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-4b/q4-gguf | cpu-8gb | 35840 (未達) | 200704 (強制) |
| qwen3.5-4b/q4-gguf | mac-8gb | 119808 (未達) | 200704 (強制) |
| qwen3.5-9b/q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-9b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.5-9b/q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-9b/q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.5-9b/q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | 16gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | cpu-16gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | mac-16gb(macmini) | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | mac-24gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | mac-32gb | 171008 (未達) | 200704 (強制) |
| qwen3.6-27b/mtp-q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | 16gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | cpu-16gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | mac-16gb(macmini) | 32768 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | mac-24gb | 59392 (未達) | 200704 (強制) |
| qwen3.6-27b/q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | 16gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | cpu-16gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | mac-16gb(macmini) | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | mac-24gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | mac-32gb | 99328 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/mtp-q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | 16gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | 28gb+24gb | 158720 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | 8gb+2gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | cpu-16gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | cpu-4gb | 0 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | cpu-7gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | cpu-8gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | mac-16gb(macmini) | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | mac-24gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | mac-32gb | 32768 (未達) | 200704 (強制) |
| qwen3.6-35b-a3b/q4-gguf | mac-8gb | 32768 (未達) | 200704 (強制) |

(旧列の「未達」= 連続サイジングが有効フロア未満の窓を選んでいた、
新列の「強制」= Fits=false で最下位 rung 起動・警告付き・宣言なし)

## Consequences

- 容量ゲートを通るが rung を証明できないホスト(例: UMA 32 GB に 22 GB
  モデル)は、従来の 158k 等の切り詰め窓ではなく rung で起動し、carve-
  out の超過分を報告する。遅くなるのは spill であって仕様どおり
  (waired#1067 R5 のソフトリミット方針)。
- 従来 32k フロアに落ちていた「詰んだ」ホストは rung 分の KV を確保す
  るようになる。そこに居るのは容量確認済み(または R5 の明示確認済
  み)ピックのみ。
- verify latch により、最下位 rung で計測 spill が許容を超えても再起動
  で窓が縮むことはなくなった。代わりに宣言が落ちる。
- CP 側は proto tag 追随後に同じ判定になる(それまで CP=旧サイジング /
  agent=新の一時ズレ。20260809/0016 と同種で、方向は CP が緩い側)。

## Refs

- waired-ai/waired-agent#587(追跡 issue)
- waired-ai/waired#1067(オーナー裁定 R4/R5)/ waired#1031(窓契約)
- docs/decisions/20260809/0016-measure-the-os-deduction-at-install.md(実測控除)
- docs/decisions/20260808/1907-price-capacity-at-the-served-window.md(#552、rung での容量値付け)
