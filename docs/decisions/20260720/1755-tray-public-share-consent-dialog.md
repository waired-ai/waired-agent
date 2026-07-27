---
status: accepted
---

# トレイの Public Share 同意ダイアログ — 表示ごと warning 取得・docs はメニュー行・Windows のボタンラベル (20260720 17:55)

## Status
Accepted

## Context
トレイから Public Share を有効化する際、利用者にセキュリティ/プライバシー警告を提示し、その同意を control plane に記録する必要がある(waired#833)。実装にあたり 3 点の制約に直面した。(1) 警告文面はサーバ側で版管理され更新されうるが、利用者が実際に読んだ文面と記録される同意版がずれると「読んでいない版に同意した」ことになる。(2) 警告本文には詳細を説明する docs ページへの導線を出したいが、ネイティブのダイアログバックエンド(Linux の polkit/zenity 系、Windows の MessageBoxW、macOS)はいずれも本文中のハイパーリンクをクリック可能にはレンダリングしない。(3) Windows の `MessageBoxW` は OK/Cancel などの標準ボタンのラベルを差し替えられない — TaskDialog 系でカスタムボタンを使うには comctl32 v6 のサイドバイサイドマニフェストが必要で、トレイバイナリはそれを積んでいない。

## Decision
1. **表示ごとに `GET /waired/v1/public/warning` を取得し、表示した版をそのまま `POST /public/consent` の `warning_version` に載せる**。キャッシュした古い文面で同意を取らない。表示中に版が上がっていて 409(`warning_version_mismatch`)が返ったら、警告を 1 回だけ再取得して 1 回だけ再試行し、それでも駄目なら諦める(無限ループにしない)。
2. **docs へのリンクはダイアログ本文中ではなく、トレイメニューの独立した行として出す**。ダイアログ backend がリンクを描けない以上、クリック可能な導線はメニュー側に置くのが唯一移植性のある方法。
3. **確認ボタンのラベル(Accept/Cancel の文言)は、ラベル差し替えの効かない OS では本文末尾に追記する**。`ConfirmWithLabels` シームは各 OS 実装に委ね、Windows はマニフェスト前提を避けて本文にラベルを畳み込む。

## Consequences
- 同意の完全性(表示 == 記録)が版ずれ下でも壊れない。再試行が有界なので、版が高頻度で動いても UI がハングしない。
- リンクをメニュー行に分離したことで、ダイアログはテキストと 2 ボタンだけの最小構成に保て、3 OS 全てで同一の描画契約になる。
- Windows でボタン文言が本文に回るぶん本文がやや冗長になるが、comctl32 v6 マニフェスト追加という配布形態の変更を避けられる。将来 TaskDialog へ移行する場合はこの追記を外す。

## Refs
- internal/gui/tray/tray.go (`runPublicConsent` / `onPublicShareToggle` / `confirmWithLabels`)
- https://github.com/waired-ai/waired-agent/pull/118
