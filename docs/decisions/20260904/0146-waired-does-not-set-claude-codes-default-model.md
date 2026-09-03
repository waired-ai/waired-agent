---
status: accepted
supersedes:
  - docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md
---

# waired は Claude Code の既定モデルを設定しない (20260904 01:46)

## Status

Accepted。オーナー裁定（2026-09-04、waired-ai/waired-agent#1184 の実装中）。
`docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md` は
**§4「既定モデルはユーザー設定に記録する」だけ**が超えられる。同記録の §1〜§3・§5〜§7
（名指したモデルが実行先、裸形での照合、経路の判定と id 所有の分離、`/model` は永続設定を
書かない、退役した cloud 行、拒否された replacement の破棄）はすべて有効で、本記録は
そのうち §1 をむしろ徹底する。

## Context

`20260828/0252` §4 はこう決めた: id が実行先を決める以上、セッションが始まる id が既定の
実行先を決める。Claude Code 自身の既定は実 Anthropic のモデルなので、何もしなければ
触っていないセッションは全部 Anthropic へ行き、手元のハードは遊ぶ。だから
`waired claude enable` が `~/.claude/settings.json` の `model` に `claude-waired-auto` を
書く（waired-agent#1037）。

その裁定が安全だったのは、**Waired のターンがまだ実 Anthropic API へ運ばれ得た**からである。
`docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md` がその経路を
撤去し、Waired の id を持つターンは Waired のノードで答えるか理由付きで失敗するようになった。
その世界で既定に Waired の id を書くと、**エンジンも共有ピアも無いコンピュータでは、
連携を有効にした瞬間からそのユーザーの全ターンが失敗する**。0.0.3-rc5 の実機検証には
まさにその状態のホストがあった（`waired-ai/waired#1309`、エンジン未導入のまま連携だけ有効）。

## Decision

**waired はユーザーの既定モデルを変更しない。** 条件付きではなく、無条件に。

- `waired claude enable` と `waired init` の連携ステップは `~/.claude/settings.json` の
  `model` に何も書かない。`EnsureModelSetting` と `waired claude _model-default write` は
  撤去する。
- **既に書かれている値には触らない。** 以前の版が書いた `claude-waired-auto` も、操作者自身が
  選んだ値も、そのまま残る（waired-agent#1185 が legacy の綴りを新しい行へ移す）。
- `waired claude disable` は今日どおり **waired 自身の id だけ** を消す
  （`RemoveModelSetting`）。それは waired が書いた値だからである。
- `waired claude status` の `default model:` 行は残す。何が既定かを**報告**し、Waired の
  行が `/model` にあることを案内する。書き換えないことと、黙っていることは別である。

`20260828/0252` §4 の他の判断は維持する: managed settings には書かない（毎起動で引き戻し、
操作者の選択を毎セッション取り消すため）、そして操作者が選んだ値は触らない。

## Consequences

- 導入直後の何も触っていないセッションは Claude Code 自身の既定（実 Anthropic のモデル）で
  始まり、Anthropic へ行く。ユーザーが `/model` で Waired の行を選んで初めてこちらに来る。
  §4 が避けようとした「手元のハードが遊ぶ」は、**一度も選ばせないより一度選ばせる**方で
  引き受ける。init と `waired claude enable` の完了文、`waired claude status`、docs の該当節が
  その一手を案内する。
- エンジンの無いホストで連携を有効にしても、既存のセッションは壊れない。ユーザーが
  意図して Waired の行を選んだときだけ、fail-closed のエラーが出る — それは選択の結果であって、
  こちらが書いた既定の結果ではない。
- 「waired がユーザーのファイルに書く」面が 1 つ減る。残るのは statusLine と、
  waired 自身が書いた id の削除だけ。

## Refs

- waired-ai/waired-agent#1184（実装）/ waired-ai/waired#1313（レーン L97/L98）
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`（前提を変えた裁定）
- `docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md`（§4 のみ超える）
- waired-ai/waired#1309（rc5 実機検証。エンジン未導入のまま連携だけ有効なホスト）
