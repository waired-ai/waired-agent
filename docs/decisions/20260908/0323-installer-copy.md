---
status: accepted
---

# インストーラの文言: 人手のインストーラに合わせ、製品文言規約を適用する (20260908 03:23)

## Status

Accepted

## Context

製品文言の規約(`docs/decisions/20260907/0327-product-copy-conventions.md`)は CLI・
Waired アプリ・お知らせ・NAVI に適用され、インストーラ(`packaging/install/install.sh`
/ `install.ps1` / `uninstall.sh` / `uninstall.ps1`、Inno ラッパー
`packaging/windows/waired-setup.iss`、Debian の `postinst` / `prerm`)は「別パス」として
保留されていた(オーナー承認 2026-09-08、#1287)。

棚卸し(2026-09-08)で分かった状態: 短縮形がゼロ(`could not` 約 30、`did not`、
`cannot`)、`please` 1、`machine` がバナーの tagline と Windows の recap に、`tray` が
約 20 行、サービスの呼び名が `agent service` / `the agent` / `background service` の
3 通り、`enrol` / `enroll` / `Enrolled` の混在、同じ文が sh では `—` `…`、ps1 では
`--` `-` `...` と 3 通りに見える、絵文字 7 種のフォールバックが CLI の表
(`cmd/waired/ascii.go` の `statusMarkFolds`)と不一致(`[ok]` / `[*]` / `Note:`)、
大文字強調(`NOT` / `THERE` / `KEPT` / `PERMANENT`)、同じ意味の別文が 25 組。

参照したのは人が書いたインストーラの実文字列: Tailscale、Ollama、Docker、rustup、
Homebrew(install と uninstall)、uv、Deno、Bun、nvm、Scoop、Chocolatey、Inno Setup の
`Default.isl`、および clig.dev、Microsoft の Win32 error-message guidelines と
style guide、Google の error-messages / word-list。14 本に共通していたのは、接頭辞は
helper 1 か所が決める、進行行は動名詞＋`...`、成功行は成果物と場所を名指す、次の一手は
そのまま貼れるコマンド、エラーは原因→対処を別行で、dry-run は動詞の時制を変えるだけ、
プロンプトは既定を大文字で示す、非対話は検出して言い抜け道のフラグを名指す、絵文字は
ゼロ、`!` は成功行に 1 つまで、大文字強調をしない、`please` を状態行に書かない。

## Decision

オーナー決定(2026-09-08):

1. **記号は CLI と同じ表を使う。** `waired init` と同じ ✅ ⚠ 🎉 ⬆ ℹ と、`cmd/waired/ascii.go`
   の `statusMarkFolds` と同じフォールバック(`*` `!` `*` `^` `i`)。表に無い 🔧 💡 🔑 は
   使わず文で言う。`scripts/install/emo_fallback_test.go` が全 `emo` / `Emo` 呼び出しを
   表と突き合わせる。
2. **sh と ps1 は両側 ASCII の同一文。** `—` は文を切り直す(`.` `:` `,`、`so` /
   `because`)、省略は `...`。曲線引用符・`→` を文に使わない。バナーの装飾行だけは
   glyph ゲート＋base64 鏡像のまま(`scripts/install/banner_mirror_test.go`)。
3. **警告は `[waired] Warning: <文>` の 1 形。** `common_warn` / `Common-Warn` が付け、
   本文には書かない。警告でない呼び出し(案内、前行の対処行、想定内の分岐)は無印の
   `common_log` / `Common-Log` へ。
4. **範囲に Inno の自前文字列と Debian の保守スクリプトを含める。**

規約(製品文言規約 0327 の下位):

- 接頭辞 `[waired] `(info 水色 / 警告 黄 / 失敗 赤)は維持。失敗は
  `[waired] <何が起きたか>. <どうするか>`。`Error:` は書かない。
- 短縮形を全面で(`couldn't` / `didn't` / `can't` / `isn't` / `won't` / `doesn't`)。
- 名詞: **computer**(この機、本文)/ **device**(コンソール上の登録に限る)/ `machine` は
  書かない(Windows の `machine PATH` は **the system PATH**)。`tray` は書かない:
  **the Waired app**(ラベル `Waired app:`、操作は `open the Waired app's menu and pick
  "Sign in..."`)。プロセス名 `waired-tray`、`WAIRED_NO_TRAY`、bundle id、launchd
  ラベルはそのまま。GNOME の拡張は `the AppIndicator extension`。
- サービスの呼び名は 1 つ: **the background service**(`waired init` の
  `The background service is running.` と同じ)。
- **sign in / signed in**(決定 `docs/decisions/20260803` 系、waired 側 1355):
  `Enrol this device against` → `Sign in to <url>`、`Enrolled` → `Signed in`、
  `enrolment did not complete` → `Sign-in didn't finish`。
- sentence case。大文字強調をしない。`!` はコマンド例 `"hello, world!"` 以外に書かない。
  `please` / `sorry` / `invalid` / `Oops` を書かない。
- 端末なしの案内は 3 OS で同じ 4 行(`ℹ No terminal detected. Sign-in was skipped. To
  finish setup:` ＋ 3 つの選択肢)。端末ありの既定と端末なしの fallback は一文に畳まない
  (waired 側の決定 20260726/1811)。
- pin 付きの行は語を残すか、同じ PR でアサートを動かす: `Waired is installed.` /
  `Waired is installed (macOS, <arch>).` / `Ask for administrator rights` /
  `Stopping the Waired app (waired-tray, PID <n>)` / `No terminal detected` /
  `Update available: X -> Y`。`Local inference is not running on this device.` は
  `Local inference isn't running on this computer.` に再裁定(TRANSLATION.md の
  `local inference` 行)。

守り方: `scripts/ci/installer-copy-guard.py`(規則は `scripts/ci/installer-copy-rules.txt`、
免除は同じ行の `copy-ok: <why>`、空振りの免除は失敗)が 4 スクリプト・Inno・Debian の
印字文字列(helper 呼び出し、heredoc / here-string の本文、`[Code]` のリテラル)を読む。
`scripts/ci/install-script-lint.sh` から呼ばれ、自己テストは
`scripts/ci/installer-copy-guard-test.sh`。

## Consequences

- 1 本目の PR は機械的な適用(短縮形、名詞、`tray`、`enrol`、記号、句読点、`Warning:`、
  大文字強調)とガード。内容(事前サマリ、Done ブロック、更新、失敗と復旧、アンインストール
  の完了文)は後続の PR で、承認表を経て動かす。
- インストーラの出力を読むハーネス(`scripts/dev/installtest-dash.sh`、
  `installtest-pwsh.ps1`、`installtest-windows.ps1`、`installtest-macos.sh`、
  `installtest-run.sh`)と公開 docs の逐語引用(en / ja)は、文言と同じ PR で動く。
- `waired update` はチャンネルのリリース資産からインストーラを落とすので、文言は次の
  edge ビルド／次の stable タグで届く。merge では届かない。
- `install.ps1` / `uninstall.ps1` は純 ASCII のまま(`scripts/install/encoding_test.go`)。
  `Get-ExitCodeReason` / `ConvertTo-NativeArg` は両ファイルでバイト一致のまま。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1288
- https://github.com/waired-ai/waired-agent/issues/1277
- https://github.com/waired-ai/waired/issues/1330
- docs/decisions/20260907/0327-product-copy-conventions.md
- docs-site/TRANSLATION.md
