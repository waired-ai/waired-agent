---
status: accepted
---

# local enrollment の実装を削除する (20260727 21:00)

## Status
Accepted

## Context

#175 の最後の一手。PR-1 (#271) が暗黙のフォールバックを、PR-2 (#290/#291) が
`--google-sa-login` / `--bypass-mode` を、PR-3a
(docs/decisions/20260727/2030-daemon-owns-reauthentication.md) が再認証を
daemon に移した結果、`waired init` プロセス自身が enrollment を行う経路は
**到達不能**になっていた。

## Decision

到達不能になったコードを消す。約 3,300 行:

- `cmd/waired/main.go` の `runInitBody` 後半（`switch route` 以降）
- `internal/setup/init.go`（`Init` / `InitOptions` / `InitResult`）と
  `deploy.go` の `Deploy` 系。残った Ollama 検出だけを
  `ollama_detect.go` に改名して残す
- CLI 側の局所ヘルパー: `chooseListenAddr` / `printInitSuccessBox` /
  `offerBenchmark` / `ensureBundledEngine` / `promptInference` /
  `applyBundledModelSelection` / `initStepLabels` ほか
- `waired init` のフラグ 7 本:
  `--listen` / `--endpoint` / `--skip-deploy` / `--start-agent` /
  `--no-wait-model` / `--ollama-source` / `--reset-config`

**残すもの**: `setup.Enroll`（daemon の `enrollFunc` が使う）、
`setup.Integration`（`link` / `doctor`）、`confirmRenew`、
`handStateToServiceUser`（`runtimes install` にも呼び出し元がある）、
`benchmarkWithScanner` / `waitForBundledModel`（daemon 経路が使う）。

**`--skip-claude-route` は消さない**: `install.ps1` が実際に渡しているので、
削除するとインストールが `flag provided but not defined` で落ちる。

## Consequences

- daemon が動いていないホストでは `waired init` が enrollment を完了できない。
  #175 の設計判断そのもの（PR-1 で決定）。
- 削除によって、既に到達不能だった 2 つの欠落が恒久化した。どちらもこの PR が
  作ったものではなく、`routeDaemon` が既定になった時点から起きていた:
  - #294 — daemon 経由の init は Claude Code のルーティングを一切書かない。
    インストーラは自前の `waired claude enable` を外して init に委ねているので、
    CLI インストールはルーティングされないまま終わる。
  - #295 — GNOME AppIndicator 拡張の自動導入がなくなる。
    `waired-tray` パッケージの Suggests だけが残る。
- `internal/setup` は「daemon と CLI の共有部品」だけになった。パッケージ doc を
  それに合わせて書き直した。

## Refs
- https://github.com/waired-ai/waired-agent/issues/175
- https://github.com/waired-ai/waired-agent/issues/294
- https://github.com/waired-ai/waired-agent/issues/295
- docs/decisions/20260727/2030-daemon-owns-reauthentication.md
