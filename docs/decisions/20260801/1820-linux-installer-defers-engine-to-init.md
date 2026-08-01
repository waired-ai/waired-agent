---
status: accepted
---

# Linux インストーラはエンジン導入を init に委ねる (20260801 18:20)

## Status
Accepted

## Context

`install.sh` の Linux パスだけが、サインイン前・consent 前に無条件で
`waired runtimes install ollama` を実行していた（`linux_apt_install` 内の
`AI engine (Ollama)` セクション）。macOS と Windows は PR #55/#73 で先行
インストールを撤去済みで、その PR 自身が「Linux の state-dir pre-install は
意図的に据え置き」と明記していた。据え置きの根拠は「Linux の `waired init` は
standalone 経路を通るのでエンジンを自力で入れない」だったが、

* PR #119 で daemon 経路が 3 OS すべての既定になり、
* PR #205 で当時のブロッカー（PATH-only の `engine_installed` probe, #179）が解消し、
* `docs/decisions/20260727/2100-delete-local-enrollment.md` で standalone 経路
  そのもの（`ensureBundledEngine` 含む）が削除された

ことで、根拠は 3 つとも消えていた。0.0.2-rc7 の内部レビュー（waired#986）では
実機 3 OS で、ブラウザのウィザードを開いた時点で「Install the AI software」が
既に done になっている（= 質問の前に ~1.4GB 落ちている）状態が観測された。

`packaging/install/README.md` と docs-site は 3 OS すべてについて既に
「エンジンはセットアップ中に、モデルを動かす端末にだけ入る」と書いており、
コードだけが仕様から外れていた。

## Decision

Linux も `linux_install_ollama` を撤去し、エンジンの**判断も導入も**
`waired init` が持つ。導入経路は既存のものをそのまま使う:

* ブラウザのウィザードが動いている場合 —
  `cmd/waired/setup_install.go` `runSetupEngineInstall`（executor がリースを
  保持したまま実行）
* ウィザードが無い場合（`--non-interactive` / `--no-browser` / ヘッドレス /
  端末を返した場合）— `cmd/waired/init_daemon_inference.go`
  `ensureDaemonPathEngine`（daemon の inference subsystem が `no_engine` の
  ときだけ導入）

いずれも daemon が申告した state dir に入れるので、Linux の strict な bundled
resolver が要求する `/var/lib/waired/runtimes/ollama/bin/ollama` と同じ場所に
落ちる。`--skip-ollama` / `WAIRED_NO_OLLAMA` は今後「init に入れさせない」
という意味だけを持つ（Windows/macOS と同じ）。

併せて、apt の前提パッケージから `zstd` を落とした。根拠だった上流
`ollama.com/install.sh`（`.tar.zst` を外部 zstd で展開）はもう使っておらず、
現在は `internal/runtime/ollama_install.go` の `extractTarZst` が
`klauspost/compress` で in-process に展開する。

## Consequences

* `--no-init` / 端末なし / 非 systemd のホストは、最初の `sudo waired init`
  までエンジン無しで終わる。これは Windows の `-SkipInit` と同じ契約であり、
  consent ゲートの意図どおりの挙動。完了バナーの `Ollama:` 行はその場で
  実測して、入っていなければ「installed by sign-in when local inference is on」
  と言う（`darwin_next_steps` と同じ 3 分岐）。
* インストール前サマリはサインイン → エンジンの順に並べ替え、エンジン行は
  「サインイン時に、ここでモデルを動かすと答えた場合だけ」であることを明示する
  文言に変更（install.ps1 も同文言にミラー）。
* per-PR の installtest は `--skip-ollama --no-init` で走るためエンジン導入
  経路を踏まない。実際の導入は nightly の `installtest-inference.yml`
  （`--inference` / `--daemon-engine`）が担保する。
* 退行検知は `scripts/dev/installtest-dash.sh` の 2 ケース（`AI engine
  (Ollama)` セクションと bundled Ollama のインストールログが**出ない**こと）。

## Refs
- https://github.com/waired-ai/waired-agent/issues/138
- https://github.com/waired-ai/waired/issues/1002 (L11)
- docs/decisions/20260727/2100-delete-local-enrollment.md
