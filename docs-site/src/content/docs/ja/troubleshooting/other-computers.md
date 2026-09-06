---
title: 別のパソコンとアプリの問題
description: 別のパソコンがモデルに届かない、パソコンを固定したあとリクエストが失敗する、LinuxでWairedのアイコンが表示されない、といった症状の対処と、ログの場所です。
meta:
  audience: パソコンが2台以上ある人、アイコンが表示されない人
  needs: そのパソコンのターミナル
  time: 各対処は1〜2分
---

まず`waired doctor`を実行してください。準備ができていない部分が表示されます。
そのあと、下から症状を探します。

## <a id="my-other-computer-cannot-reach-the-model"></a>別のパソコンがモデルに届かない

```sh
waired status --observability
```

**Mesh**の行は`enrolled / reachable / ready`の形です。`reachable`が0の場合は
次を確認します。

1. **2台とも同じGoogleアカウントでサインインしていますか。** 群を抜いて多い
   原因です。それぞれの`waired status`のアカウントの行を比べます。
2. **相手のパソコンは起動していて、Wairedは動いていますか。** そのパソコンで
   `waired doctor`を実行します。
3. **共有していますか。** パソコンがほかのパソコンに答えるのは、自分の共有の
   スイッチがオンで（`waired share status`で確認、`waired share on`でオン）、かつ
   Webコンソールの［Sharing］のカードで［Your other computers］に提供している
   ときだけです。[自分の別のパソコンと共有する](/ja/guides/sharing/)を参照して
   ください。

届いてはいるが`ready`にならない場合、そのパソコンにはモデルが読み込まれて
いません。そのパソコンで
[答えが返ってこない](/ja/troubleshooting/no-answer/#no-answer-comes-back)の手順を
確認します。

すべて届いているように見えるのにリクエストが届かない場合は、`waired doctor`を
実行します。**mesh peers**の行は、ネットワークの申告を鵜呑みにせず、各パソコンに
実際のリクエストを送って結果を報告します。

```
⚠ mesh peers — 2/3 reported reachable, but only 0 answered an overlay ping —
  no reply from mac-mini, work-laptop. Inference cannot route to a peer that
  does not answer; check NAT traversal and relay connectivity
```

この行は、2台のパソコンが接続済みと表示されているのに、実際には何も届いていない
ことを意味します。名前の挙がったパソコンで、上の3つの確認を行います。

ポートの開放やVPNの設定は必要ないはずです。パソコンどうしは、ネットワークが
許せば直接接続し、ファイアウォールに阻まれる場合は暗号化されたリレーに切り替わり
ます。

## <a id="requests-stopped-working-after-i-pinned-a-computer"></a>パソコンを固定したあとリクエストが失敗する

```sh
waired worker get
```

固定は厳密な指示です。そのパソコンを使い、ほかは使いません。そのため、固定した
パソコンがスリープ中、オフライン、または共有していない場合、Wairedはほかの場所で
処理を実行せず、エラーを返します。これは意図的です。黙って別のパソコンが答えると、
大きなGPUマシンに送ったはずのリクエストが実は目の前のノートパソコンで処理されて
いて、それを知る手がかりがない、ということになるからです。

Wairedアプリも同じことを表示します。［Worker: `<name>` (pinned) — unavailable,
requests are not served here］です。Claude Codeにも同じ答えが返ります。ターンは
すぐに失敗し、パソコンの名前を表示します。

```
API Error: 400 The computer this turn is pinned to, sv-mag, is not answering. Pick an Anthropic model in /model to send this turn to the cloud, or run `waired doctor` to see what is missing.
```

直すには、固定したパソコンを起動する（`waired peers list`と、そのパソコンでの
`waired doctor`で確認）か、固定をやめます。

```sh
waired worker set --mode=auto
```

固定したパソコンが戻ってもターンが同じメッセージで失敗する場合は、1分ほど待ち
ます。そのパソコンのWairedのバックグラウンドサービスが再起動すると、ほかの
パソコンから処理を受ける前に、自分のアカウントに改めて名乗る必要があります。
そのパソコンで直すものはありません。

## <a id="the-waired-icon-is-missing-on-linux"></a>LinuxでWairedのアイコンが表示されない

GNOMEは、そのままでは時計のとなりにアイコンを表示しません。Wairedのアイコンには
AppIndicator拡張機能が必要です。セットアップは、パソコンにGNOMEがあると拡張機能を
インストールし、以後もログインのたびに確認します。拡張機能はあるが無効になって
いる場合は、再び有効にします。

それでもアイコンが表示されない場合は、次のコマンドで直ります。

```sh
waired doctor --fix
```

問題を報告し、変更の前に確認し、必要に応じて拡張機能をインストールまたは有効に
します。手動で同じことをするには次のように実行します。

```sh
sudo apt install gnome-shell-extension-appindicator
gnome-extensions enable appindicatorsupport@rgcjonas.gmail.com
```

そのあとログアウトして再ログインします。Waylandでは必須です。KDE Plasmaでは
何も必要ありません。MATEではアイコンを表示できません。

## <a id="reading-the-logs"></a>ログを読む

`waired doctor`のあとに読みます。`waired logs`は以下のすべてを1つのファイルに
集めます。[不具合を報告する](/ja/getting-started/report-a-problem/)を参照して
ください。

| | |
|---|---|
| Linux | `journalctl -u waired-agent -e` |
| macOS | `/Library/Logs/waired-agent.err.log`、または`sudo log show --predicate 'process == "waired-agent"' --last 10m`。Wairedはこのファイルを32MBに抑え、以前のものを`waired-agent.err.log.0.gz`、`.1.gz`のように10世代残します。`debug`では上限が128MBになります。 |
| Windows | Wairedの状態フォルダの`logs\waired-agent.log`。通常のサービスインストールでは`C:\ProgramData\waired\logs\…`で、読むには管理者のPowerShellが必要です。上限はmacOSと同じです。`Get-WinEvent -ProviderName waired-agent -LogName Application -MaxEvents 50`は警告とエラーだけの短い版です。 |
| 推論エンジン | Wairedの状態フォルダの`…/runtimes/ollama/logs/engine.log`。Linuxでは`/var/lib/waired/…`、macOSでは`/Library/Application Support/waired/…`、Windowsでは`C:\ProgramData\waired\…`です。 |
