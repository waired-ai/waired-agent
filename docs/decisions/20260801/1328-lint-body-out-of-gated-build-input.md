---
status: accepted
---

# 前方一致ゲートが載せるファイルに、ゲートと無関係な中身を同居させない (20260801 13:28)

## Status
Accepted

## Context

`scripts/ci/testnet-gate.sh` の照合は **プレーンな前方一致のみ**（glob も regex も無い）。
これは意図的で、`scripts/ci/testnet-relevant-paths.txt` の header が
「policy file stays trivially auditable」と明記している。代償として、ゲートは
**ファイル内のどの部分が変わったか**を原理的に見られない。

`Makefile` は allowlist にファイル全体で載っている。これ自体は正しい —
`dist-agent` / `build-agent` / `dist-agent-testharness` は実際に testnet が使う
バイナリとハーネス入力を作る。しかし同じ Makefile が
`install-script-lint`（shellcheck 対象ファイルの一覧）という**開発者向け lint の
入口**も抱えていたため、出荷物に一切影響しない編集でも 25 分の実 NAT run が
armed になっていた。

`origin/main` 直近 60 コミットで Makefile を触った 6 件のうち 5 件が、Makefile だけで
ゲートを引いていた（#284 / #259 / #249 / `538df62` / `ec8cac1`）。うち 3 件は
「lint 一覧にスクリプト名を 1 行足す」だけの変更である。

allowlist の header は「境界的な変更で一覧を場当たりに編集するな、`run-testnet`
ラベルで強制しろ」と指示しているが、これは **force-ON の手段しか無い**。
force-OFF に相当する仕組みは存在せず、PR 側からこの誤検知を抑える方法が無かった。

## Decision

ゲートに例外機構（PR 本文での opt-out 等）は**足さない**。誤検知の原因そのもの —
ビルドと無関係な内容が build input ファイルに同居していること — を取り除く。

- shellcheck の対象一覧は `scripts/ci/install-script-lint.sh` が持つ。Makefile 側は
  そのスクリプトを呼ぶだけの 1 行のターゲットとして残す（`make install-script-lint`
  も ci.yml の `run:` も変えない ＝ required check `install-scripts` は無風）。
- **一般則**: 前方一致ゲートが載せているファイルには、そのゲートと無関係な理由で
  編集されうる中身を置かない。置き場所は `scripts/ci/`。この不変条件は
  `testnet-relevant-paths.txt` の `Makefile` エントリと `install-script-lint.sh` の
  ヘッダの両方に、理由付きで書いてある。
- CI スクリプトの lint 対象は `shellcheck scripts/ci/*.sh` で自動収集する。手書き
  一覧は既に 9 本（ゲート自身の `testnet-gate.sh` を含む）取りこぼしており、
  「新しい guard を足したとき一覧に書き忘れる」という失敗モードごと消す。

### 採らなかった案

PR 本文に `testnet-not-needed: <理由>` を足す案（`docs-not-needed:` /
`mirror-not-needed:` と同型）。実装は小さいが、(1) ゲートに抜け道を作る、
(2) `testnet-pr.yml` の `types:` に `edited` が無いため本文追記だけでは再評価されず
push が要る、という二点で劣る。

## Consequences

- lint 対象の追加は Makefile を触らない ＝ testnet を arm しない。過去 5 件を再生
  すると、Makefile 変更を除いた差分は全て skip になる。
- ただし **Makefile の編集が全て無害になるわけではない**。新しいターゲットの追加や
  ビルド変数まわりのコメント修正（#249 / #259 が該当）は依然として Makefile を
  触り、arm する。今回消えるのは「lint 一覧の編集」という最頻の経路である。
- 未 lint だった 9 本が新たに shellcheck 対象になった。指摘は 2 件だけで、同じ PR で
  実修正した（`gen-third-party-licenses.sh` の SC2129、
  `claude-code-canary.sh` の SC2034 = 未使用のループ変数）。以後 `scripts/ci/` に
  追加される shell は自動的に lint される。
- `ps-script-lint` は変更しない。対象一覧は既に `scripts/ci/ps-script-lint.ps1` 側に
  あり、Makefile 側の body（pwsh 存在チェック + 呼び出し）は安定している。
  `command -v pwsh` は pwsh の外でしか書けないため移設もできない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/292
- scripts/ci/install-script-lint.sh
- scripts/ci/testnet-relevant-paths.txt
- scripts/ci/testnet-gate.sh
