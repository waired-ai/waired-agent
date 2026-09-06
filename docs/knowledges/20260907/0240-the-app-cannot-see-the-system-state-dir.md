# デスクトップアプリはシステム状態ディレクトリを見られない (20260907 02:40)

## Issue

waired-agent#1269。アプリの Sign out… が確認と管理者パスワードまで進むのに
何もサインアウトしない。真因はアプリが昇格コマンドに渡す `--state-dir` が
**常に間違っていた**こと。

`cmd/waired-tray` は「システム導入か」を
`os.Stat(<システム状態ディレクトリ>/identity.json)` の成否だけで判定し、
**エラーの種類を見ていなかった**。

## Learnings

**システム状態ディレクトリは 3 OS すべてでデスクトップユーザーから読めない。**
`internal/platform/secrets` の `secureDir` が Unix で 0700 root、Windows で
SYSTEM+Administrators の DACL を付ける(`identity.PathsFor` 経由)。実測:

```
macOS  /Library/Application Support/waired   drwx------  root  admin
Linux  /var/lib/waired                       drwx------  waired waired
```

Linux のデスクトップユーザーは `waired` グループに入っていない。したがって
デスクトップユーザーとして動くプロセスの stat は **必ず EACCES** になる。

**`os.Stat` は「無い」と「読めない」を区別できない。** `err == nil` だけを見る
コードは、締め出されているシステム導入を「存在しない」と読む。分岐が要る:

```go
case errors.Is(err, fs.ErrPermission): // 締め出されているシステム導入
```

Go は Windows の `ERROR_ACCESS_DENIED` を `fs.ErrPermission` に写すので、
1 分岐で 3 OS を覆える。

**この誤りはリポジトリ内で既に 2 回見つかって直っていた。**
`cmd/waired/statehint.go` の `resolveSystemFallbackAt` は自らのコメントで
「古い os.Stat の推測を置き換える正直な版」と名乗り、
`cmd/waired/doctor_statedir.go` は #1005 で absent / unreadable / system-wide の
3 分類を入れ、`packaging/install/install.sh` は
「素の `[ -e ]` は、登録されているホストで『未登録』と答える」と明文で記録して
いる。**アプリ側の複製だけが取り残されていた** — 同じ知識の N 個の複製は
N 通りに古びる。

**失敗が静かなのが最悪の性質。** 誤った per-user ディレクトリに対する
`waired logout` は、存在するディレクトリを見つけ、5 つのパスを消そうとして全部
`ErrNotExist` を無視し、`logout: identity + secrets removed.` と表示して
**exit 0** で返る。呼び出し元はエラーを受け取らないので、ダイアログも出ない。

**現場で 1 行で確かめる方法がある。** アプリのフラグ既定値がアプリ自身の答え
なので、デスクトップユーザーとして

```sh
/Applications/Waired.app/Contents/MacOS/waired-tray -h 2>&1 | grep -A2 state-dir
```

を実行し、印字されるパスが `~/Library/...` なら欠陥、
`/Library/Application Support/waired` なら直っている。デバッガは要らない。

**権威はデーモンが既に公開していた。** `GET /waired/v1/setup/state` の
`state_dir` は「クライアント側の既定値は `--state-dir` 付きで起動したデーモンと
静かに乖離する」という理由で存在する(waired#835 §11.1)。セッション未発行の
デーモンでは空になるので、手元の判定も両方要る。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1269
- https://github.com/waired-ai/waired-agent/issues/1005
- docs/decisions/20260907/0230-sign-out-is-the-daemons-job.md
