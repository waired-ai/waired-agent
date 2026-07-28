---
status: accepted
---

# daemon 経路の init が Claude ルーティングの単一決定者 (20260728 14:44)

## Status
Accepted

## Context

Claude Code のルーティングとは、OS 全体の managed-settings ファイル
（Linux なら `/etc/claude-code/managed-settings.json`）に
`ANTHROPIC_BASE_URL=http://127.0.0.1:9472` を書くこと、ただそれだけ。

インストーラは以前、init のあとに `waired claude enable` を別途実行していた。
それが init の「ルーティングしますか？」への「いいえ」を無条件で上書きして
いたため削除され、オプトアウトは `--skip-claude-proxy` /
`-SkipClaudeProxy` / `WAIRED_NO_CLAUDE_PROXY` → `waired init
--skip-claude-route` として転送する形に一本化された。install.sh と
install.ps1 のコメントは「init が唯一の決定者」と明言している。

ところが実際に managed settings を書いていたのは standalone enrollment 経路
だけだった。インストーラは init の前にサービスを起動するので、**実インストールは
100% daemon 経路を通る**。`runInitViaDaemon` は `skipClaudeRoute` を受け取らず、
`claudemanaged` を参照もしていなかった。結果:

- Linux / macOS / Windows-CLI の CLI インストールは未ルーティングで終わる
- `--skip-claude-route` は起きないことをスキップしていた

しかも #175 の後始末で standalone 経路自体が消えたため、`claudeRouteEligible`
（`!renewing` を要求）と `promptClaudeRouting` は到達不能なコードになっていた。

ブラウザのセットアップウィザードも同様だった。`desired_integrations` に
`claude-code` を載せて送るが、適用側は claudecode アダプタ = `~/.claude/skills/`
を置くだけ（アダプタのコメントにも "It writes nothing else"）。一方ウィザードの
UI 文言は「このコンピュータを使う全員に対して Claude Code の送信先を変更します
— 管理者所有の設定です」と machine-wide の変更を約束していた。**約束を裏付ける
実装が無かった。**

## Decision

**init（daemon 経路）とウィザードの両方がルーティングを行い、両方が
`--skip-claude-route` を尊重する。**

1. **判断は純関数 `planClaudeRoute(claudeRouteFacts) -> claudeRouteAction`**。
   `(integConsent, elevated, managedPath, skipClaudeRoute, nonInteractive,
   wizardDriving)` を事実として受け取り、`None` / `NeedsElevation` /
   `Apply` / `Ask` を返す。OS 差は `managedPath` として渡されるだけなので、
   1 台のホストから 3 OS 分を table test できる（CLAUDE.md §Cross-OS parity）。
   オプトアウトは能力ゲートより先に判定する — 「やるなと言われた作業」に対して
   昇格ヒントを出さないため。

2. **ターミナル経路**は waired#772 の deferred question を踏襲し、engine
   install → model download → benchmark の**後**に聞く（ローカルスタックが実際に
   応答できるようになった時点）。`--non-interactive` なら聞かずに適用。

3. **ウィザード経路**は `claude-code` トグルで適用し、決して聞かない
   （waired#835 §4.2）。昇格できていない executor はヒントを出すだけで、
   ウィザードの integration 行を赤くはしない — `waired claude enable` で
   あとから埋められる欠落であり、赤にすると回復可能なギャップが失敗に見える。

4. **書き込むのは常に昇格した CLI プロセスで、daemon ではない**
   （waired#935 §8.3）。daemon はサービスアカウントで動くので、root 所有の
   machine-wide ファイルを書かせると、その unauthenticated な management API に
   到達できる任意のローカルプロセスにとっての特権ブリッジになる。per-user の
   sudo hop（`runLinkAllAsUser`）も設計上 root を落とすので書けない。

5. **`applyClaudeRoute` を単一実装にする**。`waired claude enable` と init の
   両呼び出し口が同じ副作用（legacy MITM 掃除 → managed settings 書き込み →
   `/waired-route` スキル → statusline）を持つ。これまでは差があり、init 経路は
   `/waired-route` を入れていなかった。

6. **`--skip-integration` はルーティングも抑止する**（standalone 経路の既存契約と
   同じ）。ルーティングは Claude Code 連携の一部であって別製品ではない。

## Consequences

- CLI インストール（Linux / macOS / Windows-CLI）は、オプトアウトしない限り
  ルーティングされた状態で終わる。**無人インストールの既定が変わる**ため、
  docs-site の first-run に machine-wide である旨と opt-out を明記した。
- ウィザードの `claude-code` トグルの文言が実装で裏付けられた。private 側
  （`waired/web/admin/src/lib/setup.ts`）の変更は不要。
- installtest が全 leg で渡していた `--skip-integration` を既定オフにした。
  実インストールが渡さないフラグで、この欠陥を隠していた張本人。
  `assert_claude_route` は両方向（通常 = ルーティング済み / `--skip-integration`
  = 未変更）を検証し、常に 2 アサートを出すので assert-count floor が
  設定によらず成立する（tier2: 18 → 20）。
- GUI インストーラ（`packaging/windows/waired-setup.iss`）は init を実行しない
  ため、自前の `[Run] waired claude enable` を引き続き保持する。変更なし。
- `runInitViaDaemon` の 12 個の位置引数を `daemonInitOpts` に置き換えた。
  隣接する 3 つの string と 5 つの bool は、呼び出し口での取り違えが
  コンパイルを通ってしまう形だった。
- ルーティング sentinel はこの種の欠陥を捕まえられない。:9472 のゲートウェイを
  直接叩くのであって、Claude Code が実際に何を向いているかは読まない。
  e2e の担当は installtest 側にある。

## Refs
- https://github.com/waired-ai/waired-agent/issues/294
- `cmd/waired/init_route_claude.go`, `cmd/waired/setup_integration.go`
- `scripts/dev/lib/installtest-enroll.sh` (`assert_claude_route`)
- docs/decisions/20260727/2030-daemon-owns-reauthentication.md (#175 の系列)
