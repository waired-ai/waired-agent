---
status: accepted
---

# setup の baseline は「畳んだ最初のフレーム」で刻む (20260827 15:20)

## Status

Accepted。

## Context

0.0.3-rc4 実機検証 (waired-ai/waired#1280) で、ブラウザ導線のつもりの
`waired init` がターミナル主導に落ちた (waired-agent#1033)。サインインを
素早く済ませると、ウィザードは「このコンピュータの応答を待っています」の
まま設定を掴めず、`waired init` は 3 分の grace を待ち切って
`No setup started in the browser; continuing here.` を出し、
`TakeOver()` する。両方の面はそれぞれ正確なので、見えない競争で導線が
決まっていた。

grace が短いのではない。`setupAwaitGrace` は 3 分で、production では
短縮されない。真因は `setupDriving()` が**永久に偽になり得る**こと:

- 制御プレーンは、デバイスが inference status を一度も push していない
  あいだ desired state をネットワークマップに載せない。したがって新規
  デバイスの最初のフレーム群は `Self.InferenceState == nil` で届く。
- `setupReconciler.Apply` は `st == nil` で即 return し、baseline を
  刻んでいなかった。
- 結果、desired を運ぶ最初のフレームが baseline そのものになり、
  `desiredChangedAt` はゼロのまま → `desiredStaleLocked()` が真 →
  `/setup/state` が `desired_stale: true` → ブラウザは駆動していないと
  読まれる。

`Apply` の baseline ブロックは、まさにこの型を防ぐためにゼロ値 fast path の
**上**に置かれている。`desiredSeen` のフィールドコメントも「指示を運ぶか
どうかに関わらず、ここで畳まれたフレームが 1 つでもあれば」と書いていた。
`st == nil` のガードがそのさらに上にあり、宣言済みの不変条件を破っていた。

既存テストが取り逃していた理由も同じ形で、`TestSetupDesiredFreshWhenTheWizardWrites`
は「新規デバイスの最初のフレームは何も運ばない」を **非 nil の空フレーム**
で模していた。本番の同じフレームは nil。フェイクが欠陥境界の上に置かれて
いて subject が走っていなかった (CLAUDE.md §Test discipline)。

なお ~40 秒の遅れは、probe goroutine がハードウェアプロファイルを同期実行
してから最初の push を出すためで、ネットワークマップの poll 自体は即時。

## Decision

**nil フレームも「何も運ばないフレーム」として baseline を刻む。**
`Apply` は `st == nil` で `desiredSeen` を立ててから戻る。

### 採らなかった案 1: 制御プレーン側で desired を早く配る

未 push のデバイスにも desired state を配れば順序は直るように見えるが、
**直らない**。fold は指示が在るときにしか書かないので、合成された状態は
ウィザードが書いたそのフレームで初めて現れる。つまりそのフレームが
「最初の非 nil」であると同時に baseline になり、失敗が前倒しになるだけ。

無条件に空の状態を載せる形なら順序は直るが、一度も publish していない
全デバイスの署名済みマップのバイト列が変わり、何も公開していないホストで
`applySelf` がゼロ値を持って走り始める。上の 1 行で足りる話に対して大きい。
制御プレーン側の「未 push のデバイスには合成しない」は、ウィザードの
「このコンピュータはまだチェックインしていません」カードと対になった
意図的な挙動でもある。

### 採らなかった案 2: nil を空フレームに正規化する

`if st == nil { st = &signer.InferenceState{} }` は一見きれいだが、nil を
ゼロ desired の経路に通してしまう。その経路は `setupNoteDesired("", false)`
を報告し、それは boot pre-pull hold を解く証拠 (「制御プレーンが答えて、
誰も駆動していない」)。何も告げていないホストはその証拠を出していない。
正規化すると、インストーラがサービス起動前にエンジンを置くホストで、
利用者がブラウザを開く前に bundled のフォールバックダウンロードが始まる
(waired-agent#305/#379)。だから baseline だけを刻んで戻る。

### 依存している不変条件

「nil フレームのあとに指示が来た」はフレームだけでは live な書き込みと
残骸を区別できない。この修正は live 側に倒す。根拠は推測ではなく制御
プレーン側の事実で、**デバイスの報告済みエンジン状態を消す唯一の書き手は、
同じミューテーションで desired 列も消す**(登録解除してから再登録する経路)。
したがって残骸の指示を持つデバイスは必ずエンジン状態を報告済みで、最初の
フレームが nil になることはない。#308/#645/#626 の保護はそのまま効く
(`TestSetupDesiredStaleOnLeftoverState` は無改変で緑)。

## Consequences

- ブラウザ導線が、初回チェックインとの競争に負けなくなる。
- **副次的に閉じる窓がある**: 同じ `desiredStaleLocked()` を
  `setupEngineInstallWanted` と boot pre-pull hold が読んでいる。新規
  デバイスではウィザードの書き込みが見えていなかったので hold が解けて
  しまい、ウィザードが別のモデルを指名すると二重ダウンロードになり得た。
- **残余**: 書き込みを運ぶフレームより前に**フレームが 1 つも届かない**
  場合 (マップストリームが未接続、または書き込みの後で初めて接続した
  場合)、baseline は依然その書き込みに乗る。これを消せるのは、CP が
  「いつ変わったか」を明示的に載せる案だけで、proto の追加が要る。
  今回は採らず、追随の issue に切り出す。
- 反転したテストは無い。`TestSetupDesiredFreshWhenTheWizardWrites` と
  `TestSetupDesiredStaleOnLeftoverState` は無改変で緑のまま、nil 前置きの
  行を持つ表テストが増える。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1033
- https://github.com/waired-ai/waired/issues/1280
- `docs/decisions/20260821/1420-setup-report-says-what-happened.md`
- `docs/decisions/20260805/1721-executor-lease-is-not-a-wizard.md`
- `docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md`
