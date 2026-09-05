---
status: accepted
---

# お知らせは公開され、繰り返されなければ消える (20260905 00:00)

## Status
Accepted。オーナー裁定（2026-09-05、作業セッション中、waired-ai/waired-agent#1205）:

> 汎用的に、こういう通知を全部出す（つまり上書きされない）感じにしてはどうかと思います。消えるかは、しばらく通知がなければ消す、という形でどうでしょうか？なので、出しておきたい通知については定期的にフィールドにpushし続ける形です。エスケープについては通知フィールドのモジュールでそもそも違反を受け付けないようにするといいかも。

続けて:

> この方針でtrayを再実装し、trayとwaired doctor, waired statusに問題のメッセージを出すようにしてください。なお、claude code内では出さないでいいです。

この記録がその裁定の引用元になる。裁定文中の「通知」は本文では「お知らせ」と書く
（`docs-site/TRANSLATION.md` の headword。`internal/platform/notification` の
OS のデスクトップ通知と読み分けるため）。

## Context

#133 のモデル切り替えの提案 — このパソコンが対話床（`router.CodingAgentSelectionFloorTokps`
= 60 tok/s）を下回ったら軽いモデルへ、余裕があれば強いモデルへ — は、
`cmd/waired-agent/inference.go` の `currentRecommendations` が導いて、ローカル管理 API の
`ModelCatalogResponse` に載せていた。それを読む相手は 2 つしか無かった（#1205 の表）。

- `waired init` / `waired runtimes benchmark` の完了時に**1 回だけ**、同期で出す問い
  （`cmd/waired/init_benchmark.go`）。
- tray の 5 秒ポーリング → 一度きりのネイティブダイアログ、それと Inference サブメニュー
  の 2 クリック奥にある 1 行。

セットアップの最中は誰かが見ている。問題はその後だ。`runBootBenchmarkLoop` は
モデル・variant・エンジン種別・エンジン版のどれかが変わるたびに再計測する。`/model` や
tray やコンソールからのモデル切り替え、エージェント更新によるエンジン pin の移動
（人の操作ではない）、余裕が出てきたときの step-up — どれも、画面の無いサーバでは
誰にも届かなかった。計測は正しく、**届け方が欠陥**だった。

`docs/decisions/20260904/0000-retire-the-long-context-sweep.md` は、廃止した長文
コンテキストの sweep について同じ結論に達している（「測り方より先に届け方を解く」）。
#1204 はその廃止で失った観測を記録する issue、#1205 は sweep に依存しなかった残りの
隙間 — この記録が塞ぐもの — を記録する issue。

#1205 の表は、画面の無いホストへ届く push 経路として Claude Code の statusline と
Stop hook を挙げていた。オーナーはどちらも採らないと裁定した（上記「claude code内では
出さないでいいです」）。

## Decision

1. **1 つの汎用フィールドが、すべてのお知らせを運ぶ。** `internal/notice` を新設し、
   管理 API に `GET /waired/v1/notices` を足す。後から書く産出側が先の産出側を
   上書きすることは無い。#133 の提案は最初の産出側であって、専用の欄ではない。
2. **お知らせは、繰り返されなければ消える（lease）。** レジストリ
   （`internal/notice/registry.go`）は産出側ごとに期限を持ち（`DefaultTTL` = 60 秒）、
   読み出し時に期限切れを落とす。産出側は公開し続ける（`cmd/waired-agent/notices.go`、
   `noticeRepublish` = 15 秒）。レジストリは判定を保存しない — 産出側は毎回ベンチマーク
   から導き直すので、条件が消えれば言わなくなるだけで、消す操作は要らない。提案を
   断ったときとモデルを切り替えたときは次の tick を待たず同期で再公開する（答えた行が
   15 秒残るのは「答えが効いていない」と読める）。
3. **公開の単位は産出側の集合全体で、種別ごとの 1 件ではない。** `Publish()` は
   産出側の全集合を受け取り、空を渡せばその産出側をその場で消す。種別ごとの push に
   しなかった理由は 2 つ、どちらも産出側が繰り返しはじめて初めて現れる — 種別ごとの
   push は heartbeat ごとに `FirstSeen` を打ち直し、`FirstSeen` が守るはずの表示順を
   毎回並べ替える; また、産出側の答えが別の対象へ移ったとき旧い方が消えたと言えず、
   矛盾する 2 件が lease いっぱい並ぶ。
4. **エスケープと検証は notice モジュールの中に置き、違反を「拒否」ではなく「作れない」
   にする。** 産出側は種別ごとの型付きコンストラクタ（`LighterModel` / `BetterModel`）を
   呼び、文字列を渡さない。公開時に拒否すると、お知らせは黙って落ちる — 誰にも見えない
   お知らせこそ、この仕組みが直す欠陥である。サニタイザは構築時だけでなく
   `UnmarshalJSON` でも走る: 3 面ともこの型を socket から復号して描くので、不変条件は
   ワイヤの両側で成り立たなければならない。除くのは、制御文字（ターミナルはエスケープ
   シーケンスを**実行する**）、改行（全面が 1 行で描く）、Unicode の双方向制御
   （人が読む行の順序を変える）、そして各面が「この行をどう読むか」を示すのに使う
   状態記号（✓ を含むお知らせは判定を偽造する。`cmd/waired` の記号表とは
   `TestSanitiseStripsEveryMarkThisCLIFolds` で突き合わせる — 表は package main で
   import できず、複製は古びる）。メニューのマークアップはエスケープ**しない**: それは
   描く側ごとの事情で、既に widget で 1 回行っている（`internal/gui/tray/rows.go`）。
5. **tray の Inference サブメニューにあった提案行は、メニュー上部のお知らせ行へ移す。**
   アップデートの案内と同じブロック、`notice.MaxActive`（= 5）の枠ちょうど。一度きりの
   ネイティブダイアログ（`maybeShowRecommendation`）は**残す** — ダイアログが「いま気づけ」、
   行が「まだ事実だ」で、アップデートの案内と toast が既に持つ対になる。行はグレーに
   しない（全行にクリックがある）。生きた推奨が無いままクリックされたら状態レポートを
   開く — お知らせのポーリングとカタログのポーリングは独立した best-effort な GET で、
   何も起きない行が最悪だから。
6. **`waired doctor` に出すのは警告だけ。** doctor はセットアップの健康を報告する面で、
   「もっと良いモデルを動かせる」はその欠陥ではない。出すには ✓ ⚠ ✗ · の横に 5 つ目の
   記号を発明することになる。お知らせは終了コードを動かさない（失敗だけを数える）。
   route を持たない古い daemon には黙る — 「お知らせが無い」と「この daemon はお知らせを
   公開しない」は、見せるものが同じ。
7. **専用 route `GET /waired/v1/notices`、socket 専用。** 既存レスポンスの欄にしない:
   `waired status` は `/waired/v1/status` を JSON のまま逐語で刷るので、そこに置くと
   生の JSON と描いた行で 2 回出る; `/inference/status` は doctor が読まない唯一の route;
   汎用の欄を推論ペイロードの中に置くべきでもない。`tcpReadRoutes` は Go 以外の消費者が
   叩く route の一覧で、tray・doctor・status は既に IPC socket で daemon に届いている。
   テストが pin するので、TCP に開けるのは反射ではなく決定になる。

## Consequences

- **`waired doctor` と `waired status` は pull の面である。** 届くのは、ログインして
  見る運用者にであって、放置されたサーバに push されるわけではない。#1205 の表が
  挙げた画面無しの push 経路は Claude Code の statusline と Stop hook の 2 つだけで、
  オーナーはどちらも採らないと裁定した。残る隙間は見落としではなく決定である。
- **画面の無い運用者はお知らせを読めるが、消せない。** dismiss は socket 専用の POST
  （`/waired/v1/inference/recommendation/dismiss`）で、CLI の動詞が無い。お知らせに
  応じる — `waired models use <model-id>` — のが消す方法で、条件が消えれば言わなく
  なる。この隙間は #1226 で追跡する。
- 同じ文が 3 面に出る。産出側はコンストラクタを 1 つ書けばよく、3 面を編集しない。
  `waired status` の `Notices:` ブロックは `--observability` の後ろではなく無条件に
  出て、言うことが無ければブロックごと出ない。
- `internal/gui/tray/state_recommendation_test.go` は消えた。その規則 — 断った組は
  何も言わない、対象が空なら何も言わない、step-down が勝つ — は導出とともに daemon 側
  （`cmd/waired-agent/notices_test.go`）へ移った。tray はもう規則を適用する唯一の面では
  ない。
- お知らせが出るまで最長 15 秒、消えるまで最長 60 秒（同期で再公開する 2 経路を除く）。
  docs-site は「1 分以内」と書く。
- Claude Code の面（statusline、hook）には出さない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1205
- https://github.com/waired-ai/waired-agent/issues/1204
- waired-ai/waired-agent#133（対話床の step-down 提案）
- docs/decisions/20260904/0000-retire-the-long-context-sweep.md — 「測り方より先に届け方を解く」
- `docs-site/TRANSLATION.md` — headword「お知らせ」
- `internal/notice/`、`internal/management/notices.go`、`cmd/waired-agent/notices.go`、
  `internal/gui/tray/state.go`、`cmd/waired/notices.go`、`cmd/waired/doctor_notices.go`
