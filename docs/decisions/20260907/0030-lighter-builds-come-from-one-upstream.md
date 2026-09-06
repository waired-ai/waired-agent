---
status: accepted
---

# 軽い量子化は足す。出所は `hf.co/unsloth` の 1 つに揃える (20260907 00:30)

## Status

Accepted。オーナー裁定 2026-09-06(waired-agent#1265)。

## Context

カタログの ollama タグ 15 本はすべて Q4_K_M 以上、すべて公式 `library`
namespace のものだった。16 GB のディスクリート GPU と 24 GB の Mac では
`proto/hostfit` が 27B と 35B の全項目を拒否し、提示できる最良は `qwen3.5-9b`
の quality_tier 52 — 27B 帯の下限 67 に届かない。

レジストリを直接読んだ結果(`docs/knowledges/20260907/0030-lighter-quants-only-exist-off-library.md`):

- 公式 `library` は Q4_K_M より軽い GGUF を 1 本も公開していない。軽い量子化は
  `library` を出ないと存在しない。
- 出荷中の ollama 向けモデル全部に GGUF リポジトリを持つ公開者は
  `hf.co/unsloth` だけで、リポジトリはすべて apache-2.0。unsloth の UD 動的
  量子化は `frob/` が再梱包しているものの上流である。
- Hub の ollama 互換レジストリは分割 GGUF を 400 で拒むので、候補は
  50 GB 未満の量子化に限られる。

オーナーにこの 2 点を示したうえで、「軽い量子化を足すか」「出所を 1 つに
揃えるか」を問うた。

## Decision

1. **軽い量子化は足す。** waired はハードウェアが制約になっている人のための
   製品であり、その人に 9B の tier 52 しか出せないのはカタログの欠落である。
2. **出所は `hf.co/unsloth` に揃える。** モデルごとに「その量子化を持っている
   namespace」から拾うのではなく、1 つの公開者を指す。

最初の 5 本:

| モデル | 量子化 | 計測ホストクラス |
|---|---|---|
| qwen3.8-27b | UD-Q3_K_XL、UD-Q2_K_XL | nvidia-24gb-discrete |
| qwen3.6-35b-a3b | UD-Q3_K_XL、UD-Q2_K_XL | nvidia-24gb-discrete |
| qwen3.5-122b-a10b | UD-Q2_K_XL | amd-unified-128gb |

このうち 122B は**別の PR で着地する**。41.85 GB の重みは 24 GB のカードでは
spill するので `amd-unified-128gb` クラスでしか採点できず(spill するホストは
モデルではなく自分自身を採点する)、そのホストが空くのを待つより先に採れた
4 本を出すほうが早いからである。併せてそのとき `qwen3.5-122b-a10b` の
`unmeasurable` を外す — 記録されている理由は「81 GB の重み / 128 GB 最低 RAM。
どのランナも越える」だが、#1192 で `amd-unified-128gb` クラスが入った時点で
その記述は事実でなくなっており、64 GB のマシンに自動推奨する variant を
無測定で出すことはできない。

### 退けた案

- **`library` に留まる。** そのときオーナーの問いへの答えは「軽い variant は
  存在しない」になる(上記ノート §1)。
- **モデルごとに、その量子化を持つ namespace から拾う。** オーナーが求めた
  統一に反するうえ、候補は個人の再アップロードである。
- **自前の namespace に自分で量子化して公開する。** `ollama create --quantize`
  が作れるのは静的な K-quant だけで UD 動的量子化は再現できない。加えて
  成果物の保守・配布・鍵の管理が増え、このプロジェクトの他のどこもそれを
  必要としていない。
- **既存の `qwen3.8-flash-next` を `frob/` から unsloth に移して揃える。**
  その UD-Q2_K_XL は Hub 上で 3 ファイルに分割されており、上記ノート §3 の
  400 で閉じる。`frob/` がこれを引ける唯一の経路のまま残る。

## Consequences

- **新しい variant 1 本につき GPU ホストでの `make e2e-agentgrade` 1 回。**
  `unmeasurable` はモデル単位のキーなので、variant 単位の免除は無い。
- **Hub のタグは variant に renderer を書いてある場合にだけ出荷できる。**
  `internal/catalog/sources_integration_test.go` のガードは、template レイヤに
  頼る Hub のタグを拒否する。Hub の config blob には `renderer` が無く、
  template レイヤは旧式 3 フィールドのテンプレートで、コーディングエージェントの
  会話を render できないため(上記ノート §4)。
- **quality_tier は手書きにする。** 合成式
  `10*log10(params) - 5*log10(footprint)` は小さい footprint に報いるので、
  同じモデルの軽い variant が自分の重い variant を上回り、両方が載るホストでは
  `RankModels` が軽い方を選ぶ — 軽い variant は重い方を載せられないホストの
  ためにあるのだから、これは目的の逆である。`tier_override` ではこの補正を
  表せない(`benchmarks.CheckEvidence` は受理済みの source を要求し、受理する
  どちらのランナーも 1 モデル内の量子化同士を比較しない)。そこで tier は
  手で書き、`internal/catalog/manifest_test.go` の
  `TestBundledManifests_QualityTierFollowsPrecisionWithinAModel` で「精度を
  多く残す variant が同じモデルの中で上に来る」ことを守る。
- **ライセンスは動かない。** unsloth のリポジトリは apache-2.0 で、
  Qwen Community License を受け入れた際の裁定(内部記録)は、ライセンスは
  タグでなくモデルに付くと記している。受理済みモデルの再量子化に新しい
  裁定は要らない。
- **製品の中でのモデルの同一性は model_id であり、variant ではない。**
  `internal/router/lighter_picker.go` は同じモデルの別 variant を「軽い候補」
  として意図的に返さない。切り替え経路・常駐チェック・「古い重みを消す」提案が
  すべて model_id で結ばれているためである。軽い variant は、重い方を載せられ
  ないホストに与えられるものであって、同じモデルの中で人に「一段下げる」と
  提示されるものではない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1265
- docs/knowledges/20260907/0030-lighter-quants-only-exist-off-library.md
- docs/decisions/20260906/1900-stamp-the-renderer-on-the-pulled-tag.md
- docs/decisions/20260828/1930-arm-the-request-shape-gate.md
- docs/knowledges/20260906/0400-an-ollama-tag-needs-a-renderer.md
