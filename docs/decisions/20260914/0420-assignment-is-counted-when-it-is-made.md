---
status: accepted
---

# リクエストは割り当てた時点で数え、順位付けと割り当てを直列にする (20260914 04:20)

## Status

Accepted。オーナー決定(2026-09-14、waired-ai/waired-agent#1354 の修正方針の相談で)。

## Context

rc6 の実機検証(waired-ai/waired#1361 の記録)で、Claude Code がサブエージェントを並列に起動すると、要求元の 1 スロットのエンジンに積まれることが分かった。

- 実測: 6 本が 6 ms 以内に届き、ピア A に 1 本、ピア B に 1 本、要求元に 4 本入った。
  - 要求元の 4 本は 1 本ずつ順番に処理され、最大 371 秒かかった。
  - 順位がすぐ下のピア C(2 スロット)には 1 本も入らなかった。
- 同じ 6 本を 50 ms ずつずらすと、1/1/2/2 に分かれた。
- 原因は、6 本すべての順位付けが、どれかが Commit する前に終わっていたこと。
  `assignSpeedRanks` の混雑の係数 `(capacity_used + 1)` には、同時に来た兄弟リクエストが 1 本も入っていなかった。
  - ピアの `capacity_used` は、前回のプローブで得た値。
  - 要求元の `capacity_used` は、スロットを持っているリクエストだけを数えていて、`AdmitLocal` で待っているものは入らない。

オーナーの確認: ランキングと推論先の決定はどこで行っているか。
答え: 要求元の waired-agent が行う。コントロールプレーンはネットワークマップを配るだけで、リクエストごとの判断には関わらない。
これを受けて、オーナーは「リクエストを直列に受け付け、並列数を加味して振り分ける」形を選んだ。
候補に挙がっていた「Commit 時に数が変わっていたら選び直す」形は、プローブのラウンドを余分に回すので採らなかった。

## Decision

1. **順位付けと、1 位への割り当ての計上を、短い排他区間で直列に行う**(`router.Selector.SelectKAssigned`、ロックは共有の `router.Assignments`)。
   - 排他区間で行うのは、スナップショットの読み取り、並べ替え、1 位の計上だけ。
   - 準備確認のプローブは区間の外で、各リクエストが並列に行う。
2. **計上先**
   - ピア: 既存の `LocalInFlight`。Commit ではなく、割り当てた時点から数える。
   - 要求元のエンジン: `Assignments` で数える。上限では断らない。
     ローカル脚は待ちで受け、時間の上限を置かない(`docs/decisions/20260912/2130-owner-priority-is-the-account-not-the-device.md` §3)ので、数は順位にだけ効く。
3. **混雑の係数に入れる `capacity_used` は、「観測値」と「自分が割り当て済みで未解放の数」の大きい方にする。**
   - 足し合わせない。プローブやスロットがすでにそのリクエストを見ている場合、二重に数えることになるから。
   - 式そのもの(`docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md` の決定 9)は変えない。
4. **1703(プローブは Selector の順位を尊重する)は維持する。**
   1 位のプローブが ready でなければ、既存の順送りで ready な最上位を Commit し、1 位に計上した数は返す(`Candidate.Abandon`)。
   割り当てた候補を捨てても、sticky の束縛と公開共有の grant 使用は残らない。どちらも Commit まで行わないから。

## Consequences

- 同時に届いたリクエストも、時間差で届いたときと同じ分かれ方になる。#1354 の 4 台構成を再現したテストでは、A 1・B 1・要求元 2・C 2。
- 同じ時刻に届いたリクエストの順位付けは、1 本ずつ待ち合わせる。区間内はスナップショットの読み取りと並べ替えだけなので、待ちはリクエストの応答時間に比べて無視できる。
- 1 位のプローブが返るまで、その数は計上されたまま。1 位が ready でなかった回だけ、並行するリクエストが実際より 1 本多い数で順位を付ける。
- `waired infer --explain` など、配送しない `SelectK` の呼び出しは何も数えない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1354
- `docs/decisions/20260805/1703-probe-honours-the-selector-ranking.md`
- `docs/decisions/20260912/2130-owner-priority-is-the-account-not-the-device.md`
- `docs/decisions/20260913/2245-speed-is-one-request-at-32768-tokens.md`
