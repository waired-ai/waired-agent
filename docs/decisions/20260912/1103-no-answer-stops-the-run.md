---
status: accepted
---

# 答えの来なかった質問は run を止める (20260912 11:03)

## Status

Accepted（オーナー裁定 2026-09-12、waired-ai/waired-agent#1300）。

## Context

`waired init` の質問は、`ynAsk` が stdin から 1 行読もうとして EOF に
当たると `ynNoAnswer` を返す。空行は答えである（人が Enter を押した。
既定を印字するのはそのためだ）が、閉じた stdin は答えではない。

この `ynNoAnswer` をどう扱うかは、3 本の CLOSED issue が順に決めてきた。

- **#1048** *init: an engine install can start from a default answer
  nobody typed (off a TTY)* — 適合ホストの既定は Yes で、それを取ると
  誰も頼んでいないエンジン導入と数 GB のダウンロードが始まる。
- **#1070** *init: a machine-wide Claude Code route can be written from a
  default answer nobody typed (off a TTY)* — 既定 Yes が**マシン全体の**
  managed-settings 書き込みになる。
- **#1071** *init: a TTY-less re-run turns local inference off where
  --non-interactive would leave it on* — ここ**だけ**は `--non-interactive`
  と一致させた。既定を取るほうが破壊的だったからである。

つまり確立していた規則は「no-answer は `--non-interactive` に合わせる」
ではなく「**no-answer は誰も頼んでいない副作用を起こさない側に倒す**」で、
#1071 はたまたま両者が一致した例だった。

その原則は正しい。正しくなかったのは、それを**黙って**やり、
exit 0 で終えていたことである。rc6 の実機検証（Windows / RTX 4070
Laptop 8 GB、ssh に `-t` を付けない実行）が撮った端末:

```
  Run models on this computer? [Y/n] (default: Yes)
No answer on stdin. Nobody is here to say whether this computer should run models.
Skipping local inference. This computer still routes requests to your other computers.
✅ Waired is ready - local inference is switched off on this computer
```

画面に `(default: Yes)` と出したうえで No を適用し、「ready」と言い、
exit 0 する。「誰も答えなかった」が「誰かが No と答えた」として記録され、
サーバーに入れたはずのローカル推論が静かにオフのまま残る。導入した
自動化からは正常完了と区別できない。

## Decision

**原則は保ち、沈黙をやめる。** 答えが来ず、フラグでも答えられていない
質問に当たったら、`waired init` は

1. その質問に**何も書き込まず**、
2. 何が答えられなかったかと、端末なしで答えるフラグを名指しし、
3. 答えられなかった質問を並べた箱で終わり、
4. **exit 4** を返す。

`exitNoAnswer = 4` は `exitLocalAIDown = 3` とは別の事実である。3 は
**マシン**について言っている（エンジンが入らない / 起動し続けない）ので
修理で直る。4 は**その run** について言っている — 何も決まらなかった —
ので、フラグを足して再実行すれば直る。

### 止まる質問は 3 つだけ

答えが無いことが「誰も**決めなかった**」を意味する質問に限る。
no-answer の着地点が `--non-interactive` の着地点と同じ質問では、
止まる理由が無く、止めれば「仕様どおりに動いたインストール」を
失敗させることになる。

| 止まる | 止まらない（= `--non-interactive` と同じ所に着地する） |
|---|---|
| `Run models on this computer?` | モデルピッカー（Waired が選んだモデルを保つ） |
| `Set up coding-agent integration?` | ベンチ後の step-down / upgrade の申し出（動いているモデルを保つ） |
| `Route Claude Code inference through Waired now?` | `Keep local inference on anyway?`（#1071 が意図的に揃えた） |

### TTY 判定にはしない

引き続き isatty では判断しない。スクリプト導入は答えをパイプで流し込む
（`scripts/dev/lib/installtest-enroll.sh` がモデルピッカーをそう駆動して
いる）ので、端末でないことは答えが来ないことではない。判断材料は
「**この質問が答えを得られなかった**」という狭い事実のままにする。

## Consequences

- インストーラはこの腕に到達しない。`install.sh` は端末の無いホストでは
  init を走らせないか `--non-interactive` を渡し（`linux_maybe_init` /
  `darwin_maybe_init`）、`install.ps1` は stdin のリダイレクトを検出して
  `--non-interactive` を強制する（`install.ps1:2895`）。どちらのフラグも
  stdin を読む前に答えを確定させる。3 つの installtest ハーネスの
  `waired init` 呼び出しも同様で、唯一フラグを渡さない #590 の
  engine-only probe は 3 つの質問のいずれにも到達しない
  (`--inference-enabled=true` がエンジンの質問を、`--skip-integration` が
  連携の質問を答え、連携同意が無いので `planClaudeRoute` は
  `claudeRouteNone` を返す)。
- #1048 が書いた「ここで local AI をオフにして exit 0 のまま」という
  後始末は無くなる。`turnLocalAIOff` は**人が No と打った**ときだけ走る。
- 実装は `cmd/waired/init_unanswered.go`。どの質問が該当し、なぜ他が
  該当しないかはそこに書いてある。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1300
- https://github.com/waired-ai/waired-agent/issues/1048
- https://github.com/waired-ai/waired-agent/issues/1070
- https://github.com/waired-ai/waired-agent/issues/1071
- https://github.com/waired-ai/waired/issues/1361
