---
status: accepted
---

# マシンに残る共有設定は 1 つのスイッチだけ (20260830 17:47)

## Status
Accepted。オーナー裁定 2026-08-30（waired-ai/waired#1297）: 誰に提供するかは
コンソールが決め、マシンが持つのは「そもそも貸し出すか」という 1 つの答え
だけ。本記録はその agent 側の実装決定を固定する。実装は
waired-ai/waired-agent#1164、proto は #1163（`proto/v0.2.61`）。

## Context

「このマシンの共有」に対して agent 側に書き手が複数系統あった。

- **メッシュ共有**: `runtime/desired-share`。書き手は `waired inference
  share on|off`・アプリのトグル・`waired init --share-with-mesh`・
  `agent.json` の `share_with_mesh`。コンソール側に対応する設定が無く、
  デバイスページは「マシンでコマンドを実行してください」と案内するしか
  なかった。
- **Public Share**: `runtime/desired-public-share` + 署名付き
  `POST /v1/devices/self/public-share`。同じ CP 列をコンソールも書くため
  書き手が 2 つになり、両者を揃えるために 60 秒の pending 窓と、自分の
  書き込みが CP から返るのを待つ `echoTrueSeen` ラッチを抱えていた。

## Decision

1. **永続する共有設定は `runtime/desired-sharing` の 1 つ。** 書くのは
   `waired share on|off`（管理 API `POST /waired/v1/sharing/{enable,disable}`）
   だけ。OFF はメッシュ・public guest・今後増える提供先を一括で止める。
   読み取りは `waired share status` と `GET /waired/v1/sharing`。ファイルが
   無い / 空は「誰も止めていない」= 共有する（このスイッチはオプトアウト）。
   「1 つの質問に永続の真実は 1 つ」は
   `docs/decisions/20260805/1236-local-inference-toggle-single-truth.md` の形。
2. **OFF は何も待たずに止め、永続化は後。ON は鏡像で、永続化が先**
   （public-share spec §8.3 の必須要件をそのまま実装）。ゲートを閉じ、実行中の
   guest ストリームを切ってからディスクに書く。ON 側は逆順で、選択を記録
   できなかったマシンがその記録を根拠に提供を始めない fail-safe。
3. **セッションラッチは非永続のまま、対象を public に広げる。** アプリ終了
   （Quit）で全提供が止まり、次回起動で解ける。操作はポリシーではない —
   `docs/decisions/20260801/1035-mesh-share-suspension-is-live-only.md` は維持
   で、上書きしない。
4. **メッシュ共有はコンソールの指示になる。** `InferenceState.DesiredShare`
   （`mesh-share-v1` capability ゲート付き）を毎フレーム適用し、
   `runtime/applied-mesh-share` にキャッシュする（再起動から最初の署名 map
   までの隙間を作らない。`applied-residency` と同じ形）。ローカルの書き手は
   無い。毎フレーム再アサートが安全なのはまさにそのためで、act-once-per-value
   （`DesiredIdleTimeout`）が守る「ローカルの変更」がこの設定には存在しない。
   `InferenceState.NotShared` の意味は「デバイスローカルの半分」（本スイッチと
   Quit ラッチ）に狭まる。
5. **public について agent は何も永続せず、ブート時は OFF。** 当初案は
   `desired-public-share` を CP の最後の言葉のキャッシュに降格する予定だった。
   退役する `echoTrueSeen` の並行セッションによるレビューで、あのラッチが
   実際に支えていたものが判明した: 無ければ再起動が記憶した `true` から
   立ち上がり、CP が何か言う前に他人へ提供する。他人への提供は厳格に
   オプトイン（public-share spec §4.1: デフォルト OFF）なので、ファイルごと
   廃止した。
6. **public のゲートは 2 つの答えを合成する。1 本のフラグに畳まない。**
   「コンソールの設定」AND「マシンが貸し出しているか」。初版はスイッチ OFF で
   public 値をクリアして 1 本に畳んでいたが、それだとマシンが戻ったとき
   public が OFF のまま — コンソール側の値は変わっていないので、再アサート
   する者がどこにもいない。`StopServing` は実行中を切るだけにする。
7. **書き込みサーフェスの削除は一括。** `waired inference share`・
   `waired public share|unshare`・`waired init --share-with-mesh`・
   インストーラの `--share-with-mesh` / `-ShareWithMesh`・`agent.json` の
   `share_with_mesh`・アプリの提供側トグル行・それらを書いていた管理ルート。
   プレリリースで互換を保つべきリリースが無い（waired#1297）。読み取り専用の
   状態表示（CLI・アプリ）は残す。
8. **旧 2 ファイルは読まずに消す。** `RemoveRetiredSharingFiles` がデーモン
   起動時に `runtime/desired-share` と `runtime/desired-public-share` を削除
   する。2 つは別の質問への答え（「自分のメッシュに出さない」/「他人に
   出さない」）で、どちらも新しい質問の答えではない。裁定は「全マシンが共有
   再開」（waired#1297）。マイグレートせず削除するのは、誰も読まないファイルを
   残すと後の読み手が別の意味で復活させ得るため。

## Consequences

- アップグレードでメッシュ共有は全マシン再開になる（ローカルの旧ファイルは
  読まれず、コンソール側の新設定は既存行を「共有中」と読む）。public は CP の
  `public_share_enabled` がそのまま真実で、値は保存される — ブート既定 OFF は
  「CP の言葉が届くまで提供しない」であって、値のリセットではない。
- pending 窓と echo ラッチのクラスの複雑さが消える。public 側のコントローラは
  「CP の設定を読む + 実行中を切る」だけになった。
- 配布順序に依存が生まれる: `DesiredShare` を送らない古い CP の下では既定
  （共有）のままで安全だが、コンソールのトグルが効くのは CP の更新後。#1164 は
  CP 側（waired#1301）の prod 反映まで hold（waired#1297 のコメント）。
- アプリを起動しないマシン（サーバ・ヘッドレス）に Quit ラッチが掛からないのは
  従来どおりで、対象が public に広がっても 1035 の帰結は変わらない。

## Refs
- waired-ai/waired#1297（オーナー裁定・親 issue）
- waired-ai/waired-agent#1163（proto: `InferenceState.DesiredShare` /
  `CapabilityMeshShareV1`、`proto/v0.2.61`）・#1164（本実装）
- `docs/decisions/20260801/1035-mesh-share-suspension-is-live-only.md`（維持）
- `docs/decisions/20260805/1236-local-inference-toggle-single-truth.md`
  （「永続の真実は 1 つ」の先例）
- `waired` `docs/specs/waired_public_share_spec.md` §4.1・§8.3
- `cmd/waired-agent/share_control.go`・`public_share_control.go`・
  `internal/runtime/state/state.go`（`ReadDesiredSharing` /
  `RemoveRetiredSharingFiles`）・`cmd/waired/share.go`
