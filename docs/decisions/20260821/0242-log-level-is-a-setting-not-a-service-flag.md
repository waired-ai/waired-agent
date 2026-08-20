---
status: accepted
---

# インストール時のログレベルは永続設定であってサービス定義の一部ではない (20260821 02:42)

## Status
Accepted

## Context

`waired-agent` のログ詳細度には入口が二つある。

- インストール時: `install.sh --log-level` / `install.ps1 -LogLevel` / `WAIRED_LOG_LEVEL`
- 実行時: `waired config log-level <lvl>` — 再起動なしで反映し `agent.json` の
  `logging.level` に永続化する（`cmd/waired-agent/log_control.go`）

`internal/agentconfig` の `ResolveLogLevel` は起動時に
**`--log-level` フラグ > `$WAIRED_LOG_LEVEL` > `$WAIRED_DEBUG` > agent.json > info**
の順で解決する。ところがインストーラは install 時の値を**サービス定義そのもの**に
焼き込んでいた。

| OS | 焼き込み先 | 順位 |
|---|---|---|
| macOS | LaunchDaemon plist の `ProgramArguments` | 1 |
| Windows | SCM ImagePath の引数 | 1 |
| Linux | `/etc/waired/agent.env` の `WAIRED_LOG_LEVEL=` | 2 |

`waired config log-level` が書くのは順位 4 なので、**サービスが再起動するたびに
install 時の値へ黙って戻る**。戻す再起動は `waired update` のたび（インストーラは
どの OS でもサービス定義を書き直さない）と、モデル切替時の exit 17 自己再起動を
含む。rc9 キャンペーンの実測で debug のログ量は macOS/Windows とも **≈2.7 MB/h**
（waired-ai/waired#1217）。出荷ドキュメントは逆を約束していた
（`docs-site/.../reference/install-options.mdx`「再インストールせずに変更できる」、
`reference/cli.md`「再起動後も保持されます」、`management.LogController` の doc
コメントも同文）。

出典: waired-ai/waired-agent#801（macOS で起票、Windows で再現、Linux も同型）。

## Decision

**install 時のログレベルは、サービス定義ではなく永続設定 (`agent.json`
`logging.level`) に入れる。** 焼き込みは 3 OS すべてで廃止し、インストーラは
サービスが上がったあとに `waired config log-level <lvl>` を呼ぶ。

これで唯一の永続的な出所は `agent.json` になり、`--log-level` は
「foreground 実行のその場限りの上書き」という本来の意味に戻る。優先順位表
（`ResolveLogLevel`）は変更しない。

書き込みは**必ず稼働中の daemon 経由**で行い、CLI の daemon 停止時分岐には
落とさない。落ちてはいけない理由が二つある。

1. **`agent.json` が daemon の初回起動前に存在すると、ハードウェア対応の
   bundled-model 選択が恒久的に無効化される。** ゲートは
   `shouldAutoSelectBundledModel(agentJSONExists, preferenceExists, intent)` の
   `!agentJSONExists`（`cmd/waired-agent/bundled_model_select.go`、
   waired-ai/waired#756）。スペック未満のホストが inference 有効で既定モデルを
   引く状態になる。
2. **Linux の daemon は `User=waired` で動く。** root が書いた
   `/var/lib/waired/agent.json` は、postinst の `chown -R` が存在する理由その
   ものの所有権不整合を作る。

したがってインストーラは daemon を待ち、応答しなければ**書かずに警告する**。
待機の条件は `/waired/v1/status` ではなく `waired config log-level` の読み取り
そのものにした — status はループバック TCP でも供されるので、書き込みが必要と
する IPC ソケットがまだ無い状態でも緑になってしまう。

## Consequences

- 新規インストールでは `waired config log-level` の選択が再起動・update・
  モデル切替の自己再起動を越えて残る。出荷ドキュメントの記述が真になる
  （文面の変更は不要）。
- **既にサービス定義に固定を抱えているホストは、この変更では直らない。**
  update 経路はサービス定義を書き直さないため。オーナー裁定により今回の範囲は
  新規インストールのみ。既存ホストの移行は waired-ai/waired-agent#934。
  なお macOS/Windows は再インストールで定義が作り直されるので自然に解消するが、
  Linux の `agent.env` の行は再インストールでも残る。
- **一点だけ後退する。** `--log-level` を指定し、かつ daemon が起動しない
  ホスト（非 systemd、サービス起動失敗）では、レベルが適用されず警告だけが出る。
  以前は定義に焼けば必ず効いていた。適用されなかったレベルはコマンド 1 本で
  回復でき、上記 2 つの失敗は一切目に見えない、という比較で受容した。
- `--log-level` 指定時に限り `agent.json` が 1 ブート早く生まれる。boot 時の
  bundled-model 選択は同じブートで既に走った後なので当該ブートは無影響。
  `--skip-ollama --no-init --log-level X` の組み合わせでのみ、次回ブートの選択が
  走らなくなる。指定した人にだけ起きる狭い経路として受容。

## Alternatives considered

- **`--log-level-default` という新しいフラグを足し、agent.json より下の順位に
  置く。** サービス定義には焼くが負ける、という形。daemon を待つ必要がなく上記の
  後退も無い。**却下**: `ResolveLogLevel` の `cfgLevel` は
  `agentconfig.Defaults()` 由来の `"info"` で埋まっているため、agent.json の下に
  新しい順位を足しても到達しない。到達させるには `Defaults()` の
  `Logging.Level` を空にする必要があり、そうすると `bundled_model_select` /
  `residency_control` など既存の全 `Save` 経路の出力が変わる。さらに、既に
  `"level":"info"` が書かれている現行フリートでは、再インストール時に
  `--log-level` が無言の no-op に化ける — 直そうとしている欠陥と同じ型。
- **`logging.level` に出自マーカーを足し、operator が設定した値がフラグに勝つ
  ようにする。** **却下**: プロセスからは「plist 由来の `--log-level`」と
  「人が端末で打った `--log-level`」が区別できない（同じ `fs.String` に届く）。
  順位を逆転させると、今打ったフラグが agent.json に負けることになり、直そうと
  している欠陥より悪い。
- **daemon がサービス定義に書き戻す。** **却下**: Linux の unit は
  `ProtectSystem=strict` / `ReadWritePaths=<StateDir>` / `NoNewPrivileges=yes` で、
  `User=waired` の daemon は `/etc/waired/agent.env` にも unit にも書けず昇格も
  できない。
- **起動時に「フラグ/env が別の永続値を上書きしている」と警告する。**
  **却下**: 固定が消えれば発生しない状態への警告であり、新規のユーザー向け文言が
  増える。回帰は installtest の 3 OS レグ（下記）が CI で捕まえるほうが早い。

## Verification

回帰ガードは「レベルを設定 → サービス再起動 → 読み戻す」を 3 OS の installtest
レグに置いた（Windows は実 SCM、macOS は実 launchd、Linux は LXD ゲストの実
systemd）。Windows は `$ContractBlocking['801'] = $true` で最初から blocking。
`installtest-dash.sh` には dry-run とスタブ実行の両方でケースを足し、**固定を
一時的に戻すと落ちることを実測して確認した**（Linux 側 = `WAIRED_LOG_LEVEL=` の
再出現、darwin 側 = 登録 argv への `--log-level` の再出現）。

## Refs

- waired-ai/waired-agent#801
- waired-ai/waired-agent#934 (既存ホストの移行 — 本記録の Consequences より)
- waired-ai/waired#1223 / record waired-ai/waired#1217
- `cmd/waired-agent/bundled_model_select.go` (waired-ai/waired#756)
- `internal/agentconfig/config.go` `ResolveLogLevel`
