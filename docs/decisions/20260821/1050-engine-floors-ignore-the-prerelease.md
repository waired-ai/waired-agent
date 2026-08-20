---
status: accepted
---

# エンジンの床は prerelease を見ない — `AtLeast` と `Compare` は別の問いに答える (20260821 10:50)

## Status

Accepted。オーナー裁定 2026-08-21（waired-ai/waired-agent#804）。
**挙動の変更はない**。今日の挙動を「記録」から「契約」に格上げし、
`internal/version` に順序が 2 本ある状態を、意図されたものとして確定させる。

## Context

`internal/version.AtLeast` は dotted-numeric の部分だけを比べるので、
`AtLeast("0.6.0-rc1", "0.6.0")` は true を返す。同じパッケージの
`Compare` は waired-ai/waired-agent#781 で prerelease-aware になっており、
同じ組に対して逆を答える。#781 はこの差を意図的に残し、
`AtLeast` 側の可否を #804 に送っていた。

`AtLeast` の非テスト呼び出し元は 1 か所だけである
（`internal/router/model_picker.go`、カタログの `min_engine_version`）。
つまりこの述語が答えているのは「このエンジンは床を越えているか」の
一問のみで、`Compare` が答えている「2 つのビルドのどちらが新しいか」
とは用途が分かれている。

## Decision

**エンジンの床は prerelease を無視する。** `AtLeast` は今のまま、両辺の
dotted-numeric だけを比べる。

理由は 2 つ。

* **床は release に対して書かれている。** カタログが `0.30.0` と書くとき、
  それは 0.30.0 リリースを指している。その prerelease を回しているホストは
  自分で opt-in した開発者であって、「あなたが見えている版が要ります」と
  言ってモデルを引き上げるのは、床が守ろうとしているものより悪い答えになる。
* **カタログが既にこの挙動の上に値を決めている。**
  `docs/decisions/20260816/2024-qwen3-8-takes-the-27b-band.md` は ollama の
  pin に 0.32.13 を採り、**0.32.14-rc0 を採らなかった理由として
  「`version.AtLeast` は prerelease-blind だから」を明記している**。
  つまり「床の側に prerelease を書かない」という運用は、この挙動を前提に
  既に成立している。ここを変えると、その決定が意味していたことが変わる。

**パッケージに順序が 2 本あるのは競合ではない。** 問いが 2 つあるからである。

| 述語 | 問い | prerelease の扱い |
|---|---|---|
| `Compare` | この 2 つのビルドはどちらが新しいか | release より下。これが無いと rc→rc の更新が全部「最新です」になる（#781） |
| `AtLeast` | このエンジンは床を越えているか | 無視する。release に対して書かれた数字に対するノイズ |

どちらももう一方の代用ではない。`dotted.go` の doc コメントと
`dotted_test.go` のピンが、この 2 行表を両側から述べる。

## Consequences

* `dotted_test.go` の 2 ケース（`{"0.6.0-rc1","0.6.0",true}` とチルダ綴り）は
  「今日の挙動の記録」から**契約のピン**になり、#804 を指すのをやめて
  この決定と `20260816/2024` を指す。
* 床の側の向きにピンを 2 本足した（`0.32.13` は `0.32.14-rc0` を越えない／
  `0.32.14` は越える）。`20260816/2024` が実際に依存しているのはこちらの
  向きなのに、テストは呼び出し側の向きしか見ていなかった。
* **prerelease を床として書けば、それは release を書いたのと同じ意味になる。**
  カタログを書く側はこれを知っている必要があり、`20260816/2024` は既に
  知った上で 0.32.13 を選んでいる。
* 今日、観測できる差は無い。出荷中の床（0.30.0 / 0.32.13）の prerelease は
  流通していない。この決定は将来それが起きたときに、驚きとしてではなく
  既定の挙動として扱われるようにするためのものである。

## Alternatives considered

* **`AtLeast` を prerelease-aware にする。** パッケージ内の順序が 1 本になり、
  「床が在るのは、その下に何かが欠けているから」という安全側の読みにも合う。
  棄却の理由は上記 2 点、とくに `20260816/2024` が反対の前提で値を決めて
  いること。ただしこれは筋の通った反対案で、床の側に prerelease を書きたく
  なった時点で再検討に値する。
* **`AtLeast` を消して `Compare` に寄せる。** 呼び出し元が 1 か所しか無い
  ので技術的には可能だが、上の 2 行表の区別そのものを失う。

## Refs

* waired-ai/waired-agent#804（この issue）
* waired-ai/waired-agent#781（`Compare` を prerelease-aware にした側）
* `docs/decisions/20260816/2024-qwen3-8-takes-the-27b-band.md`（この挙動を
  前提に ollama の pin を決めている）
* `internal/version/dotted.go` / `internal/version/dotted_test.go`
* `internal/router/model_picker.go`（唯一の非テスト呼び出し元）
