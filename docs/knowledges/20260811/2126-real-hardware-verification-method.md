# 実機で前後比較するときの手順と落とし穴 (20260811 21:26)

## Issue

rc8 の実機検証（linux / windows / macOS の 3 台、`c0e2a1f` → `8d2c120` →
`be2d4b3`）で、**測り方の欠陥で無効になった観測が複数出た**。値そのものは
issue に残るが、手順は残らないので書いておく。次に同じことをやる人が同じ
3 回を無駄にしないためのもの。

## Learnings

### 1. `engine.log` のリクエスト本数は、作成時刻だけでは比較できない

`/api/generate` の本数を前後で数えるのは、screen が 2 本目を投げたか
（= 誤発火したか）を見る一番直接的な指標。ただし ollama はログを**その場で
truncate する**ことがあり、しかも**非決定的**。

実測（同一ホスト、2 回の更新）:

| 回 | 作成時刻 | len | 判定 |
|---|---|---|---|
| 1 回目 | 変化なし | 119769 → 135924（増加） | **有効**、delta 4 |
| 2 回目 | **変化なし** | 156945 → **143729（減少）** | **無効** |

作成時刻が同じでも中身が縮む。**`CreationTimeUtc` / inode に加えて、len の
単調性も控える**こと。片方だけでは 2 回目のようなケースを見逃す。

別のホストではローテートで inode ごと変わっていた。ホスト間で本数を比較する
のは常に無意味で、**同一ホストの前後でだけ、上の 2 条件が揃ったときに**有効。

### 2. `--no-init` / `-SkipInit` で更新すればローカル AI は切り替わらない

アップグレード自体は `waired init` を回さない。ローカル AI のトグルを動かし
得るのは init のステップ 6 だけなので、**init を回さなければ実機の状態を
壊さずに前後比較できる**。

daemon 側は `hostCutoffIsStillOurs` が `desired != ""` で判定を降りるため、
トグルが書かれているホスト（= 一度でも使われたホスト）では cutoff が勝手に
オフにすることはない。

検証で 3 台とも `desired_state: enabled` を保てたのはこの組み合わせによる。

### 3. 再計測は `agent_version` を汚して daemon を再起動すれば何度でも踏める

`ensureHostSpeedMeasured` は `state.HostSpeedRecord.AgentVersion` が
`buildinfo.Version` と違えば測り直す。state dir の記録を書き換えて再起動
すれば、アップグレードを待たずに再計測を再現できる。

VRAM / RSS を 6 秒間隔でトレースしながらこれを回すと、計測とモデル常駐の
時間関係が取れる。docs/knowledges には書けない詳細だが、順序が固定である
ことはこれで確定した。

### 4. Windows のエージェント INFO ログは Event Log ではなくファイルにある

**（2026-08-12 訂正。当初の記述は下の「訂正」を参照）**

Event Log は WARN 以上しか受け取らない。これは今も変わらない。**エージェント
自身の INFO / DEBUG は Event Log ではなくファイルに落ちる** — 場所を決めて
いるのは `logrotate.AgentOwnedLogFile` で、既定では
`C:\ProgramData\waired\logs\waired-agent.log`。プロセスが自分で開いて自分で
サイズを抑えるファイルなので、サービスマネージャは関与しない
（既定 32 MB × 10、`waired config log-level debug` の間は 128 MB × 10）。

読み方は 3 つあり、どれも同じファイルを指す。`waired logs` / `waired doctor`
／インストーラの完了行（`Diagnostics: waired doctor (logs: …)`）。実機では
32 MB のこのファイルに INFO レコードが入っていることを確認済みで、その中には
ファイル自身の存在を告げる `"agent log: writing to a rotating file"` も含まれる。

したがって **INFO 行の有無を確認する検証は Windows でも実行できる**。3 OS を
揃える検証を設計するとき、Windows だけ INFO を根拠から外す必要はない。

### 5. soft assert は「緑だから正しい」と「緑だが何も見ていない」を区別できない

`installtest` の host-speed assert を blocking に反転した瞬間、それまで
緑だった routing sentinel が落ちた。原因は製品ではなく assert 側で、
**非同期な計測をステータス 1 回読みで判定していた**。

連続する 2 つの run で両方の目が出ている:

| run | 計測の着地 | 1 回読みが見たもの |
|---|---|---|
| 31329512155 | `measured_at 18:30:58` | 18:30:59 に読んで通過（**1 秒差**） |
| 31331382917 | 未着地 | 何も無く失敗 |

**1 秒の余裕は通ったテストではなく、表が出たコイン。** soft のままだと
この race は見えない（#178 / #215 の系統）。

修正はポーリング化（最大 180 秒、5 秒間隔、値が出たら即抜け）。これで
assert の主張が「私が見た瞬間に持っていた」から「**daemon はこの窓の中で
判定に到達する**」に変わり、テスト自体が強くなる。同じファイルのモデル
ready チェックは既に同じ理由でポーリングしていた。

**教訓**: 非同期な処理に対する assert は、blocking にする前にポーリングに
しておく。soft のうちは race が緑に隠れる。

### 6. CI ランナーは推奨要件未満である

GitHub-hosted linux ランナーの実測: prefill **81.9 tok/s**、1 往復の下界
**256 秒**（45 秒 budget に対して 5.7 倍）。実機 3 台は 19700 / 6132 /
1269 tok/s なので、**CI と実機で 240 倍開いている**。

したがって、**ローカル推論がオンであることを暗黙に期待している harness の
leg は、これからランナーの速度で結果が決まる**。期待するなら
`--inference-enabled` で明示すること。#579 Stage 3c が init にその判定を
読ませるようになって初めて表面化した。

逆に、この 240 倍の開きは判定線（45 × 1.5 = 67.5 秒）の検証としては強い:
**実機 3 台は誤発火せず、CI ランナーは正しく発火する**、が両側から取れる。

## 訂正 (20260812)

**§4 は執筆時点（2026-08-11）では正しかったが、翌 08-12 に無効になった。**
当初はこう書いていた:

> ### 4. Windows ではエージェントの INFO ログがどこからも読めない
>
> Event Log は WARN 以上しか受け取らず、`%ProgramData%\waired` にエージェント
> のログファイルは無い。…**INFO 行の有無を確認する検証依頼は Windows では
> 物理的に実行できない**。…3 OS を揃える検証を設計するときは、Windows で
> INFO が読めない前提で組む。

このノートの Refs が既に挙げていた #636 —「Windows では INFO / DEBUG の
行き先が無い」— を #687 (`5b0ee49`) が塞ぎ、エージェントが自分で開いて自分で
回転させるログファイルを Windows に与えた。したがって「書き出し先が無い」も
「検証は物理的に実行できない」も、08-12 以降は成り立たない。§4 は現在の姿に
書き換えてある。

**この訂正が遅れたこと自体に代償があった。** 同じ falsehood が
`internal/platform/logrotate/logrotate_windows.go` のコメントにも残っており、
それを現行と読んだ結果、「Windows では 3 チェックポイントが取得不能」という
誤った検証設計が一度組まれた。実機から反証されて判明した (#745)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/579
- https://github.com/waired-ai/waired-agent/pull/643
- https://github.com/waired-ai/waired-agent/issues/636
- https://github.com/waired-ai/waired-agent/pull/687
- https://github.com/waired-ai/waired-agent/issues/639
- https://github.com/waired-ai/waired-agent/issues/745
- https://github.com/waired-ai/waired/issues/1139
- https://github.com/waired-ai/waired/issues/1140
