---
title: よくある質問
description: Wairedのインストール前によく聞かれる質問と、インストール後によく聞かれる質問への短い回答です。
meta:
  audience: 導入を検討している人、セットアップを終えたばかりの人
  needs: なし
  time: 斜め読み
---

<!-- 機能別ではなく、疑問が浮かぶ順（検討、ハードウェア、プライバシー、運用）で
     分類。見出しは読者が検索に入力する形の質問文。質問形の見出しはこのページ
     だけの慣行（TRANSLATION.md §Register）。 -->

## <a id="deciding-whether-to-use-it"></a>使うかどうかを決める

### <a id="is-it-hard-to-set-up"></a>セットアップは難しい？

インストールはコマンド1つです。そのあとはブラウザで進めるか、ターミナルでいくつかの質問に答えます。どちらも約10分と、モデルのダウンロード時間がかかります。[クイックスタート](/ja/quickstart/)を参照してください。

### <a id="does-it-cost-money"></a>お金はかかる？

かかりません。サブスクリプションもメッセージごとの課金もありません。モデルはすでに持っているハードウェアで動くので、費用は電気代だけです。

### <a id="do-i-need-a-gpu"></a>GPUは必要？

必須ではありませんが、あると違います。最近のプロセッサなら小さいモデルは実用的な速度で動き、GPUがあると答えは数倍速くなります。各モデルの要件は[モデルカタログ](/ja/reference/model-catalog/)にあります。セットアップが収まるモデルを選ぶので、読む必要はありません。

### <a id="which-tools-work-with-it"></a>使えるツールは？

Claude Code、OpenCode、OpenClawは、それぞれコマンド1つで使えます。OpenAI APIかAnthropic APIを話せるクライアントなら、自分のモデルに接続できます。[チャットアプリから使う](/ja/guides/chat-clients/)を参照してください。

### <a id="is-it-open-source"></a>オープンソースで公開されている？

自分のパソコンで動くもの、つまりクライアントはオープンソースで、[GitHub](https://github.com/waired-ai/waired)で読めます。端末どうしが互いを見つけられるようにするコントロールプレーンは、こちらでホストしています。

## <a id="hardware-and-models"></a>ハードウェアとモデル

### <a id="which-models-can-i-run"></a>使えるモデルは？

Wairedはコーディング向けモデルのカタログを同梱し、そのパソコンで動かせる中から最良のものを選びます。あとから切り替えられます。[モデルを変更する](/ja/guides/choose-a-model/)を参照してください。

### <a id="how-does-waired-choose-a-model-for-me"></a>モデルはどう選ばれる？

プロセッサ、メモリ、GPUを見て、余裕をもって収まる中で最も品質の高いモデルを選びます。単体のGPUを搭載したパソコンでは、GPU自身のメモリに収まることを意味します。そのあと実際の速度を計測し、このパソコンが追いつけない場合はより軽いモデルを提案します。詳しくは[Wairedがモデルを選ぶ仕組み](/ja/guides/how-a-model-is-chosen/)を参照してください。

### <a id="can-i-run-a-model-that-is-bigger-than-recommended"></a>推奨より大きいモデルも動かせる？

動かせます。Wairedは警告して不足分を表示しますが、止めはしません。少し超える程度ならたいてい動き、遅くなるだけです。大きすぎるモデルは読み込みに失敗します。[遅い・ハードウェアの問題](/ja/troubleshooting/slow-or-wrong/)を参照してください。

## <a id="privacy-and-networking"></a>プライバシーとネットワーク

### <a id="is-it-private"></a>プライバシーは守られる？

プロンプトと答えは、自分の端末どうしをエンドツーエンドで暗号化された接続で移動します。Wairedのコントロールプレーンは端末どうしが互いを見つけられるようにするだけで、送った内容を受け取りません。直接接続できないときだけ使われるリレーは、読めない状態のデータをそのまま転送します。[プライバシー：パソコンの外に出るもの](/ja/concepts/privacy/)を参照してください。

### <a id="if-my-model-is-down-does-my-data-go-to-the-cloud"></a>自分のモデルが止まっていたら、クラウドに送られる？

送られません。［Waired］の行で始めたClaude Codeのターンは、自分のパソコンで動くか、その場で理由を表示して失敗するかのどちらかです。Anthropic APIには送られません。クラウドでターンを実行したい場合は、`/model`でAnthropicのモデルを選択します。Anthropicにターンを送るのは、その選択だけです。[Claude Codeのターンはどこで実行されるか](/ja/guides/claude-code/how-turns-are-routed/)を参照してください。

### <a id="do-i-need-to-open-ports-or-set-up-a-vpn"></a>ポート開放やVPNの設定は必要？

不要です。ネットワークが許せばパソコンどうしが直接接続し、ファイアウォールや厳しいNATに阻まれる場合は暗号化されたリレーに切り替わります。どちらも自動です。

### <a id="how-does-signing-in-work"></a>サインインの仕組みは？

Googleでサインインします。同じアカウントでサインインしたパソコンは同じプライベートネットワークに入り、互いに到達できます。ペアリングの操作も、アドレスの入力もありません。

### <a id="can-i-use-it-offline"></a>オフラインでも使える？

モデルをダウンロードし終えていれば、そのパソコンはインターネット接続なしで答えます。別の端末からそのパソコンに届くには、2台の間にネットワークの経路が必要です。同じ家庭内や社内のネットワークなら、オフラインでも動きます。

## <a id="running-it"></a>運用する

### <a id="how-do-i-update"></a>アップデートするには？

`waired update`を実行するか、Wairedアプリに更新のお知らせが出たらそれを選択します。[Wairedをアップデートする](/ja/getting-started/update/)を参照してください。

### <a id="how-do-i-remove-it"></a>削除するには？

コマンド1つ、約10秒です。ダウンロード済みのモデルを残すかどうかを選べます。[Wairedをアンインストールする](/ja/getting-started/uninstall/)を参照してください。

### <a id="something-is-wrong-where-do-i-start"></a>調子が悪いとき、まず何をすればいい？

`waired doctor`を実行します。全体を検査して、直せるものを修復します。そのあとは症状別にまとめた[トラブルシューティング](/ja/troubleshooting/)を参照してください。
