---
status: accepted
---

# OS 取り分は各マシンがインストール時に実測する (20260809 00:16)

## Status
Accepted

## Context

`OSMemoryAllowanceGB = 2` は 3 OS 共通の推測で、容量計算が system RAM から
差し引く唯一の控除だった(#568)。実機 3 台(Linux 121 GiB / macOS 16 GiB /
Windows 32 GiB)の実測で、(a) Windows はカーネル+プール系だけで 2 GiB を
超える、(b) 3 OS とも available 指標はキャッシュ/プリロードを空き側に数える
(total − available がキャッシュを OS に誤算入しない)、(c) total − available の
主成分は常駐アプリで、Windows では短時間に GiB 単位で揺れる、が確認された
(#568 のコメントに計測表)。

## Decision

1. **控除は `max(OSMemoryAllowanceGB, RAMTotalGB − RAMAvailableGB)`**
   (`hostfit.Host.OSMemoryDeductionGB()`、#568 のオーナー裁定 2026-08-08)。
   定数 2 は floor として存続(proto-guard の const 値変更禁止とも整合)。
   available=0 と実測不能値(負・total 超)は「測定なし → 定数」。
2. **計測はインストール毎に一回**、エンジン/モデル非常駐の時点で実施
   (デーモン起動時、engine bootstrap の前)。`runtime/host-memory.json` に
   AgentVersion キーで永続化(host-speed.json の waired#1099 方式)。
   エンジンポートに応答がある場合(オペレータ運用の外部エンジン)は
   再計測を延期し前回レコードを保持。probe 成功時は 1 GiB を下限に丸め
   一回で記録し、record・wire・両アダプタが同じ整数で計算する。
3. **wire は永続値のみ**(`signer.HardwareSummary.RAMAvailableGB`、
   `ram-available-v1` capability 背後)。live 値は診断用にプロファイル JSON に
   残るが、fit 判定・配信には決して使わない(常駐モデルを自ホストに
   課すことと 5 分毎 resample の map churn を同時に防ぐ)。
4. **CI/オペレータ向けの決定性シーム**: `WAIRED_RAM_AVAILABLE_GB`
   (probe/record より優先、永続化しない)。GH-hosted ランナーの実測揺れが
   容量アサーションを flake させる経路を塞ぐ。

## Consequences — bundled カタログ × ホスト種別スイープ

判定が動くのは実測控除が定数を超えるホストだけで、**全 52 行が
fits true → false(締める方向のみ)**。控除 4 GiB では 1 行も動かない
(軽負荷で計測されたホストは全判定を維持する)。2026-08-08 のソフト化裁定
(waired#1067)下では、false は拒否ではなく「自動選択・推奨から外れ、明示選択には
警告+確認(デフォルト No)」を意味する。

ホスト略記: cpu*N* = CPU-only *N* GiB / disc32+8gb = 32 GiB + 8 GiB dGPU /
disc64+24gb = 64 GiB + 24 GiB dGPU / uma*N* = unified *N* GiB。
控除列は実測 `total − available`(GiB)。need/have は MiB。

| host | variant | 控除 | 判定 | need / have |
|---|---|---|---|---|
| cpu8 | qwen3.5-0.8b q8-gguf | 6| true → false | 3194 / 2048 |
| cpu8 | qwen3.5-2b q4-gguf | 6| true → false | 4088 / 2048 |
| cpu16 | qwen3.5-4b q4-gguf | 10| true → false | 7539 / 6144 |
| cpu16 | qwen3.5-9b q4-gguf | 6| true → false | 10719 / 10240 |
| cpu16 | qwen3.5-9b q4-gguf | 8| true → false | 10719 / 8192 |
| cpu16 | qwen3.5-9b q4-gguf | 10| true → false | 10719 / 6144 |
| cpu32 | gpt-oss-20b mxfp4-gguf | 16| true → false | 19544 / 16384 |
| cpu32 | qwen3.5-27b q4-gguf | 10| true → false | 24189 / 22528 |
| cpu32 | qwen3.5-27b q4-gguf | 16| true → false | 24189 / 16384 |
| cpu32 | qwen3.5-35b-a3b q4-gguf | 6| true → false | 26833 / 26624 |
| cpu32 | qwen3.5-35b-a3b q4-gguf | 8| true → false | 26833 / 24576 |
| cpu32 | qwen3.5-35b-a3b q4-gguf | 10| true → false | 26833 / 22528 |
| cpu32 | qwen3.5-35b-a3b q4-gguf | 16| true → false | 26833 / 16384 |
| cpu32 | qwen3.6-27b mtp-q4-gguf | 8| true → false | 25183 / 24576 |
| cpu32 | qwen3.6-27b mtp-q4-gguf | 10| true → false | 25183 / 22528 |
| cpu32 | qwen3.6-27b mtp-q4-gguf | 16| true → false | 25183 / 16384 |
| cpu32 | qwen3.6-27b q4-gguf | 10| true → false | 23493 / 22528 |
| cpu32 | qwen3.6-27b q4-gguf | 16| true → false | 23493 / 16384 |
| cpu32 | qwen3.6-35b-a3b mtp-q4-gguf | 8| true → false | 25442 / 24576 |
| cpu32 | qwen3.6-35b-a3b mtp-q4-gguf | 10| true → false | 25442 / 22528 |
| cpu32 | qwen3.6-35b-a3b mtp-q4-gguf | 16| true → false | 25442 / 16384 |
| cpu32 | qwen3.6-35b-a3b q4-gguf | 6| true → false | 26733 / 26624 |
| cpu32 | qwen3.6-35b-a3b q4-gguf | 8| true → false | 26733 / 24576 |
| cpu32 | qwen3.6-35b-a3b q4-gguf | 10| true → false | 26733 / 22528 |
| cpu32 | qwen3.6-35b-a3b q4-gguf | 16| true → false | 26733 / 16384 |
| disc32+8gb | qwen3.5-35b-a3b q4-gguf | 16| true → false | 26833 / 24576 |
| disc32+8gb | qwen3.6-27b mtp-q4-gguf | 16| true → false | 25183 / 24576 |
| disc32+8gb | qwen3.6-35b-a3b mtp-q4-gguf | 16| true → false | 25442 / 24576 |
| disc32+8gb | qwen3.6-35b-a3b q4-gguf | 16| true → false | 26733 / 24576 |
| disc64+24gb | qwen3.5-122b-a10b q4-gguf | 6| true → false | 83864 / 83859 |
| disc64+24gb | qwen3.5-122b-a10b q4-gguf | 8| true → false | 83864 / 81811 |
| disc64+24gb | qwen3.5-122b-a10b q4-gguf | 10| true → false | 83864 / 79763 |
| disc64+24gb | qwen3.5-122b-a10b q4-gguf | 16| true → false | 83864 / 73619 |
| uma16 | qwen3.5-4b q4-gguf | 10| true → false | 7403 / 6144 |
| uma16 | qwen3.5-9b q4-gguf | 6| true → false | 10455 / 10240 |
| uma16 | qwen3.5-9b q4-gguf | 8| true → false | 10455 / 8192 |
| uma16 | qwen3.5-9b q4-gguf | 10| true → false | 10455 / 6144 |
| uma32 | gpt-oss-20b mxfp4-gguf | 16| true → false | 18984 / 16384 |
| uma32 | qwen3.5-27b q4-gguf | 10| true → false | 23509 / 22528 |
| uma32 | qwen3.5-27b q4-gguf | 16| true → false | 23509 / 16384 |
| uma32 | qwen3.5-35b-a3b q4-gguf | 8| true → false | 25873 / 24576 |
| uma32 | qwen3.5-35b-a3b q4-gguf | 10| true → false | 25873 / 22528 |
| uma32 | qwen3.5-35b-a3b q4-gguf | 16| true → false | 25873 / 16384 |
| uma32 | qwen3.6-27b mtp-q4-gguf | 10| true → false | 24463 / 22528 |
| uma32 | qwen3.6-27b mtp-q4-gguf | 16| true → false | 24463 / 16384 |
| uma32 | qwen3.6-27b q4-gguf | 10| true → false | 22841 / 22528 |
| uma32 | qwen3.6-27b q4-gguf | 16| true → false | 22841 / 16384 |
| uma32 | qwen3.6-35b-a3b mtp-q4-gguf | 10| true → false | 24538 / 22528 |
| uma32 | qwen3.6-35b-a3b mtp-q4-gguf | 16| true → false | 24538 / 16384 |
| uma32 | qwen3.6-35b-a3b q4-gguf | 8| true → false | 25777 / 24576 |
| uma32 | qwen3.6-35b-a3b q4-gguf | 10| true → false | 25777 / 22528 |
| uma32 | qwen3.6-35b-a3b q4-gguf | 16| true → false | 25777 / 16384 |

代表行は `proto/hostfit/os_deduction_test.go` の表テストとして固定した
(1907 決定の検証標準: 使い捨てスイープ → 本表、恒久分 → table test)。

## この決定に含まれないもの

- **起動窓の rung 固定(切り詰め撤廃)** — #587。サイジング側控除の統一は
  そちらで `OSMemoryDeductionGB()` に載る。
- **計測の status/doctor 露出と再計測手段** — #589。
- **CP 側の proto tag 追従** — 追従までは CP=定数 / agent=実測のズレが残る
  (1907 決定の末尾注と同類、一時的)。

## Refs

- #568(field table・オーナー裁定・実機計測表)/ #591(wire)/ 本 PR
- waired#1067(ソフト化裁定)・waired#1099(インストール毎計測の先例)
- docs/decisions/20260808/1907-price-capacity-at-the-served-window.md(検証標準)
