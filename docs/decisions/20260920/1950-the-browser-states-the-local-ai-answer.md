---
status: accepted
---

# ローカル AI のオンはブラウザが明言する。指示の中身から推測しない (20260920 19:50)

## Status

Accepted。オーナー裁定 2026-09-20（waired-ai/waired-agent#1446）。製品の方針は変えない。方針の一次ソースは引き続き waired-ai/waired#1056 のオーナー決定コメント（2026-08-03）決定 4「推奨要件未満は警告つきオプトイン可」で、その waired-agent 側の実装は `docs/decisions/20260805/1236-local-inference-toggle-single-truth.md`（#465）。本記録が変えるのは、その後に足された「ブラウザ側のオプトインをどう検出するか」の機構だけ。

## Context

`cmd/waired-agent/setup_desired.go` の `Apply` に、次の一節があった（origin/main 6ddc6003 の :811-821）。

```go
if changedServe && (d.engine != "" || d.modelID != "") {
    r.provider.setupEnableLocalInference("setup: the wizard asked this device to serve")
}
```

`changedServe` は「このプロセスが最後に見た desired と、inference フィールドだけを空にして比較して違うか」（:729-731）。#507 が「ローカル推論オフ」を「サブシステムを作らない」から「エンジンが待機する」に変えたので、推奨要件未満のコンピュータにもブラウザのウィザードが届くようになった。そこで engine ステップが拒否されないように、CP が desired_engine / desired_model_id を書いたこと自体を「人が頼んだ」と読んでいた。waired#1056 決定 4 の「オプトインは両方の面に要る」の、ブラウザ側の面がこれだった。

問題は、この判定が指示の**中身**を読んでいて、**何が変わったか**を読んでいなかったこと。CP は desired 値を決して消さず毎フレーム再送するので、人が頼んでいないのに推論がオンに戻る経路が 3 つあった。

| 経路 | 何が起きるか |
|---|---|
| 1. デーモン起動直後の最初のフレーム | 比較対象がゼロ値なので、一度ブラウザでセットアップしたコンピュータが持ち続ける指示は必ず「変化」に見える |
| 2. CP 自身の追随（#647） | CP は desired_model_id をデバイスが報告した ActiveModel に寄せる。追随はデバイスに従っているのに、デバイス側はそれを人の依頼と読む |
| 3. engine / model 以外だけが動いたフレーム | `changedServe` には benchmarkGen と integrations も入っているのに、ゲートは「いま持っている指示が engine か model を含むか」しか見ない。セットアップ済みのコンピュータで `waired inference off` した後に、ブラウザで「速度を測り直す」を押す、あるいはコーディングツールのトグルを変えるだけで推論がオンに戻る |

コードのすぐ上のコメントは「ベンチマーク世代だけは依頼ではない」と主張していたが、指示に engine か model が残っている限りその主張は成立していなかった。

実機（0.0.3-rc6 の Linux のフリート機、自前ビルドなし）で確認したこと。

- `waired inference off` のあと agent を再起動すると、起動の約 1 秒後に `turning local inference on` / reason=`setup: the wizard asked this device to serve` が出て、`<state-dir>/runtime/desired-inference` が**ディスク上で** `disabled` から `enabled` に書き換わった。人が永続化した選択そのものが消えた。
- **同じフレーム**に `setup: leaving the desired model alone; nobody here chose it this install` が出ていた。モデルステップは #308 の鮮度判定でその指示を「誰も選んでいない残り物」として断りながら、推論だけオンにしていた。適用しない指示のためにサブシステムを起こしていたことになる。

## Decision

**暗黙の依頼を廃止し、#597 の明示フィールド `desired_inference` を唯一の入口にする**（オーナー裁定 2026-09-20）。ブラウザ側の面（waired#1056 決定 4）は無くなったのではなく、推測された合図から明言された答えに変わった。ウィザードは engine ステップとモデルステップの書き込みに `inference: "on"` を載せる（private waired の `web/admin/src/pages/Setup.tsx`、`chooseEngineStep` と `writeSetup`）。CP は `"on"` を engine / model と同乗させることを既に許している（`"off"` との同乗だけが 400）。

これを成立させるには前提工事が 1 つ要った。`applyDesiredInference` は永続値ごとに 1 回しか効かず（記録は `<state-dir>/runtime/setup-inference`）、ローカルの `waired inference off` はその記録に触らない。この非対称は #465 の要請そのもの（人のローカルの選択は、ウィザードが別のことを言うまで立っていなければならない）だが、同じ語を二度言えないという代償があった。実機で再現済み: ブラウザの「ローカル AI をオンにする」に相当する書き込みは 1 回目だけ効き、2 回目からは CP が 200 を返して値を保存しても、デバイスは 60 秒ぶんのフレームを通して完全に無反応（#1459）。

そこで proto に `DesiredInferenceSetAt` を足し、記録も（値, 依頼の時刻）の対にした。CP が答えを記録した時刻が変われば、同じ値でも新しい依頼として扱う。時刻が空なら今日どおり値ごと 1 回に退化するので、移行作業は要らない。

## Consequences

- PR は 3 本に分かれ、順番が製品の正しさに効く。
  1. proto: `DesiredInferenceSetAt` + `CapabilityOnboardingV5`、および同じ PR での agent 側の宣言。
  2. waired: 列に時刻を刻む、新 capability でのゲート、ウィザードの 2 箇所に `inference: "on"`。
  3. waired-agent: 暗黙の依頼の削除と、依頼ごとの適用。
- **PR 3 を積んだ agent を、PR 2 を積んだ CP が本番にデプロイされる前にリリースしてはいけない**（本裁定の帰結）。admin の SPA は CP バイナリに go:embed で焼き込まれており（`web/admin/embed.go`）、別のアセットホストは無い。古い SPA と新しい agent の組み合わせでは、推奨要件未満のコンピュータがブラウザから二度とオンにできない。PR 2 と PR 3 の間はどちらの機構も生きているが、`setupEnableLocalInference` は既にオンなら何もしないので二重には見えない。
- capability を足す PR は proto 単独にできない。`TestEveryProtoCapabilityIsDecided`（`internal/controlclient/network_map_capability_coverage_test.go`）が、proto が公開した定数を本体が宣言も免除もしていないと落ちる。mesh-share-v1 が宣言されないまま出荷された waired#1297 の再発を止めるガードで、`docs/decisions/20260719/0000-concurrent-proto-development.md` の「proto は単独の小 PR」と正面から噛み合う。事実として記録しておく。
- 宣言する capability の CSV は CP の STRING(256) 列に入り、正規化が sort してから末尾を黙って捨てる。`onboarding-v5` を足すと 211 から 225 バイトになり、agent 側のガード `TestCapabilityCSVFitsTheColumn` が 32 バイトの余白を守っているため 1 バイト超過で落ちた。**このガードが切り捨ての前に止めた最初の実例**。列幅の是正は #1456 として別レーンが進めている。
- ターミナルの `waired init` は影響を受けない。オン/オフを独自の経路で扱っている（`cmd/waired/init_daemon_inference.go`）。
- 既存テストのうち、`TestSetupApplyTurnsLocalInferenceOn` は反転し `TestSetupApplyNeverTurnsLocalInferenceOnByItself` になった。製品の方針（waired#1056 決定 4）は無傷で、変わったのは機構だけ。`TestDesiredInference_OffBesideAStandingEngineDoesNotAskToServe` は守る対象が消えて空虚になるため削除した。
- ブラウザからの戻り道には、この決定では直していない狭い隙間が残る。「ローカル AI をオンにする」ボタンは `complete` の下にしか描かれない（private waired の `web/admin/src/pages/setup/Progress.tsx`）ので、推論がオフでセットアップが未完了のコンピュータにはブラウザからの経路が無い。#1459 に記録した。

## Refs

- waired-ai/waired-agent#1446（この決定が閉じる issue）
- waired-ai/waired-agent#1459（明示フィールドが再アサートできない。前提工事）
- waired-ai/waired-agent#1456（capability の CSV が入る列の幅）
- waired-ai/waired-agent#597 / #465 / #507 / #308 / #647 / #1297
- waired-ai/waired#1056 決定 4 / waired-ai/waired#1109 / waired-ai/waired#1110
- `docs/decisions/20260805/1236-local-inference-toggle-single-truth.md`
- `docs/decisions/20260719/0000-concurrent-proto-development.md`
