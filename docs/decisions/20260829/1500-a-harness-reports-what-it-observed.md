---
status: accepted
---

# 面は自分が観測したことだけを報告する (20260829 15:00)

## Status

Accepted。waired-agent#1118 / #1119 / #1145。

## Context

3 件の独立した欠陥が同じ形をしていた。**ある面が、自分で観測していない
結果を肯定的に報告している。**

**#1118** — ルーティング番人の wrapper は
`coding-agent routing sentinel: every leg served locally (no fail-open)`
と刷るが、根拠は `go test` の終了コードだけだった。その終了コードは
「4 本のレグが全部ローカルで配信された」ことを含意しない。3 通りの無言 pass が在る:

1. daemon に届かなければ無条件 skip。
2. `WAIRED_INTEGRATION_LEGS` がどのレグにも当たらない名前を持つと選択が空
   になり、subtest が 0 本走って PASS。
3. そもそも `internal/e2e/integration/budget_test.go` は
   **ビルドタグを持たない**ので、`go test -tags integration ./...` の
   終了コードは daemon に触れない算術テスト 7 本だけで満たされる。
   つまり終了コードは `TestIntegration` が走ったことすら含意しない。

同じ断言文は 3 ファイル(linux lib / macos sh / windows ps1)に逐語で複製
されていた。

**#1119** — `docs-surface-guard.sh` の SURFACES はカタログを見ていなかった。
`proto/hostfit/` が載っている理由(ガード自身のコメント)は「印字される
文字列は変わらないが、**渡されるモデルが変わる**」。マニフェストの追加・
退役はまさに同じものを変える。しかも `CLAUDE.md` の Documentation 節は
既に「モデルカタログ」を対象と明記し「`docs-guard.yml` が上記を強制する」
と書いていた — 正規表現だけが追いついていなかった。

**#1145** — `ci.yml` の「integration-tag build smoke」は
「タグの向こうは常時レーンがコンパイルしない」と正しく述べたうえで、
**1 パッケージにしか適用していなかった**。`integration` タグは 4 パッケージ、
`e2e` / `e2e && gpu` は 2 パッケージあり、残り 5 つはどの必須チェックでも
コンパイルされていなかった。

## Decision

**1. ハーネスは空のまま成功できないようにする(#1118)。**

- 呼び手の**要求を先に検査する**。`WAIRED_INTEGRATION_LEGS` がどのレグにも
  当たらない名前を含むのは、仕事の不在ではなく要求の誤り → `t.Fatal`。
  daemon を触る前に判定する(世界の状態でなく、呼び手が何を頼んだかで決まる)。
- 選択が空なら `t.Fatal`。
- daemon 到達不能は、**呼び手が `WAIRED_MGMT_URL` を明示していたら失敗**、
  していなければ従来どおり skip。#956 が agent-harness レーンで決めた形と
  同じ。素の `go test -tags integration ./...` は今も skip する。
- ハーネスが `WAIRED_INTEGRATION_SUMMARY` に**実際に配信されたレグ名**を
  1 行ずつ書き、3 つの wrapper がそれを読んで
  `N leg(s) served locally, no fail-open (<names>)` と**走ったものを言う**。
  全称の断言をやめる — wrapper はレグが何本あるかを知らない。

**`go test` の出力を grep する形は採らない。** `--- PASS: <name>` への
grep はリネームで黙って空振りし、それを検知する手段が無い。
`scripts/ci/harness-failure-strings-guard.sh` はまさにこの結合を取り締まる
ために存在する。書き手が書いた成果物を読む方が、書き手が変わっても壊れない。

**2. カタログは docs サーフェスに入れる。ただし生成物では合格させない(#1119)。**

SURFACES に `proto/catalog/` / `internal/catalog/` / `internal/agentgrade/`
を追加する(オーナー裁定 2026-08-29、issue 本文が挙げる 3 つすべて)。

**同時に、合格判定から生成物を外す。** `make catalog-docs` は
`docs-site/src/data/model-sizes.json` を書き、合格判定は `^docs-site/` の
grep だった。つまり**マニフェストを変えたら必ず回す手順が、ガードを自分で
満たしてしまう** — 範囲を広げた当の場面で空振りする。`package-lock.json`
も同じ理由で外す。除外は生成物と lockfile だけで、手書きのローダ
(`model-catalog.ts`)は外さない。

`docs-not-needed:` は従来どおり通る。測定の取り込みや純粋な移設は
1 行書けば済む。

**3. ビルドタグの向こうはモジュール全体を vet する(#1145)。**
ディレクトリを名指す代わりに `go vet -tags <set> ./...` を 3 セット回す。
3 つとも現状 exit 0 なので、片付ける在庫は無い。

**4. 変えたガードには自己テストを付ける。** `scripts/ci/` の 5 本のガードは
自己テストを持つ慣行があり、`docs-surface-guard.sh` だけ無かった —
そしてこの決定はその判定ロジックを変える。12 ケース、うち 1 つは
「マニフェスト＋生成物だけ」が赤になること。今日の木で緑になるだけの
テストは、ルールが無くても緑になる。

## Consequences

- **番人が daemon に届かない夜は赤くなる**。従来は無言の緑だった。
  `routing-sentinel.yml` は PR ごとに回るので、新しい赤の発生源が 1 つ増える。
  これは意図した代償: wrapper は既に「届いた」と刷っていた。
- **測定を取り込むだけの PR も docs か `docs-not-needed:` を要求される**
  (`internal/catalog/*.json` は import が書くため)。1 行で済む。
  grade が下がって withheld になる import は実際に渡されるモデルを変える
  ので、望ましい側に倒れている。
- `docsSizesFile` の綴りと、ガードの除外リストの綴りが**食い違わないこと**を
  Go のテストが主張する。片方だけ変えれば落ちる。
- `WAIRED_INTEGRATION_LEGS` はリポジトリ史上一度も設定されたことがない
  (`git log --all -S` で 0 件)。それでも直すのは、これが**壊れた道具**
  だからで、使われていないことは正しさの根拠にならない。

## Refs

- waired-agent#1118 / #1119 / #1145
- waired-agent#956（名指した測定が取れないなら失敗、の元になった裁定）
- waired-agent#29（タグの向こうの typo が 15 分後にしか出なかった件）
- `93d80b76` / waired#988（`proto/hostfit/` を SURFACES に足した先例）
- `docs/decisions/20260828/1930-arm-the-request-shape-gate.md`
