# フェイルオープンなガードには「回復可能性の線」が要る (20260830 01:05)

## Issue

#1138 — mesh がノードの広告を止める述語 `deps.EngineDead`
(`cmd/waired-agent/main.go`) が `Health().State == StateFailed` だけを見ていて、
give-up ラッチを見ていなかった。#1069 系統の最後の 1 本。

## Learnings

### 1. 必要だった分類は、既に別のファイルに書かれていた

`main.go` の但し書きは述語を「StateFailed ONLY」と宣言し、その理由を
**起動途中(`StateStarting` / `StateNotStarted`)を守るため**と説明していた。
これは正しい。だが「では諦めたエンジンはどちら側か」には答えていない。

その答えは 1 ファイル隣に既にあった。`cmd/waired-agent/engine_giveup.go`:

> [refusal と exhausted は] Neither is the give-up latch — after either of
> them EnsureRunning will still try again on the next trigger … the whole
> reason the latch must not be set from here.

つまりこのコードベースは既に **「まだ試している」/「もう試さない」** で状態を
分類していた。ガードはその分類を参照していなかっただけで、線を引き直す必要は
無かった。

**教訓**: フェイルオープンなガードを直すとき、「どの状態で発火すべきか」を
自分で設計し直す前に、**同じ区別が既にどこかで言語化されていないか**を探す。
`EngineDead` の場合、正しい表はこうなる — 深刻さではなく回復可能性が軸:

| 状態 | まだ試している | 広告を止めるか |
|---|---|---|
| `StateStarting` / `StateNotStarted` | はい | いいえ |
| bootstrap refusal (アダプタ無し) | **はい** | いいえ |
| start budget 使い切り | **はい** | いいえ |
| **give-up ラッチ** | **いいえ(明示 start が要る)** | **はい** |

### 2. 「黙る」欠陥と「嘘をつく」欠陥は同じ系統でも重さが違う

#1069 系統の他の 8 件はすべて **理由が表示されない**(黙る)だった。
#1138 だけは **間違った答えを返す** — ノードが容量を広告し続けるので、
ピアがそこにターンを投げる。同じ「ラッチを読み損ねる」でも、
`internal/inferencemesh` の routability と透過プロキシの degrade 判定を
駆動している面では結果が違う。

**系統をまとめて掃くときは、面が「読み手」か「決定者」かで優先度を分ける。**

### 3. ほとんど同じ既存ヘルパは、たいてい述語ではない

`servingFailureReason` (`cmd/waired-agent/inference.go`) は必要な述語の
ほぼすべてを持っていた — 同じ `servingAdapter()` に対して Health と
ラッチの両方を訊く。再利用したくなるが、**2 行で違った**:

- `StateFailed` かつ `LastErr == ""` かつ非ラッチ → `""` を返す。
  これを述語にすると、ガードが元々存在した理由(#29)の状態で false になる
- アダプタ不在 + refusal 記録 → 非空を返す。上の表では false であるべき

**「理由を返す関数」と「判定する述語」は、片方が空文字を返す条件が
もう片方の false と一致しない限り、別物**。

### 4. 実装していないフェイクは、アサーションの miss 側を通らない

ラッチはインタフェースアサーションで取る(`peer.Adapter` が意図的に
実装していないため)。アサーションは **fail-open** なので、
本番アダプタが実装を失っても全テストが緑のまま通る。

塞ぎ方は 2 つ:

- 本物の型に対するコンパイル時アサーションを置く
  (`var _ interface{ FailureLatched() bool } = (*infruntime.OllamaAdapter)(nil)`、
  vLLM は linux タグ付きファイル側に)
- miss 側の行には、**本当に実装していないフェイク**を使う。
  既存の `recordingAdapter` は `FailureLatched() bool { return false }` を
  持っているので hit 側しか通らない — これは前提アサートで気付けた

### 5. 実物と違う挙動のフェイクは、そのフェイクで固定したい欠陥を再現できない

`latchRecorder.Stop` は `return nil` だけだった。本物は
`Health` 構造体を丸ごと上書きしつつラッチを残す
(`internal/runtime/ollama.go`, `vllm.go`)。この上書きこそが #1138 の機構なので、
**フェイクを本物に合わせるまで、テストは赤くなりようがなかった**。

関連: [ローテートするログと、1 行の面](../20260829/1530-rotating-logs-and-one-line-surfaces.md)
