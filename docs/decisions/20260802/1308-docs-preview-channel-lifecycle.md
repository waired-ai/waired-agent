---
status: accepted
---

# docs プレビューチャネルのライフサイクル (20260802 13:08)

## Status
Accepted

## Context

`deploy-docs.yml` は docs-site を触る PR ごとに Firebase Hosting のプレビュー
チャネル `pr-<N>` を作るが、**削除する経路が一本もなかった**。Firebase は
サイトあたり 50 チャネルが上限で、このリポジトリの PR 流量（docs を触る PR が
日に 5〜17 本）だと `--expires 7d` 分が常に滞留し、サイトは恒常的に上限へ
張り付いていた。

その結果、docs を触る PR の `build + deploy (docs-site)` はビルドに成功しても
`hosting:channel:deploy` が `HTTP Error: 429 … channel quota reached` で死ぬ。
実測（2026-08-02）では 50 チャネル中、オープン中の PR に対応するものは 1 件だけ、
残り 48 件はクローズ済み PR の残骸、うち 3 件は期限切れなのに Firebase が
遅延回収していないものだった。

これは必須チェックではないのでマージは止まらない。だからこそ悪い:
「インフラ都合で常時赤い非必須チェック」は、本当の失敗で赤くなったときに
誰も読まないチェックになる。加えてプレビュー URL が出ないため、docs の変更が
レンダリング結果を見ずにレビューされていた。

## Decision

チャネルの**生成**は `deploy-docs.yml`、**破棄**は新設の
`docs-preview-reap.yml` が持つ、と役割を分ける。

1. **クローズ時に消す（本命）** — `pull_request: [closed]` で `pr-<N>` を削除。
   これでチャネル数は「オープン中の docs PR 数」に追随し、蓄積しなくなる。
2. **`--expires` を 7d → 3d** — 1 の取りこぼし用の保険にすぎないので、
   1 件の取りこぼしが枠を 1 週間占有しない長さまで詰める。値は
   `deploy-docs.yml` の `PREVIEW_TTL` に 1 箇所だけ定義し、デプロイフラグと
   PR コメント文言の両方がそこを参照する（従来は `7d` と "7 days" が
   別々に書かれておりドリフトしうる状態だった）。
3. **日次 sweep + 手動 dispatch（保険）** — 期限切れ、および PR がクローズ済みの
   `pr-<N>` を掃除する。1 が取りこぼす経路（docs 差分が消えた PR、close ジョブ
   自体の失敗、fork）を自己修復させ、一度の取りこぼしが静かに枠を埋め直すことを
   防ぐ。

**削除判定は `scripts/ci/docs-preview-channels.mjs` の純粋な stdin→stdout
セレクタに置き**、シェル側は firebase CLI の呼び出しだけを持つ。理由は爆風半径:
消す先は `docs.waired.ai` を配信しているのと同じ GCP プロジェクトなので、
「`^pr-\d+$` にマッチする id 以外は絶対に選ばない」「オープン PR 一覧の取得に
失敗したら *空集合として続行せず* 中断する」という不変条件を、実インフラに触らず
オフラインでテストできる形にする必要がある
（`scripts/ci/docs-preview-select-test.sh`、ci.yml の lint ジョブで実行）。

デプロイ直前のインライン sweep は**採らない**。1+2 があれば定常滞留は数件で、
毎回の docs PR に実行時間と失敗面を足す価値がない。取りこぼしは 3 が翌日拾う。

## Consequences

* プレビュー URL は PR がクローズされた時点で消える。マージ済み PR に残る
  コメントは「削除済み」に書き換える（404 するリンクを残さない）。
* 3 日以上放置された PR はプレビューを一度失うが、次の push で再生成される。
* live チャネル（prod-waired / `waired-docs-prod`）はこの経路から到達不能。
  reap ワークフローは `environment:` を持たないので dev の vars しか引かず、
  セレクタは `live` を構造的に選べない。
* `PREVIEW_TTL` を伸ばす変更は、上限 50 との関係を再計算してからにすること。

## Refs
- https://github.com/waired-ai/waired-agent/issues/429
- .github/workflows/docs-preview-reap.yml
- .github/workflows/deploy-docs.yml
- scripts/ci/docs-preview-sweep.sh
