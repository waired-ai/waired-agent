---
title: 遅い・ハードウェアの問題
description: 答えがとても遅い、GPUが使われない、ハードウェアより大きいモデルを選んだ、長いプロンプトでGPUのメモリが足りない、GPUに割り当てるメモリの設定で遅くなった、といった症状の対処です。
meta:
  audience: モデルは答えるが、遅い、またはエラーになる人
  needs: そのパソコンのターミナル
  time: 各対処は1〜2分
---

まず`waired doctor`を実行してください。準備ができていない部分が表示されます。
そのあと、下から症状を探します。

## <a id="answers-are-very-slow"></a>答えがとても遅い

```sh
waired runtimes benchmark
```

このパソコンの実際の速度を計測します。コーディングアシスタントに必要な速度を
下回ると、Wairedはより軽いモデルを提案します。たいていは受け入れるのが正解です。

ほかに確認する価値があるものは次のとおりです。

- **GPUは使われていますか。** [GPUが使われていない](#my-gpu-is-not-being-used)を
  参照してください。
- **モデルはメモリに対して大きすぎませんか。** 大きすぎるモデルは一部が
  プロセッサで動き、はるかに遅くなります。`waired models ls --detail`で収まり
  具合が分かります。
- **AMD Ryzen AI Maxのパソコンで、グラフィックスに割り当てたメモリはどれくらい
  ですか。** 多く割り当てると遅くなります。
  [Windows：グラフィックスにメモリを多く割り当てたら遅くなった](#windows-giving-the-graphics-chip-more-memory-made-things-worse)を
  参照してください。
- **答えは別のパソコンから来ていませんか。** `waired infer --explain "hi"`が
  答えたパソコンの名前を表示します。
- **Claude Codeのセッションの最初のターンではありませんか。** 最初のターンが
  いちばん重い処理です。会話全体、指示、ファイルの内容を読み終えるまで1語も
  返ってこず、ノートパソコンや古いGPUでは数分かかることがあります。同じ
  セッションの後続のターンははるかに速くなります。
  [遅いパソコンは失敗ではありません](/ja/guides/claude-code/how-turns-are-routed/#a-slow-computer-is-not-a-failure)を
  参照してください。

## <a id="my-gpu-is-not-being-used"></a>GPUが使われていない

まず、Wairedが見つけたものを確認します。

```sh
waired models ls --detail
```

最初の行にGPUの名前とメモリが表示されます。GPUのあるパソコンで`no GPU`と出る
場合、GPUは検出されておらず、割り当てられたモデルを含むそれ以降のすべてが
プロセッサ向けに決められています。

一般的な場合はWairedが自動で対処します。AMDとIntelの内蔵グラフィックスはVulkanで
有効になり、AMDの単体GPUは対応環境ではROCmを使い、動かない場合はVulkanに
切り替わります。

NVIDIAのGPUは、`PATH`上の`nvidia-smi`を探すのではなく、ドライバー自体から検出
します。GPUが見つからない場合は、Wairedにツールの場所を直接指定してサービスを
再起動します。

Linuxでは、`sudo systemctl edit waired-agent`を実行して次を追加します。

```ini
[Service]
Environment=WAIRED_NVIDIA_SMI=/usr/bin/nvidia-smi
```

Windowsでは、管理者のPowerShellで次のように実行します。

```powershell
[Environment]::SetEnvironmentVariable(
  'WAIRED_NVIDIA_SMI', 'C:\Windows\System32\nvidia-smi.exe', 'Machine')
```

そのあとサービスを再起動し、`waired models ls --detail`をもう一度実行します。
Windowsでは、パソコン全体の新しい環境変数をサービスに確実に読ませるには再起動が
いちばん確実です。再起動のコマンドは
[コマンドが「waired-agent is not running」と言う](/ja/troubleshooting/no-answer/#a-command-says-waired-agent-is-not-running)を
参照してください。

モデルが収まることも確認してください。必要なメモリは
[モデルカタログ](/ja/reference/model-catalog/)にあります。

## <a id="i-chose-a-model-bigger-than-my-hardware"></a>ハードウェアより大きいモデルを選んだ

Wairedは警告しますが、止めはしません。大きすぎるモデルを選ぶと、
`needs 32 GB RAM (have 31 GB)`のように不足分を示して確認します。

- **少し超える程度**：たいてい動きますが、遅くなります。
- **大きすぎる**：推論エンジンが読み込みに失敗し、エラーを報告します。小さい
  モデルに戻します。[モデルを変更する](/ja/guides/choose-a-model/)を参照して
  ください。

推奨の数値には安全余裕があります。AppleシリコンとAMD Strix Haloでは、グラフィックス
側が扱えるメモリで収まりを判定します。単体のGPUを持つパソコンでは、Wairedが
自動で選ぶモデルはGPU自身のメモリで判定されるので、システムRAMにあふれないと
収まらないモデルは自分で意図的に選ぶことになります。`waired models ls --detail`は、
このパソコンでのすべてのモデルの判定を表示します。

## <a id="it-says-the-gpu-ran-out-of-memory-on-a-long-prompt"></a>長いプロンプトでGPUのメモリが足りなくなったと表示される

これはセットアップ中ではなく、使っている途中で分かります。長い会話の途中で
ターンが失敗し、そのあと`waired models ls --detail`のモデルの行が`! running here
with a warning`となり、表の下に推論エンジン自身の文が表示されます。その文は
`this computer's GPU ran out of memory serving a request at this model and window`で
始まります。

これは「遅い」とは別の問題です。短いプロンプトは動きます。会話が長くなるとVRAMが
足りなくなり、コーディングのセッションはすぐに長くなります。

Wairedが自分から何かを変えることはありません。推論エンジンは動き続け、次の短い
リクエストは動き、Wairedは目に付く場所に警告を残します。`waired models ls
--detail`、`waired status`、`waired doctor`です。

必要な長さに対して、モデルがこのパソコンには大きすぎます。軽いモデルに切り替え
ます。[モデルを変更する](/ja/guides/choose-a-model/)を参照してください。この場合、
Wairedは意図的に軽いモデルを自動では提案しません。その提案は、速度が遅いと計測
されたパソコンのためのものです。メモリ不足は別の問題で、同じ会話の長さなら小さい
モデルが必ず解決策になるわけではありません。

## <a id="windows-giving-the-graphics-chip-more-memory-made-things-worse"></a>Windows：グラフィックスにメモリを多く割り当てたら遅くなった

AMD Ryzen AI Max（Strix Halo）のパソコンでは、グラフィックス側とプロセッサが
1つのメモリを共有し、そのうちどれだけを最初からグラフィックス側に渡すかを設定で
決めます。増やせば大きいモデルが動くように見えますが、逆の結果になります。

Windowsは、グラフィックスへの割り当てと同じ量のシステムRAMを裏で確保します。
そのため、モデルにはグラフィックス側の領域と、Windowsがまだ見ているメモリの中に
同じ量がもう一度必要になります。128GBのマシンで96GBをグラフィックス側に渡すと、
Windowsには約31GBしか残らず、それがモデルの大きさの実際の上限になります。大きい
モデルは読み込みを始めてメモリを使い切り、答えを返さないまま何十分もディスクに
書き出し続けます。

128GBのRyzen AI Max+ 395で76GBのモデル1つを使い、この設定だけを変えて計測した
結果は次のとおりです。

| GPUに割り当てたメモリ | 結果 |
|---|---|
| 96GB | 読み込みが終わらない。28分たっても答えなし |
| 512MB | 15秒で読み込み、その後は全速で動作 |

割り当てを減らしても失うものはありません。グラフィックス側はどのみち残りの
メモリに届き、この種のパソコンではどちらも同じ物理メモリで同じ速度です。

つまり、小さく設定します。BIOSではVRAMのサイズを［Auto］のままにします。通常は
［UMA Frame Buffer Size］という名前です。次に、AMD Software: Adrenalin Editionで
［Performance］、［Tuning］、［System］、［Variable Graphics Memory］の順に開き、
いちばん小さい選択肢を選びます。再起動して、Wairedがどう見ているかを確認します。

```sh
waired models ls --detail
```

最初の行に、以前よりはるかに大きい値が表示されるはずです。まだ小さい残りの値が
表示される場合は、ドライバーに任せずBIOSが分割を固定しています。BIOSで［Auto］に
戻してください。
