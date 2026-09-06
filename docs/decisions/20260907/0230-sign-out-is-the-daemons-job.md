---
status: accepted
---

# サインアウトはデーモンの仕事にする (20260907 02:30)

## Status

Accepted

## Context

waired-agent#1269 の報告は「macOS のアプリで Sign out… を選び、確認して管理者
パスワードを入れても、メニューは ● Connected のまま変わらず、エラーも出ない」
だった。調べると独立した 2 つの欠陥が同じクリックの上に乗っていた。1 つ目は
アプリが渡す `--state-dir` が常に間違っていたこと(別記録)。ここで決めるのは
2 つ目、**サインアウトだけがデーモンの外にいた**ことへの対処。

デーモンは identity をメモリ上のセッションに保持し、ディスクを読み直すのは
起動時・ノード鍵ローテーション・再認証ログインの 3 経路だけ。ファイル監視も
定期再読込も無い。だから昇格した `waired logout` がファイルを消しても、
デーモンは古い identity を配り続けた。報告にあった 2 分の遅れはトークン更新の
周期そのもの(`refreshLead = 2m`)で、しかもそれが起きても `authGate` は CP 向け
コンテキストを畳むだけで `Enrolled`/`Active` は true のままだった。さらに
`restoreIdentityIfMissing`(waired-agent#800)が生きたセッションから
identity.json を書き戻すため、消したはずの登録が次のサインインで復活した。

対比が決め手になった。**サインインは #175 以降 3 OS のいずれでも管理者権限を
要さない** — アプリは `POST /waired/v1/login/start` を投げ、状態ディレクトリを
所有するデーモンが登録を行う。**サインアウトだけが 3 OS すべてで昇格を要求して
いた**。非対称なのは OS 間ではなく、サインインとサインアウトの間だった。

## Decision

`POST /waired/v1/logout` を追加し、サインアウトをデーモンの仕事にする。
アプリと CLI はこれを呼び、答えないデーモン(不達、または 404 = ルートを持たない
旧版)のときだけ従来の昇格経路に落ちる。

デーモンは 1 つの順序で 3 つの副作用を行う。

1. **CP へ deauth**(資格情報がまだディスクにある間)。best-effort で、失敗しても
   ローカル削除は止めない。順序も理由も CLI が以前から持っていた規則と同じ。
2. **セッションを畳む**(削除の前)。トークン更新は回転したトークンを `secrets/`
   に書き、reconciler も runtime-state writer も書く。**動いているものを止めて
   から消す**のが、外部プロセスには実行できない唯一の段で、これがサインアウトを
   デーモンに置く理由そのもの。
3. **削除** — `identity.RemoveEnrollment` で、デーモン自身の状態ディレクトリを。

進行中のサインインがあるときは 409 で断る。進行中の OAuth を取り消すのは
`loginController` に新しい配線が要り、Sign out 行は登録済みデバイスにしか
出ないので稀。意図的な範囲外とする。

`--server-only`(deb の prerm が使う、ローカルを残すモード)は委譲しない。
デーモンのルートは常に削除するため。

### 権限は広がらない

ルートは mutating verb なので `writeGuard` が既にローカル IPC ソケットに限定
する。ループバック TCP からは届かず、したがってブラウザからも届かない。

そのソケットは `internal/platform/localipc` が 0666 で開いており(mbp14 で
`srw-rw-rw-` を実測)、**同じソケットに載る `POST /login/start` は `auth_key` を
受け取る** — ローカルの任意プロセスがブラウザを経ずにこのデバイスを別アカウント
へ無言で再登録できるのが現状の境界。`ModeLogout` のサインアウトはそれより弱く、
CP はデバイス行を残し `waired init` で回復できる。よって新しい権限は増えない。

なお「ソケットが 0666 であること」自体の是非は本件の範囲外で、別に扱う。

## Consequences

- アプリの Sign out… は**管理者パスワードを求めなくなる**(サインインと対称)。
  旧版のデーモンに対しては従来どおり 1 回求める。
- メニューは次のポーリング(5 秒以内)で ○ Not signed in に変わる。デーモンの
  再起動もトークン失効待ちも要らない。
- 端末からの `waired logout` も同じ経路を通るので、issue の観察1 が閉じる。
- `restoreIdentityIfMissing` による巻き戻しは**順序で**止まる。セッションを
  畳んだ後は `sb.current()` が nil で `Start` の修復分岐に到達せず、
  `liveIdentity()` も nil を返す。フラグは足していない。どちらのガードも
  これまでテストが無かったので、`TestLoginStartAfterLogoutDoesNotRestoreIdentity`
  で固定した。
- 5 パスの削除リストは `identity.RemoveEnrollment` に 1 本化した。#261 で
  refresh token が追加され、waired#1277 で gateway token が外れた履歴のある
  リストで、2 か所目に写すべきものではない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1269
- https://github.com/waired-ai/waired-agent/issues/800
- docs/decisions/20260727/2030-daemon-owns-reauthentication.md — 再認証を
  デーモンに置いた判断。本記録はその延長で、サインアウトを同じ側に寄せる。
- docs/decisions/20260821/0228-uninstall-removes-what-is-running.md
- docs/decisions/20260827/2307-uninstall-stops-the-running-tray.md
- docs/decisions/20260828/0027-an-update-puts-the-running-app-back.md
