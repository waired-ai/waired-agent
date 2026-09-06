---
title: ルーティングと共有のコマンド
description: waired share、worker、peers、ping、public、pause、resumeについて、それぞれが変える内容と出力の意味を説明します。
meta:
  audience: ターミナルで作業する人、画面のないパソコンを使う人
  needs: インストール済みのWaired
  time: 必要なコマンドを読むだけ
---

## <a id="waired-share"></a>`waired share`

このパソコンを貸し出すかどうかです。パソコン側にある唯一の共有のスイッチです。

```sh
waired share on
waired share off        # 全員への提供を止める。実行中の処理も打ち切られる
waired share status
```

オフにすると、あらゆる提供がその場で止まります。自分のほかのパソコンは答えてもらえなくなり、アカウント外からこのパソコンを使っている人は切断され、その時点で実行中のリクエストは完了しません。このパソコン自身での利用には影響ありません。Webコンソールからこのスイッチをオンに戻すことはできません。戻せるのは、このコマンドか、Wairedアプリの［Share this computer］だけです。

スイッチがオンの間に誰に提供するかは、[Webコンソール](/ja/guides/web-console/)で設定します。`status`は全体像を表示します。

```text
Sharing this computer: on
Your other computers: on
People outside your account: off
Who this computer is shared with is set in the Waired console.
```

最初の行はこのパソコン自身のスイッチです。保存した選択と実際の状態が違うときは、2行目で説明します。たとえば`Paused because the Waired app is not running. It resumes when the app starts.`です。次の2行はコンソールが決めた内容で、サービスがコンソールから受け取るまでは`not known yet`と表示されます。ゲストの上限を設定していると、`Guest limit: N at once`の行が表示されます。[自分の別のパソコンと共有する](/ja/guides/sharing/)を参照してください。

## <a id="waired-worker"></a>`waired worker`

このパソコンのリクエストの送り先です。

```sh
waired worker get
waired worker set --mode=auto            # このパソコンにモデルがあればそれ、なければ別のパソコン（既定）
waired worker set --mode=local-only      # ほかのパソコンを使わない
waired worker set --mode=peer-preferred  # ほかのパソコンを優先し、なければこのパソコン
waired worker set --mode=peer-only       # ほかのパソコンだけ。なければ失敗させる
waired worker set --pin=<peer>           # 常にこのパソコン（--mode=pinnedを含む）

waired worker set --prefer=speed         # いちばん速く答える（既定）
waired worker set --prefer=size          # いちばん大きいモデルを使う
waired worker set --min-model-size=medium  # それより小さいモデルのパソコンを除外する
waired worker set --min-model-size=""      # 最小値なし（既定）
```

`waired worker get`は設定全体を表示します。

```
mode:           auto
prefer:         speed
smallest model: any
```

`<peer>`はパソコンの名前か、`waired peers list`の`DEVICE-ID`列の識別子です。選ぶのはパソコンであってモデルではありません。どのパソコンが答えても、答えはそのパソコンが動かしているモデルから返ります。固定したパソコンの電源が入っていないか届かない場合、リクエストはほかの場所に行かずに失敗します。各設定の動作は[どのパソコンが答えるかを選ぶ](/ja/guides/routing/)を参照してください。

固定したパソコンが提供していないとき、`waired worker get`は`status:`の行に理由を表示します。

## <a id="waired-peers"></a>`waired peers`

```sh
waired peers list
waired peers list --json
```

自分のほかのパソコンを、アドレス、推論エンジン、GPU、モデルとともに表示します。`worker set --pin`に渡す名前はここで調べます。同じ名前を報告するパソコンが2台あると、2台目に番号が付きます。

**MODEL**はそのパソコンが動かしているモデルです。そのとなりの**MODELS**は、同じモデルを推論エンジンが使う名前で表したものです。**WORKER-CAPABLE**は各パソコンの自己申告で、いま答えられるかどうかと、答えられない場合はその理由です。たとえば`no (downloading)`や`no (engine not answering)`です。この申告はパソコンどうしのプライベートネットワークではなくアカウント経由で届くので、`yes`は主張であって、このパソコンが確認したものではありません。`no (stale)`は、そのパソコンからの報告が途絶えたことを意味します。電源の入っていないパソコンも、ネットワークから外すまで行は残ります。

一覧のどれかのパソコンからこのパソコンへの応答がない場合、表の下にそのことが表示されます。

```
This computer has had no reply from: office-desktop.
WORKER-CAPABLE is what each computer reports about itself, not something this
computer checked. Run `waired doctor` to measure this computer's connection.
```

1つの原因は名指しで示されます。直すまでほかの何も動かないからです。

```
This computer's key does not match the one your network has for it, so no other
computer can reach it. Run `waired init` to register this device again.
```

## <a id="waired-ping"></a>`waired ping`

```sh
waired ping <peer>
```

このパソコンからプライベートネットワーク経由で別のパソコンに届くことを確認します。相手が答えないときは、エラーに相手の名前が示されます。

## <a id="waired-public"></a>`waired public`

自分の空き容量をほかのWairedユーザーに貸し、相手の容量を借りる機能です。オンにしないかぎりオフです。先に[パブリック共有](/ja/public-share/)を読んでください。

このパソコンの公開共有のオンとオフは、ここではなく[Webコンソール](/ja/guides/web-console/)で切り替えます。`waired public status`はその状態と、他人のパソコンを使うための自分の設定を表示します。

```sh
waired public status
waired public use                      # 現在の設定を表示する
waired public use --auto               # 自分のパソコンより良いときに他人のパソコンを使う
waired public use --explicit           # 自分が明示的に指示したときだけ
waired public use --off
waired public use --min-model-size small|medium|large   # このサイズ以上のモデルを動かすパソコンだけ
waired public use --main on|off --sub on|off            # メインの会話とサブエージェントについて公開ノードを許可または禁止する
```

初めて`use`を有効にするとき、ターミナルに一度だけプライバシーの警告が表示され、読んで受け入れる必要があります。

`waired public status`は共有側から始まります。`Sharing this computer publicly: on|off`、次に`Guest limit: N at once`または`Guest limit: automatic`、そして公開共有のオンとオフはWebコンソールで切り替えるという注意です。このパソコン自身のスイッチがオフのときは、何も共有されていない理由の行が加わります。

```text
Sharing is off on this computer, so nothing is shared. Turn it back on with `waired share on`.
```

## <a id="waired-pause-and-resume"></a>`waired pause`と`resume`

```sh
waired pause
waired resume
```

一時停止は、このパソコンのすべてのルーティングを止めます。答えるのをやめ、Wairedに送ったターンは、自分のほかのパソコンが受けないかぎり失敗します。Wairedアプリの［Pause Waired］は同じスイッチです。設定は再起動後も保持されます。バックグラウンドサービスが動いていないときは、選択が保存されて次の起動時に適用されます。

```
waired-agent not running — pause persisted; will apply on next start.
```

「止める」が意味する4つの違いは、[Wairedを一時停止する](/ja/guides/pause/)を参照してください。
