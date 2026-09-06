---
title: トラブルシューティング
description: いま見えている症状を普通の言葉で探し、その説明と直すコマンドが載っているページに進みます。
meta:
  audience: Wairedの動作がおかしい人
  needs: そのパソコンのターミナル
  time: 症状を探す時間。各対処は1〜2分
---

<!-- 症状から引く構成。読者は何が見えているかは知っていても、どの部品の問題かは
     知らないので、索引は読者の言葉で書く。各項目は /troubleshooting/ 配下の6ページの
     見出し1つに対応する。 -->

## <a id="start-here"></a>まず実行すること

```sh
waired doctor
```

セットアップの各項目を検査して✓、⚠、✗で表示し、`f`キーを押すと直せるものを修復します。このページのほかの項目より先に実行してください。たいていの問題はこれで解決します。各検査項目の意味は[診断を実行する](/ja/getting-started/doctor/)を参照してください。

## <a id="find-your-symptom"></a>症状から探す

### <a id="installing-and-signing-in"></a>インストールとサインイン

[インストールとサインインの問題](/ja/troubleshooting/install-and-sign-in/)

- [`waired`と入力すると「command not found」になる](/ja/troubleshooting/install-and-sign-in/#i-typed-waired-and-got-command-not-found)
- [サインイン時にブラウザが開かない、または別のブラウザが開く](/ja/troubleshooting/install-and-sign-in/#no-browser-opened-at-sign-in-or-the-wrong-one-did)
- [サインインを終える前にリンクが失効した](/ja/troubleshooting/install-and-sign-in/#the-sign-in-link-expired-before-i-finished)
- [バックグラウンドサービスが応答しないためサインインが止まる](/ja/troubleshooting/install-and-sign-in/#sign-in-stops-because-the-background-service-is-not-responding)
- [サインインしたのに、Wairedはサインアウトしていると言う](/ja/troubleshooting/install-and-sign-in/#i-signed-in-but-waired-says-i-am-signed-out)
- [デバイスの上限に達したと表示される](/ja/troubleshooting/install-and-sign-in/#it-says-i-have-reached-the-device-limit)
- [「signed in system-wide」と表示される](/ja/troubleshooting/install-and-sign-in/#it-says-the-computer-is-signed-in-system-wide)

### <a id="setting-up"></a>セットアップ

[セットアップの問題](/ja/troubleshooting/setup/)

- [セットアップが途中で止まった](/ja/troubleshooting/setup/#setup-stopped-partway)
- [セットアップが推論エンジンを起動できなかったと言う](/ja/troubleshooting/setup/#setup-says-the-inference-engine-failed-to-start)
- [セットアップが選んだモデルをダウンロードできないと言う](/ja/troubleshooting/setup/#setup-says-it-cannot-download-the-model-you-chose)
- [セットアップがテストの生成を完了できなかったと言う](/ja/troubleshooting/setup/#setup-said-it-could-not-complete-a-test-generation)
- [Wairedがとても小さいモデルを選んだ](/ja/troubleshooting/setup/#waired-chose-a-very-small-model-for-my-computer)
- [選んでいないのにローカル推論がオフで始まった](/ja/troubleshooting/setup/#local-inference-started-off-and-i-did-not-choose-that)
- [ローカル推論がまだセットアップされていないと表示される](/ja/troubleshooting/setup/#it-says-local-inference-is-not-set-up-yet)
- [このパソコンに推論エンジンがない](/ja/troubleshooting/setup/#this-computer-has-no-inference-engine)
- [モデルが新しい推論エンジンを必要としていると表示される](/ja/troubleshooting/setup/#a-model-says-it-needs-a-newer-inference-engine)

### <a id="nothing-answers"></a>応答が返らない

[応答が返らない](/ja/troubleshooting/no-answer/)

- [答えが返ってこない、または推論エンジンが「not ready」のまま](/ja/troubleshooting/no-answer/#no-answer-comes-back)
- [Wairedアイコンがエージェントが動いていないと言う](/ja/troubleshooting/no-answer/#the-waired-icon-says-the-agent-is-not-running)
- [コマンドが「waired-agent is not running」と言う](/ja/troubleshooting/no-answer/#a-command-says-waired-agent-is-not-running)
- [macOS：バックグラウンドサービスが一度も起動しない](/ja/troubleshooting/no-answer/#macos-the-background-service-never-starts)
- [Windows：502エラーになる](/ja/troubleshooting/no-answer/#windows-i-get-a-502-error)

### <a id="claude-code"></a>Claude Code

[Claude Codeの問題](/ja/troubleshooting/claude-code/)

- [Claude Codeがクラウドを使ったまま](/ja/troubleshooting/claude-code/#claude-code-is-still-using-the-cloud)
- [WairedがClaude Codeは組織が管理していると言う](/ja/troubleshooting/claude-code/#waired-says-claude-code-is-managed-by-your-organization)
- [Claude CodeがWairedは答えられないと言う](/ja/troubleshooting/claude-code/#claude-code-says-waired-cannot-answer)
- [/modelにWairedの行がない](/ja/troubleshooting/claude-code/#the-waired-rows-are-missing-from-model)
- [Claude Codeの長いセッションが要約される](/ja/troubleshooting/claude-code/#long-claude-code-sessions-get-summarized)
- [Claude Codeにステータス行が表示されない](/ja/troubleshooting/claude-code/#the-status-line-does-not-show-up-in-claude-code)

### <a id="answers-are-slow-or-the-hardware-is-not-used"></a>答えが遅い、ハードウェアが使われない

[遅い・ハードウェアの問題](/ja/troubleshooting/slow-or-wrong/)

- [答えがとても遅い](/ja/troubleshooting/slow-or-wrong/#answers-are-very-slow)
- [GPUが使われていない](/ja/troubleshooting/slow-or-wrong/#my-gpu-is-not-being-used)
- [ハードウェアより大きいモデルを選んだ](/ja/troubleshooting/slow-or-wrong/#i-chose-a-model-bigger-than-my-hardware)
- [長いプロンプトでGPUのメモリが足りなくなったと表示される](/ja/troubleshooting/slow-or-wrong/#it-says-the-gpu-ran-out-of-memory-on-a-long-prompt)
- [Windows：グラフィックスにメモリを多く割り当てたら遅くなった](/ja/troubleshooting/slow-or-wrong/#windows-giving-the-graphics-chip-more-memory-made-things-worse)

### <a id="other-computers-and-the-app-itself"></a>別のパソコンとアプリ

[別のパソコンとアプリの問題](/ja/troubleshooting/other-computers/)

- [別のパソコンがモデルに届かない](/ja/troubleshooting/other-computers/#my-other-computer-cannot-reach-the-model)
- [パソコンを固定したあとリクエストが失敗する](/ja/troubleshooting/other-computers/#requests-stopped-working-after-i-pinned-a-computer)
- [LinuxでWairedのアイコンが表示されない](/ja/troubleshooting/other-computers/#the-waired-icon-is-missing-on-linux)
- [ログを読む](/ja/troubleshooting/other-computers/#reading-the-logs)

## <a id="still-stuck"></a>それでも直らないとき

[不具合を報告する](/ja/getting-started/report-a-problem/)に従ってください。問題を再現する前に詳細なログをオンにし、1つのファイルに集めて添付します。`waired logs --mask-pii`は、ホームディレクトリ、ユーザー名、ホスト名、アカウントのメールアドレスを伏せるので、そのファイルは[issue](https://github.com/waired-ai/waired-agent/issues)に安全に添付できます。
