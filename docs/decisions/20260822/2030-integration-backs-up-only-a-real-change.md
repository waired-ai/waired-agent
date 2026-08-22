---
status: accepted
---

# 設定のバックアップは「実際に変える」ときだけ取り、unlink は消さずに場所を伝える (20260822 20:30)

## Status
Accepted

## Context

OpenClaw アダプタの `Apply` は、`~/.openclaw/openclaw.json` が**存在しさえ
すれば**毎回そのコピーを `openclaw.json.waired-bak-<unix秒>` に取っていた
(`internal/integration/openclaw/adapter.go`)。判定に使っていたのは
「ファイルが在るか」であって「このマージが何かを変えるか」ではない。しかも
無変更でも `json.MarshalIndent` の結果を毎回書き戻していたので mtime も毎回
動いていた。

その結果、再 link のたびにファイルが 1 本ずつ増える。台帳
(`<state>/integrations/applied.json`)が覚えられる `backup_path` は 1 本だけ
なので、2 本目以降は書いた瞬間に台帳から辿れない残留物になる。
`waired unlink openclaw` は台帳駆動で外科的なので、追加したキーとプラグインは
消すが、この残留物は 1 本も消さない。

**実測(この開発機の WSL ホーム、2026-08-22)**: `~/.openclaw/` に
`openclaw.json.waired-bak-<ts>` が **183 本**あり(7/27〜8/2 の開発作業の産物)、
本体を含む **184 ファイルすべてが同一の md5** だった。つまり中身は一度も
変わっていないのに、バックアップだけが 183 回取られていた。台帳が指していたのは
最新 1 本だけで、残り 182 本は孤児。waired-agent#995 が「呼び出し位置からの推定」
として書いていた挙動は、実物で裏付けられた。

waired-agent#988 以降、tray の「Reconfigure…」行が
`waired link openclaw --force --no-prompt` をワンクリックで撃つようになった。
状態行のすぐ隣にあるので、設定を確認しながら数回押した人は、押した回数だけ
**他社製品が所有するディレクトリ**にファイルを増やすことになる。

兄弟も確認した。`integration.ManagedJSONConfig.BackupPath`(VSCode 系の設定を
claude-code アダプタが編集していた頃の形)は**書き手も読み手も残っていない**。
`VSCodeConfigs` 自体に producer が無く、旧台帳の巻き戻し専用として
`internal/integration/vscode` に残っているだけ。設定 JSON を書くアダプタは
今日 OpenClaw だけで、OpenCode と claude-code は自分が全所有するファイルしか
書かないため、この蓄積は起きない。

## Decision

**バックアップは、マージ結果が読み込んだバイト列と違うときだけ取る。同じなら
書き込みもしない。**

- `readConfigObject` は読んだ生バイトも返す。`Apply` は「読む → マージ →
  marshal → 比べる」の順に変え、`!existed || !bytes.Equal(body, raw)` の
  ときだけ backup + write を行う。収束済みの設定に対する再 link は、ファイルも
  その mtime も触らない。
- 無変更のときは、直前の台帳の `BackupPath` を**引き継ぐ**(そのファイルが実在
  するときだけ)。空で上書きすると、ユーザーが書いたままの設定がどこにあるかを
  唯一記録している場所が消える。ファイルが消えていれば引き継がない。
- `backupConfig` は `O_CREATE|O_EXCL` で作り、同一秒の衝突時は `-2`, `-3`… を
  付ける。名前は秒解像度なので、同じ秒に**内容の違う**バックアップを 2 回取ると
  先の内容が黙って消えていた。バックアップの存在意義は「他に写しが無い内容」を
  残すことなので、上書きは最も避けたい失敗になる。

**`waired unlink` はバックアップを消さない。残した場所を出力する。**

外科的除去はキーを元に戻すが、**キーの順序と書式は戻せない**
(`marshalConfig` は `json.MarshalIndent` なのでキーは辞書順になる)。つまり
バックアップは、ユーザーが書いたままのファイルの唯一の写しである。削除は
そのユーザーの持ち物を消すことになる。代わりに、台帳から場所を読んで
`unlink` の出力で名指しする — 名指しされていれば、それは「誰の物とも分からない
残留物」ではなくなり、消すかどうかはユーザーが決められる。`--dry-run` の
除去計画にも同じ 1 行を出す。

これは既存の反 sweep 前例と揃っている。`packaging/install/uninstall.sh` は
`/Applications/Ollama.app` を「このホストの Ollama.app を我々の物とは断定できない」
として消さずに名指しするし、`docs/decisions/20260821/0228-uninstall-removes-what-is-running.md`
は「**Waired を消す**のであって他人の物を消すのではない」を線として引いている。

**既存の残留物に対する移行処理は入れない。** リリース前であり、影響を受けるのは
開発中に何度も re-link した dogfood ホストだけで、必要なら手で消せる
(オーナー裁定 2026-08-22)。`<config>.waired-bak-*` を glob で掃く形は、
`unlink` は `link` が追加したものだけを取り消す、という公開ドキュメントの約束
(`docs-site/.../guides/openclaw.mdx`、`reference/cli.md`)にも反する。

## Consequences

- 収束済みの設定に対する `waired link openclaw` は、ファイルを 1 バイトも
  書かない。tray の Reconfigure を何度押しても増えるものは無い。
- ユーザーが自分で設定を編集して連携を壊した場合は、これまでどおり修復され、
  そのときだけバックアップが 1 本増える。中身は**編集後・マージ前**の姿になる。
- 台帳の `BackupPath` に初めて読み手ができた(`cmd/waired/link.go` の
  `recordedConfigBackups`)。これまでこの項目は書かれるだけで誰も読まなかった。
- `backupClock` という時刻の差し替え口をパッケージに足した。テストが「何本
  残ったか」を数えるとき、同一秒に走る複数の Apply が**同じファイル名**に
  落ちると、コードが何をしていてもアサートが通ってしまう(実際、修正を戻して
  試すと 6 本中 4 本しか落ちなかった)。並行テストからの差し替えは想定しない。
- ドキュメントを実装に合わせた。en/ja とも、バックアップの存在自体が書かれて
  いなかった(「そのファイルのほかは一切変更せず、ホームフォルダの外も変更しない」
  としか書いていなかった)。あわせて `#521` 以降ずれていた「3 モデルを追加」も
  1 つに直した。

## Refs
- https://github.com/waired-ai/waired-agent/issues/995
- https://github.com/waired-ai/waired-agent/issues/988 (tray の Reconfigure 行が CLI を撃つようになった経緯)
- docs/decisions/20260822/1742-integration-rows-belong-to-the-desktop-user.md
- docs/decisions/20260821/0228-uninstall-removes-what-is-running.md
- docs/decisions/20260805/1806-waired-aliases-are-dynamic-or-internal.md (再 link で旧 ref を消す形)
