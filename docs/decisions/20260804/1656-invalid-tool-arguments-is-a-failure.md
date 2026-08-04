---
status: accepted
---

# エージェントが実行できないツール呼び出しは失敗として数える (20260804 16:56)

## Status
Accepted

## Context

`warn_invalid_tool_arguments`（#455）は、提示されたスキーマが受け付けない引数を
持つツール呼び出し——必須プロパティ欠落、型違い、`additionalProperties: false`
なのに未宣言のプロパティ——を検出する。導入時に**警告として、暫定的に**置かれ、
昇格は #322 に預けられた。理由は 2 つあった。

1. **保存済みの判定が全部このチェック以前のもの**だった。昇格すれば、そのファイルを
   見たこともない規則で再採点することになる。
2. **導入時の実測がゼロ**だった（qwen3.5:9b の 144 ターン）。何も観測されていない
   ものを失敗に格上げする根拠が無い。

#467 のスイープはチェックが入った状態で回ったが、それでも判断できなかった。
ストアが**クラス別に数えていなかった**からである。`failed` は失敗クラスをプールした
数、`verdict` は最悪クラス 1 つ。警告はそのどちらにも入らないので、
「何回起きたか」が記録されていない。昇格は試行を pass の列から failed の列へ移す
操作なので、何回移るかが分からないまま実施はできない。

#479 がクラス別カウントを入れ、その状態でカタログ全体を測り直した。これで両方の
条件が消えた。

## Decision

**`IsFailure()` を true にし、判定値を `warn_invalid_tool_arguments` から
`fail_invalid_tool_arguments` にリネームする。**

根拠は #479 の実測：

* このクラスは **51 の (バリアント, ケース) 組のうち 13 件**で発生している。
  導入時の「ゼロ」とは違い、盲点は実在した。
* 昇格しても **退役ワークリストは 1 行も動かない**。昇格後の最大は
  granite4-350m の 38%、ライン 50% に届かない。提供しているモデルの最大は
  qwen2.5-coder-3b の 23%。`--require-pass` は緑のまま。
* 変わるのは **Grade（pass/fail ラベル）4 バリアント**と、ユーザーが
  `waired models check-agent` で自分のモデルを測ったときの合否。後者が実質。
  エージェントが実行できない呼び出しを返すモデルに「pass」と答えていたのは、
  このパッケージが問うている質問に対して誤った答えだった。

**リネームを伴う理由**：判定値の文字列は保存される。`warn_` のまま失敗として
数えると `{"verdict": "warn_...", "failed": 13}` という、読んだ人が意味を取れない
記録が残る。

**`Severity` は動かさない。** `IsFailure` と直交する。引数が壊れた呼び出しは、
少なくともモデルがどのツールに手を伸ばしたかは伝えている。呼び出さないより軽い、
という順序は昇格後も正しい。**失敗ではあるが、最も軽い失敗**。

## Consequences

### 方針変更が GPU を要求しなくなった

`catalog-tool agentgrade --recompute` を追加した。保存された
`cases[].verdicts` を**現在の規則で読み直し**、`failed` / ケースの `verdict` /
バリアントの Grade を再導出する。今回の昇格はこれで適用した——**測定は 1 回も
していない**。

`CaseOutcome` の doc は以前から「grading policy は測り直さずに変えられる」と
主張していたが、`Failed` がスカラで保存されている限り偽だった。#479 の集計と
このコマンドで初めて真になった。

再計算しないのは `trials` と集計そのもの（＝測定値）だけ。**集計を持たない記録は
そのまま残す**。カウンタ以前の記録には読み直す材料が無く、ゼロにすると黙って
「綺麗」に再採点してしまう。何件飛ばしたかは出力する。

### 旧名のマッピングが要る

`CanonicalVerdict` を追加した。保存された文字列をリネームすると、古いファイルには
旧名が残る。そして **未知のクラスはこのパッケージで最も無害な扱いになる** ——
`IsFailure` は false、`Severity` は pass の順位を返す。マッピングが無ければ、
リネームされた失敗はエラーも出さずに数えられなくなる。A/B で確認済み：
外すと `warn_invalid_tool_arguments×4` が `pass×20` の**後ろ**に印字される。

### 陳腐化ガードが 2 PR 連続で効いた

#475 の `TestRatesCitedByRetireFailureRate` が、#479（測定による変化）に続いて
今回（**測定を伴わない方針変更による変化**）でも発火した。引用値は
granite4-350m 17%→38%、qwen2.5-coder-3b 14%→23%、qwen3.5-35b-a3b 5%→8%。

### 余白は狭まった。ラインは動かしていない

granite4-350m の 38% が、これまでで最もラインに近い。ただしこれは
**ユーザーに提供していない CI フィクスチャ**（352M、小さくてすぐ止まることを
理由に選ばれたモデル）で、提供している中の最大は 23%。

なおこの 38% は**レポートのどこにも印字されない** —— offered ではないので
レート表に出ず、50% 未満なので `WITHHELD, PENDING RETIREMENT` 節にも出ない。
見えない数字が育っても誰も気づかない、という別の穴として #484 に切った。

## Refs
- https://github.com/waired-ai/waired-agent/issues/483
- https://github.com/waired-ai/waired-agent/issues/455
- https://github.com/waired-ai/waired-agent/issues/322
- https://github.com/waired-ai/waired-agent/issues/479
- docs/decisions/20260803/2026-agentgrade-counts-classes-not-just-worst.md
- docs/decisions/20260803/1909-withhold-a-model-that-cannot-call-a-tool.md
