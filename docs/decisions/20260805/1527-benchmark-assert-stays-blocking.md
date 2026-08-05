---
status: accepted
---

# init-benchmark assert は「モデル未準備」でも赤のまま (20260805 15:27)

## Status
Accepted

## Context

installtest の 3 ハーネスは `waired init` のトランスクリプトに `NUMBER tok/s` が
あることを要求する。この assert が「数値が無い」という 1 つのバケツに、原因の
異なる 2 条件を入れていた（waired-agent#382）。

- benchmark が**走って**結果が出なかった — 503、エンジンが起動しない、daemon 到達
  不能。エンジンを見るのが正しい。
- benchmark が**走らなかった** — モデルのダウンロードが init の待ち時間内に終わら
  なかった。エンジンは健全で、`waired init` は成功のエピローグを出して終了する。

後者が直近 9 件の routing sentinel 失敗のうち 6 件を占め、うち 3 件は inference に
無関係な `main` への push だった。いずれも `no benchmark THROUGHPUT figure` と
報告されたため、調査は毎回エンジンに向かい、そこには何も無かった。

引き金は外部要因である。model registry の実効スループットは同日内で約 7 倍変動する
（6.6 GB の同一モデルが 2m48s と >20m、run 30998191050 / 31014182148）。

## Decision

**未準備のケースも `bad`（赤）のまま維持する。変えるのはメッセージと診断出力だけ。**

- 数値あり → `ok`
- 数値なし ＋ 未準備を示す行あり → `bad`。ダウンロードを名指し、pull 側の証拠
  （`ollama list` on :9475、`/inference/status`）のみを出す。
- それ以外 → `bad`。従来どおりエンジンを名指し、`engine.log` と boot benchmark の
  slog を出す。

未準備の行は `IT_BENCH_NOT_READY_RE` / `$BenchNotReadyRe` として 3 ハーネスに宣言し、
`scripts/ci/harness-failure-strings-guard.sh` が (1) 3 コピーの一致 (2) 各分岐が Go
ソースに現存すること、を検査する。`IT_INSTALL_FAILURE_RE` と同じ扱いである。

## Rationale

skip（緑）にする案を採らなかった理由。

- 測定できなかったレグは、頼まれたことを検査していない。model registry が期限に
  間に合わないほど遅いという事実は、このスイートが報告し続けるべき条件である。
- 赤を消すのではなく、間に合わない状態自体を減らすのが正しい対処である。init が
  同じダウンロードを 10 分ずつ 2 回待つ構造（`waitForBundledModel` →
  `waitForBenchmark`）が、CI の壁時計を無駄にしている。こちらは別途修正する。

一方で、pull が実際に失敗した場合は同じ `assert_inference` 内の
`bundled model ready via mgmt API` が既に赤くなる。benchmark assert がそれを二重に、
しかも誤った名前で報告していた点だけが欠陥であり、それがこの決定の対象である。

## Consequences

- 赤の総数は変わらない。赤が名指す対象が変わる。
- assert 数のフロア（`installtest-run.sh` 23 / `installtest-macos.sh` 31 /
  `installtest-windows.ps1` 65）は `PASS + FAIL` で数えるため、変更不要。
- 未準備の行の文言を製品側で変えるときは、3 ハーネスの alternation を同じ PR で
  更新する。ガードが同 PR で落ちる。

## References

- waired-ai/waired-agent#382（本決定の対象）
- waired-ai/waired-agent#29（数値を要求するようになった経緯）
- waired-ai/waired-agent#505 / #300（この assert が赤の一因になっているレグ）
