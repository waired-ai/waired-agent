---
title: メンテナンスのコマンド
description: waired update、config、logs、version、keygenについて、各フラグの動作とログの出どころを説明します。
meta:
  audience: ターミナルで作業する人、画面のないパソコンを使う人
  needs: インストール済みのWaired
  time: 必要なコマンドを読むだけ
---

## <a id="waired-update"></a>`waired update`

```sh
waired update              # 確認して適用する。チャンネルは変えない
waired update --check      # 報告だけ
waired update --yes        # インストーラの確認なしで適用する
waired update --edge       # 最新のmainビルドに切り替える
waired update --stable     # stableに戻す
waired update --force      # リリース元から解決し直す（Linuxではパッケージ索引を更新するためsudoを求める）
waired update --notify on|off   # Wairedアプリのポップアップの更新案内
```

アップデートは、利用できるバージョンをローカルのサービスから読み取り、管理者権限で
公式のインストーラを実行し直して適用します。Linuxはapt経由、Windowsは
インストーラの管理者権限での入れ替え、macOSは管理者権限でのインストール
スクリプトの再実行です。ここにインストール済みの推論エンジンも、同時にこの
ビルドが固定するバージョンになります。[Wairedをアップデートする](/ja/getting-started/update/)を
参照してください。

`--notify off`はポップアップを止めます。Wairedアプリの更新の項目はどちらでも
残ります。

## <a id="waired-config"></a>`waired config`

バックグラウンドサービスの保存される設定を変えます。現在は、ログの詳細度です。

```sh
waired config log-level              # 現在のレベルを表示する
waired config log-level debug        # 詳細なログをオンにする
waired config log-level info         # 通常に戻す
```

レベルは`debug`、`info`（既定）、`warn`、`error`です。`debug`は、問題を再現する
前に切り替えるものです。バックグラウンドサービスとWairedアプリの両方に再起動なしで
すぐに反映され、再起動後も保持されます。オンの間、Wairedはログを多く保持します。
ファイル1つあたり32MBではなく128MBで、古いコピーはどちらでも10世代です。終わったら
`info`に戻してください。サービスが動いていないときは、選択が保存されて次の起動時に
適用されます。

## <a id="waired-logs"></a>`waired logs`

不具合報告に添付できるよう、直近のログを1つのファイルに集めます。

```sh
waired logs                          # カレントディレクトリにwaired-logs-<time>.txtを書く
waired logs -o report.txt            # ファイルを指定する
waired logs --since 30m              # どこまでさかのぼるか（既定は1h）
waired logs --mask-pii               # ホームフォルダ、ユーザー名、ホスト名、メールアドレスを伏せる
waired logs --full                   # 直近の16MBだけでなく、ローテートしたすべてのコピー
```

システムログにあるバックグラウンドサービスのログ、システムが保持している場合は
サービス自身のログファイル、推論エンジンのログを集めます。ローテートした古い
コピーも含まれます。ファイルは新しい順に合計16MBまで集めるので、issueに添付できる
大きさに収まります。`--full`はローテートしたすべてのコピーを集め、`debug`の
詳細度では数百MBになることがあります。

もっとも役に立つ報告にするには、先に詳細をオンにし、問題を再現してから集めます。

```sh
waired config log-level debug
# ...問題を再現する...
waired logs --mask-pii -o report.txt
waired config log-level info
```

`--mask-pii`は、ホームフォルダ、ユーザー名、マシン名、アカウントのメールアドレスを
プレースホルダーに置き換えます。最善の努力なので、共有する前にファイルを確認して
ください。手順全体は[不具合を報告する](/ja/getting-started/report-a-problem/)に
あります。

## <a id="waired-version"></a>`waired version`

```sh
waired version
waired version --json      # {version, buildSHA, os, arch}
```

## <a id="waired-keygen"></a>`waired keygen`

WireGuardの鍵ペアを生成します。`init`が代わりに行います。手動で実行するのは、
特殊な構成を組む場合だけです。
