---
status: accepted
---

# 測定記録の出自は「導出する」か「語彙で縛る」かのどちらかにする (20260829 11:00)

## Status

Accepted。waired-agent#1117。

## Context

カタログには測定の記録が 2 つある。`internal/catalog/agentgrade.json`
(ツール呼び出しの verdict) と `internal/catalog/requestshapes.json`
(リクエスト形状のマトリクス)。同じ 1 回の GPU レーン実行が両方を生む。

`agentgrade` 側は出自の欄をほぼ人の手入力で受け取り、**そのどれも検証して
いなかった**。

| 欄 | 出どころ | 検証 |
|---|---|---|
| `run_url` | `--run-url` | 無し (`shapes` には `runURLPattern` が在る) |
| `host` | `--host` | 無し。公開リポジトリで、doc コメント自身が「hardware CLASS であって identifier ではない」と書いている欄 |
| `retrieved` | `--retrieved` | 空かどうかだけ。`2026-8-030` が通る |
| `engine_version` | `--engine-version` | 無し。しかも `omitempty` で、空でも被覆の穴にならない |

同じファイルが `transport` については逆の立場を明記している:

> Transport is DERIVED from what was actually driven — never typed. An
> operator-supplied value could claim a path nobody ran

実害は出荷済みのストアに出ていた。**18 件の verdict すべてが `run_url` を
持たない**のに、同じ種類の実行が生んだ形状記録 1 件は Actions の URL を
持っている。原因は単純で、CI レーンが刷る import コマンドが `--run-url` を
渡していなかった — そして `--retrieved` も渡していなかったので、**その
コマンドは刷られたとおりに実行すると必ず失敗していた**。

`engine_version` は放置できない。同じモデルの同じリクエスト形状が
ollama 0.32.13 では拒否され、0.32.14 では通り、0.32.15 では先頭ターンに
畳まれる。エンジンの版が言えない verdict は、後から判断をやり直せない。

## Decision

**1. 観測できるものは導出する。** `engine_version` はレポートが積んでいる
形状マトリクス (`.shapes.engine_version`) から取る。この値は e2e が
ランタイムアダプタから読んだもので、形状プローブ自身の doc が
「never typed by an operator: the version is the finding」と書いている。
`probeReport` が `shapes` フィールドを宣言していなかったので届いていなかっ
ただけで、読み口 (`shapeReportFile` / `readShapeReport`) は既に在った。

- `--engine-version` はマトリクスを持たないレポート専用に残す。
- レポートが観測した値と食い違うフラグは**拒否**する。
- エンジン版の異なる 2 つの実行は **pool しない**(`pool()` が
  fixture/harness の版で既に取っている立場と同じ)。
- `VariantAgentGrade.EngineVersion` から `omitempty` を外し、
  `VariantRequestShapes.EngineVersion` と揃える。

**2. 観測できないものは語彙で縛る。** `--host` は
`catalog.HostClasses` の宣言済みリストに限る。正規表現で「識別子っぽさ」を
判定しない — `sv-mag` と `apple-unified-64gb` はどちらも小文字とハイフン
であり、パターンは必ず推測になる。リストなら推測せず、クラスの追加は
レビューされる 1 行の差分になる。**両ストアが同じ 1 本のリストを読む**
(`RequestShapeGaps` が agentgrade の `unmeasurable` を使い回しているのと
同じ理由 — 語彙の綴りが 2 つあることが、2 つのストアが食い違い始める道)。

**3. 残る手入力は両側で同じ形に検査する。** `checkRetrieved` /
`checkHostClass` / `checkRunURL` を `cmd/catalog-tool` に 1 組だけ置き、
`agentgrade` と `shapes` の両方が呼ぶ。日付は正規表現でなく
`time.Parse` — 形だけ見る正規表現は `2026-13-45` を通す。
`--host` と `--retrieved` は import 時に**必須**にする(現に 19/19 の記録が
持っている)。

**4. 書き込み経路をテストから触れるようにする。** `agentgrade` に
`--store` を足す(`shapes` と同型)。これが無かったため、この importer には
**成功 import のテストが 1 本も存在せず**、拒否のテストしか無かった。
`shapes.go` のコメントがこの穴を名指しで記録していた。足したうえで、
記録された構造体の全 exported フィールドが非ゼロであることを reflect で
歩く。記録は毎回まっさらな構造体リテラルとして組まれるので、
**型に足したフィールドは次の import で黙って落ちる** — これを恒久的に
塞ぐのはこの主張だけ。

**5. 2 つのストアの突き合わせは 1 本だけ入れる。** 「形状記録を持つ変種は
verdict も持つ」。これは今日成立し、意味がある(エンジンがメッセージを
受け付けることと、モデルがツール呼び出しを駆動できることは別の主張)。

**逆向きは入れない。** 18 件の verdict は形状の表より前のもので、
「全 verdict に形状を要求する」は到着した瞬間に赤になる — それは baseline
免除が記録している当のこと。`host` / `agent_revision` / `retrieved` の
一致も**入れない**: 出荷済みの記録は数か月離れた実行から来ているので、
等値の主張は今日落ちるか、落ちないところまで条件を絞った結果**何も
検査しなくなる**かのどちらかになる。

## Consequences

- レーンが刷る import コマンドが**実行できるようになる**。`--retrieved` と
  `--run-url` を含み、`--host` はマシンタイプを決めている場所の隣で宣言
  した `GPU_LANE_HOST_CLASS` から来る。以後 lane 産の記録は
  ローカル測定と区別できる。
- 新しいハードウェアクラスで測ったら、同じ PR で
  `internal/catalog/hostclass.go` に 1 行足す必要がある。これは手間では
  なく、公開リポジトリにマシン名が入らないことのレビュー機会。
- 形状マトリクスを持たない古いレポートは `--engine-version` を明示すれば
  今も import できる。持たず、フラグも無いレポートは拒否される。
- 出荷済みの `internal/catalog/agentgrade.json` は**書き換えない**。
  18 件の `run_url` は空のまま残る(それが実際に起きたこと)。埋めるには
  再測定が要る。

## Refs

- waired-agent#1117
- waired-agent#1095 / #1099 / #1115 (形状ストアとゲート)
- `docs/decisions/20260828/1930-arm-the-request-shape-gate.md`
- `internal/catalog/requestshapes.go` (2 つのストアを分けた理由と、食い違う危険)
