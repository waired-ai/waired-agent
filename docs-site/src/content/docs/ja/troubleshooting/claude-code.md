---
title: Claude Codeの問題
description: Claude Codeがクラウドのまま、組織の管理下にある、Wairedは答えられないと言う、/modelにWairedの行がない、長いセッションが要約される、ステータス行が出ない、といった症状の対処です。
meta:
  audience: セッションが想定どおりに動かないClaude Codeのユーザー
  needs: そのパソコンのターミナル
  time: 各対処は1〜2分
---

まず`waired doctor`を実行し、`f`キーを押して見つかったものを修復してください。
次に`waired claude status`を実行すると、接続の各部分の状態が表示されます。その
あと、下から症状を探します。

## <a id="claude-code-is-still-using-the-cloud"></a>Claude Codeがクラウドを使ったまま

まずフッターを読みます。`→ waired: Anthropic`は、このセッションがAnthropicの
モデルにあることを意味します。まだ操作していないセッションはそうなります。Claude
Codeの既定がAnthropicのモデルで、Wairedはそれを変えないからです。セットアップ後の
通常の状態であり、不具合ではありません。`/model`と入力して［Waired］の行を選択
します。次のターンは自分のパソコンで実行され、フッターは`⚡ waired: on Waired`に
変わります。

`/model`にWairedの行がない場合は、[/modelにWairedの行がない](#the-waired-rows-are-missing-from-model)を
参照してください。`waired claude status`が連携は有効でないと言う場合は、有効にして
Claude Codeのセッションを再起動します。

```sh
sudo waired claude enable     # Windowsでは管理者のターミナルで
```

その結果、このパソコンのClaude Codeは組織が管理していると表示される場合は、
[次の項目](#waired-says-claude-code-is-managed-by-your-organization)を参照して
ください。

`waired claude status`は、新しいセッションがどのモデルで始まるか（`default
model:`）と、直前のターンがどうなったかを表示します。

```
last request:       claude-opus-5 → the real Anthropic API   (2 minutes ago)
```

ターンがクラウドに送られるのは、そのモデルがAnthropicのモデルのときだけです。
Wairedが自分からそこへ送ることはないので、`last request:`にAnthropic APIが出て
いれば、常にセッションのモデルがそうさせたことになります。

## <a id="waired-says-claude-code-is-managed-by-your-organization"></a>WairedがClaude Codeは組織が管理していると言う

`sudo waired claude enable`が、ルーティングをオンにする代わりに次のように止まり
ます。

```
Claude Code on this computer is managed by your organisation, so Waired did not
change its settings. Found in /etc/claude-code/managed-settings.json:
  availableModels
  forceLoginMethod = console

Pointing ANTHROPIC_BASE_URL at Waired would also switch off the settings your
organisation delivers to every session on this computer, which is not Waired's
call to make. Ask whoever manages this computer, or use Waired from another
coding tool — `waired link` sets those up per user and touches nothing
machine-wide.
```

これは多くの場合、職場のパソコンです。Claude Codeはパソコン全体の設定ファイルを
読み、パソコンの管理者がそこにClaude Codeの組織の設定を置いていると、Wairedは
それを読んで止まります。`Found in`の下の行が見つかったもので、どれか1つあれば
十分です。強制ログイン（`forceLoginOrgUUID`、`forceLoginMethod`、
`forceLoginGatewayUrl`）、許可モデルの一覧（`availableModels`）、`/model`の
行構成（`modelPicker`）、またはWaired以外を指す`ANTHROPIC_BASE_URL`です。

止まる理由は、Claude CodeをWaired経由にするには同じファイルに
`ANTHROPIC_BASE_URL`を書き込む必要があり、それによって組織がパソコン上のすべての
セッションに配っている設定が無効になるからです。それを受け入れるかどうかは
パソコンの管理者が決めることなので、Wairedは決めず、それでも書き込むオプションは
ありません。

できることは次のとおりです。

- パソコンの管理者に相談します。メッセージにファイルと該当する設定が示されて
  います。
- 同じパソコンで、ほかのコーディングツールからWairedを使います。Claude Codeの
  パソコン全体のリダイレクト以外はすべて動きます。
  [OpenCodeから使う](/ja/guides/opencode/)と[OpenClawから使う](/ja/guides/openclaw/)を
  参照してください。

`waired init`のルーティングの手順も同じ場所で止まり、同じメッセージを表示します。
残りのセットアップは完了し、Claude CodeはAnthropic APIと直接通信し続けます。

## <a id="claude-code-says-waired-cannot-answer"></a>Claude CodeがWairedは答えられないと言う

自分のパソコンのどれも処理できないWairedの行のターンは、Claude Codeの中で
`API Error: 400`と、何が答えられなかったかを示すメッセージですぐに失敗します。
Anthropic APIには送られません。これらのメッセージはすべて同じ文で終わります。
``Pick an Anthropic model in /model to send this turn to the cloud, or run
`waired doctor` to see what is missing.``これが2つの選択肢です。メッセージの
冒頭が、どちらの対処が当てはまるかを示します。

| メッセージの冒頭 | 意味 | 対処 |
|---|---|---|
| `Waired is not set up to answer on this computer, so this turn has nowhere to run.` | ここに推論エンジンがなく、自分のほかのパソコンにも届いていません。 | このパソコンで`waired doctor`を実行します。ここで推論エンジンを始めるか、モデルを動かすパソコンの電源を入れます。 |
| `The computer this turn is pinned to, <name>, is not answering.` | `waired worker`で固定したパソコンが、オフか、スリープ中か、共有していません。 | [パソコンを固定したあとリクエストが失敗する](/ja/troubleshooting/other-computers/#requests-stopped-working-after-i-pinned-a-computer)を参照してください。 |
| `The peer <name> stopped answering after <time>.`または`The peer <name> stopped working on this request after <time>.` | 前者は、そのパソコンが答えている途中で応答が途絶えました。後者は、停止を報告したか、推論エンジンは動いているのに答えなくなりました。 | `waired peers list`で確認し、そのパソコンで`waired doctor`を実行します。 |
| ``No computer on Waired runs a medium model or larger. Change the floor with `waired worker set --min-model-size`.`` | 自分で設定した最小のモデルサイズが、このパソコンを含むすべてのパソコンを除外しました。 | 最小値を下げるか解除します。[最小のモデルサイズを決める](/ja/guides/routing/#set-a-smallest-model)を参照してください。 |
| `Waired public share declined this turn:`のあとに自分の設定 | 自分のパブリック共有の設定が断りました。 | メッセージにコマンドが示されます。`waired public status`でこれらの設定を一度に確認でき、`waired public use`で変更します。 |
| `Waired public share declined this turn:`のあとに`no public machine is reachable right now`または`Public Share is set to use another machine only when it beats this one, and none does` | いま使える公開のマシンを誰も貸していないか、自分のパソコンより良いものがありません。どちらも不具合ではありません。 | 待つか、`/model`で別の行を選びます。後者が当てはまらないようにするには、`waired public use --explicit`を実行します。 |

多くの場合、フッターが先にそれを伝えます。赤い`⚠ waired: Waired cannot answer
(local disabled, no peer)`は、自分のパソコンのどれも次のターンを受けられないことを
Wairedがすでに把握していることを意味します。括弧内はこのパソコンの推論エンジンの
状態（`local disabled`、`local no_engine`など）と、ほかのパソコンに届かないときの
`no peer`です。

## <a id="the-waired-rows-are-missing-from-model"></a>/modelにWairedの行がない

`/model`には、Anthropicのモデルの下に［Waired］、［Waired local］、［Waired
peer］が、パブリック共有がオンなら［Waired public share］も表示されるはずです。
隠れる原因は4つあり、確認する順に並べます。

1. **Claude Codeを再起動していない。** 行はClaude Codeの起動時に読まれます。動作中の
   セッションで`/model`を開き直しても読み直されません。Claude Codeを終了して
   起動し直します。
2. **このパソコンでルーティングがオンになっていない。** `waired claude status`で
   確認します。行が表示されるのは、Claude CodeがWairedに向いてからです。

   ```sh
   sudo waired claude enable    # Windowsでは管理者のターミナルで
   ```

3. **行が別のユーザー向けに書かれた。** 行は自分の`~/.claude/settings.json`
   （Windowsでは`%USERPROFILE%\.claude\settings.json`）の`modelPicker`にあるので、
   `root`としてWairedを設定したインストールでは、自分のClaude Codeが見ない場所に
   書かれます。`waired claude status`が確認したファイルを表示します。

   ```
   /model rows:        not written — /home/you/.claude/settings.json
                       run `waired claude enable` as the user who runs `claude`
   ```

   行がある場合、同じ行に行数とファイルが表示されます。

4. **そのファイルにすでに自分の`/model`の行がある。** Claude Codeは`modelPicker`の
   一覧全体を1か所から読み、2つを合成しないので、自分の`~/.claude/settings.json`に
   すでに行があると、Wairedはそれに触れず何も書きません。

   ```
   /model rows:        LEFT ALONE — /home/you/.claude/settings.json already lists its own rows
   ```

   同じ行の`UNREADABLE`は、ファイルがWairedの読めるJSONではないことを意味します。
   直したら`waired claude enable`をもう一度実行します。

WindowsにWairedをインストールしてWSL2の中でClaude Codeを動かすのは別の話です。
2つは別のシステムなので、Windows側のClaude Codeを使ってください。

行が戻るまで選ぶものがないので、Claude Codeの既定のセッションはAnthropic APIの
ままで、フッターには`→ waired: Anthropic`と表示されます。

## <a id="long-claude-code-sessions-get-summarized"></a>Claude Codeの長いセッションが要約される

これは想定どおりの正常な動作です。ローカルのモデルはクラウドのモデルより一度に
保持できる会話が短いので、WairedはClaude Codeに実際の上限を伝え、Claude Codeは
収まるように古いターンを要約します。セッションは、冒頭を黙って失うのではなく
動き続けます。一時的に「Prompt is too long」と表示されても、Claude Codeは自動で
回復します。

想定よりかなり早く、または遅く要約される場合は、モデルを切り替えたあとにClaude
Codeへ伝えた上限が古くなっている可能性があります。

```sh
waired claude status
```

**local window**の行に、いまのモデルが扱える上限と、Claude Codeの起動時に伝えた
上限が並びます。食い違っていれば、`sudo waired claude enable`をもう一度実行し
（Windowsでは管理者のターミナルで）、Claude Codeを再起動します。詳しくは
[長いセッションは要約されます](/ja/guides/claude-code/how-turns-are-routed/#long-sessions-get-compacted)を
参照してください。

## <a id="the-status-line-does-not-show-up-in-claude-code"></a>Claude Codeにステータス行が表示されない

プロジェクトのディレクトリの中で`waired claude status`を実行します。Claude Codeの
ステータス行は1つだけで、プロジェクトの設定（`.claude/settings.json`または
`.claude/settings.local.json`）が、Wairedがユーザー向けにインストールしたものより
優先されます。その場合、コマンドは優先されているファイルの名前を表示し、自分の
ステータス行のスクリプトに加えられる行を示します。

連携を有効にしたあとにClaude Codeのセッションを再起動したことも確認してください。

Windowsで、`waired`自体が動かなくなると同時にフッターが空白になった場合は別の
問題です。Windowsのアプリケーション制御が`waired.exe`の実行を拒否しています。
[インストール後に起きた場合](/ja/getting-started/install/windows/#if-it-happens-after-waired-is-installed)を
参照してください。ステータス行のすべての形は、
[Wairedのステータス行](/ja/guides/claude-code/status-line/)を参照してください。
