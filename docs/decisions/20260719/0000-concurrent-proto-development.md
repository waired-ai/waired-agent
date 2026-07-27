---
status: accepted
---

# proto の並行開発運用 — auto-tag / additive-only guard / 擬似バージョン許可 (20260719)

## Status
Accepted

## Context
`proto/` は control plane / relay からも import される共有 wire contract で、公開バージョン(`proto/vX.Y.Z`)は不変。複数の開発セッションが同時に proto へ変更を積もうとすると、タグの採番・打鍵と消費側のタグ待ちが直列化ボトルネックになる。一方で契約を git ブランチで分岐させることはできない — デプロイされたフリートが喋るプロトコルは常に 1 本のマージ済みタイムラインであり、ブランチ性は「未確定作業の隔離」にしか使えない。

## Decision
1. **開発中は main に触れない**: 本リポジトリは in-tree `replace`、消費側リポジトリは一時 `replace` または「マージ済み main コミット」の擬似バージョンで参照する。ブランチ上のコミットハッシュへの依存は禁止(rebase で消える)。タグへの正規化は後続の 1 行 chore。
2. **設計確定ゲート**: proto 変更は tracking issue に確定フィールド表が載り actionable になってから、単独の小 PR としてマージする。公開はラチェット(取り消せない)なので、確定前の契約面を main に積まない。
3. **additive-only を CI で機械強制**(`proto-guard` / `scripts/ci/protoguard`): 直前タグとの比較で、公開済み exported API の削除・型変更・struct タグ変更・const 値変更・シグネチャ変更を fail。公開済み struct へのフィールド追加は `omitempty`(または `json:"-"`)必須。canonical JSON バイト同一性テストを同 PR の必須慣行とする。
4. **タグ自動発行**(`proto-tag.yml`): main への `proto/**` マージごとに次のパッチタグを自動で打つ(concurrency group で採番レースなし)。minor/major は workflow_dispatch で手動。`proto/v0.2.0` までは休止(マイルストーンタグは手動で 1 回)。
5. **実行時の分岐は capability 文字列**(例: `public-share-v1`)で行い、未完成の契約面はエージェントが宣言するまで不活性に保つ。

検討して退けた代替: submodule pin(バンプ競合が gitlink に移るだけで UX が悪化)、擬似バージョン一本化(MVS の順序意味論と go.mod diff のレビュー可視性を喪失)、ソースコピー同期(真実の源が二重化し drift 検査という新しい機械が必要)。

## Consequences
- 複数セッションの proto 変更が採番調整なしに並行でき、直列区間は main へのマージ順序のみになる。
- 公開済み契約は消せないため、品質の関門は git 構造ではなく設計確定ゲート(2)。誤公開は `retract` + deprecated コメントで処理する。
- 追加フィールドの既定が omitempty + capability ゲートに固定されることで、署名済み map のバイト同一性(非対応 poller への互換)が構造的に守られる。

## Refs
- .github/workflows/proto-tag.yml / proto-guard.yml、scripts/ci/protoguard/
- https://github.com/waired-ai/waired-agent/pull/84 (proto/v0.2.0 契約、waired#816)
