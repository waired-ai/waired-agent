---
status: accepted
---

# 2025 世代を退役させる (20260808 23:20)

## Status
Accepted

## Context

#518 が `quality_tier` を「載せると決めた世代のパラメータ順を出典付き override で
補正したもの」と再定義し、カタログを **qwen3.5 / qwen3.6 の 1 世代に固定**した。
その帰結として、前の世代のエントリが残っている理由が無くなった。

Hugging Face の作成日でカタログはきれいに割れる:

```
2025-07-20  GLM-4.5-Air                    ← カタログ最古
2025-07-22  Qwen3-Coder-480B-A35B
2025-07-31  Qwen3-Coder-30B-A3B
2025-08-04  gpt-oss-20b / gpt-oss-120b
2025-09-09  Qwen3-Next-80B-A3B
------------------------------------------  5 か月の空白
2026-02-24  Qwen3.5 27B / 35B-A3B / 122B-A10B
2026-02-27  Qwen3.5 9B
2026-02-28  Qwen3.5 0.8B
2026-04-15  Qwen3.6 35B-A3B
2026-04-21  Qwen3.6 27B
2026-04-22  DeepSeek-V4-Flash
2026-06-16  GLM-5.2
```

**Qwen3-Coder は Qwen3.5 より 7 か月、Qwen3.6 より 9 か月古い。** 諦めることになる
専門ファミリーではなく、#518 が固定した世代の 1 つ前 — qwen2.5-coder と同じ位置。

退役の根拠は**負けた測定ではない**。測定が存在する範囲では一致するが、
それは根拠そのものではなく、根拠の中の証拠にすぎない。

## Decision

**7 エントリを退役させる**（オーナー判断 2026-08-05、スコープ拡大 2026-08-05）。

| 退役 | 後継 | tier | 備考 |
|---|---|---|---|
| qwen2.5-coder-3b-instruct | qwen3.5-2b | 30 → 27 | agentgrade 12/72 → 4/72 |
| qwen2.5-coder-7b-instruct | qwen3.5-4b | 45 → 42 | 32k ネイティブで #624 床未満、既に自動選定不可 |
| qwen2.5-coder-14b-instruct | qwen3.5-9b | 55 → 52 | 同上（vLLM 経路のみ生存） |
| qwen3-coder-30b-a3b-instruct | qwen3.6-27b | 65 → 72 | |
| qwen3-coder-next-80b-a3b-instruct | qwen3.6-35b-a3b | 86 → 90 | unmeasurable → **measured** |
| qwen3-coder-480b-a35b-instruct | glm-5.2 | 92 → 97 | どちらも unmeasurable |
| glm-4.5-air-106b-a12b | glm-5.2 | 75 → 97 | 131k ネイティブで自動選定不可 |

**ライセンスの穴は空かない。** manifest は `license` フィールドを持ち、glm-4.5-air /
glm-5.2 / deepseek-v4-flash はいずれも MIT。glm-4.5-air は「ベンダー多様性」で
入っており、#521 の決定記録が「`moe-mit` は `license` として既に構造化データで
持つ事実の名前への二重符号化」と却下している。

**`waired/*` エイリアスの移し替えは発生しない。** #521 が静的 `waired/*` 名前空間を
全廃したため、offered な manifest はそもそも宣言できない。`retired.go` の
「alias は successor に移す」という段落は適用対象を失っており、書き換えた。

## Consequences

**ollama 側の実害はほぼゼロ。** qwen2.5-coder 3 兄弟は 32k ネイティブで
`MeetsNativeContextFloor` が既に硬く弾いており、qwen3-coder-30b / next-80b は
tier 順で現行世代に既に負けていた。実際に動くのは 320 GB 超のホストが
480b (92) → qwen3.6-35b-a3b (90) に 1 段下がる点だけ。

**vLLM 側は違う。24 GB 未満のカバレッジが全部この 7 つにあった。**

```
qwen2.5-coder-3b    awq-int4     4,096 MB  ← 退役
qwen2.5-coder-7b    awq-int4     8,000 MB  ← 退役
qwen2.5-coder-14b   awq-int4    16,000 MB  ← 退役
qwen3-coder-30b     awq-int4    24,000 MB  ← 退役
qwen3.6-27b         awq-int4    24,000 MB  ← 現行世代で唯一
```

退役後 vLLM の入口は 24 GB。#572 が先に着地しているため、8〜23 GB の NVIDIA Linux
ホストは ollama にフォールバックして qwen3.5-9b を得る — **ローカル推論を失わない**。
これはカバレッジではなくフォールバックで、#575 が qwen3.5 系列の vLLM variant 追加を
追跡する。

**サイズクラスは 1 つも動かない。** `shippedSizes` は 21 → 14 行になるが、
生き残ったモデルのクラスは全て不変。8 GB 線への最接近（qwen3.5-9b、7.43%）と
32 GB 線への最接近（qwen3.5-35b-a3b、24.09%）はどちらも退役対象ではないため。
32 GB 線の**上**の空きはむしろ広がった（48,721 → 62,632 MiB）。

**証拠ストアのガードを 2 つ緩めた。**

- `TestUnmeasurableEntriesCarryReasons` に退役免除を追加。unmeasurable 宣言は
  「なぜ測らなかったか」の記録であり、「どのランナーでも測れない」と書いた退役理由が
  その記録を必要とする
- 退役理由の要件から `"95% confidence"` と `"#200"` の literal を外した。あれは
  「測定された失敗率で、#200 で決めた」という**たった 1 件の退役の形**を符号化して
  いた。残したのは実際に効いていた部分 — **どこで決めたかを引用すること**。
  率を主張する理由は今も信頼区間を要求する

`internal/catalog/benchmarks.json` の next-80b 行は削除した。あれは tier の根拠であり、
tier ごと消えるため。免除機構は無く、あっても記録として意味を持たない。

**カタログレーダーが退役モデルを再提案する問題を塞いだ。** `discovery.Known` は
生きたカタログからしか作られておらず、退役 7 件がそのまま「新候補」として戻ってきた
（レーダー自身のテストが捕まえた）。`catalog.Retirements()` の `Names` を追加した。
退役表は退役名が生き残る唯一の場所なので、「見たうえで No と言った」を答えられる
唯一の材料でもある。

**`proto/hostfit.InstallQualityFloorTier` は `Deprecated:` で残す。**
`scripts/ci/protoguard` が `const removed` と `const value changed` の両方を落とし、
免除機構が無い。proto は公開契約で、コントロールプレーンは pin したタグに対して
コンパイルする。消費者の移行が先。

**`Manifest.Validate` に `context_length > 0` を追加した。** 並行セッションが #552 の
容量価格付けをレビュー中に発見: `OllamaCeilingWindow` は window の無い manifest に
0 を返し、`OllamaPlannedWindow` の cap は `ceiling > 0` でガードされているため、
window を失った manifest は**静かに cap されなくなり** #552 以前のサイジングに戻る。
今日到達可能なのは pin 経路だけだが、pin は外から来る唯一の入力。

## Refs
- waired-ai/waired-agent#522（本 issue）
- waired-ai/waired-agent#518（世代の梯子としての quality_tier）
- waired-ai/waired-agent#521 / `docs/decisions/20260805/1806-waired-aliases-are-dynamic-or-internal.md`
- waired-ai/waired-agent#572（カタログが食わせられないエンジンを選ばない）
- waired-ai/waired-agent#575（qwen3.5 系列の vLLM variant）
- waired-ai/waired#1031（ネイティブウィンドウ半分は stand-down 禁止）
- `docs/decisions/20260808/2145-the-install-floor-is-capacity-not-quality.md`
- `docs/decisions/20260804/1943-retire-a-catalog-entry-with-a-successor-map.md`(退役機構)
