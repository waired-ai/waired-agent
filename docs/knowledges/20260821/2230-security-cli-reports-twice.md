# security(1) は結果を2回報告し、2つは一致しないことがある (20260821 22:30)

## Issue

waired-agent#799。macOS の `sudo waired init` が毎回この対を出していた。

```
WARN securestore: keychain write failed; file written, clearing any stale entry
     err="keychain set waired/gateway-token: exit status 36 (... User interaction is not allowed.)"
WARN securestore: failed to clear stale keychain entry after write failure
     err="keychain delete waired/gateway-token: exit status 195 (stderr=\"password has been deleted.\")"
```

2行目が「削除に失敗した」と言いながら、根拠として `password has been deleted.`
を引用している。

## Learnings

### 終了コードは OSStatus を 8 bit に切ったもの

`security` の終了コードは Apple の OSStatus を 256 で割った余り。これを知らないと
数字が読めない。実測 (macOS 26.6.2 / Apple Silicon、`/tmp` の使い捨てキーチェーン) の対応:

| exit | OSStatus | 名前 | 出る場面 |
|---|---|---|---|
| 36 | −25308 | `errSecInteractionNotAllowed` | セッションが無くプロンプトを出せない |
| 44 | −25300 | `errSecItemNotFound` | 見つからない |
| 45 | −25299 | `errSecDuplicateItem` | 既に在る |
| 195 | −61 | `wrPermErr` | 書き込み権限が無い |

**195 は本物の失敗**。「成功に見えるから成功として扱う」は誤りで、
`securestore.Read` は Keychain 優先なので、消え残りが新しいファイルを覆い隠す。

### stderr だけを読む分類は構造的に盲目

3つの形を実測した。どれも stderr だけでは判別できない。

- **ロックされたキーチェーンへの `find -w` は exit 36 で stderr が空**。
  文面が一切出ないので、stderr を見る分類器には成功と区別が付かない。
- **拒否された書き込みは2つのエラー文を出す** —
  `Write permissions error.` と `User interaction is not allowed.` が並ぶ。
  どちらが判定かは終了コードにしか書いていない。
- **exit 195 の delete の stderr は `password has been deleted.` だけ**。
  エラー文がゼロで、`security` 自身の成功時の文言が載っている。

→ **終了コードを先に読み、文面は後**。分類は untagged な純関数
(`internal/platform/keychain/security_outcome.go`) に置いて Linux でも回す。

### delete はロックされたキーチェーンでも成功する

実測: ロック状態の使い捨てキーチェーンに対する `delete-generic-password` は
**exit 0** で削除できた (`find -w` は同じ状態で 36 で拒否される)。
書き込みが拒否されても掃除は機能する = never-stale 不変条件はセッションが
無くても保たれる。

### キーチェーンの指定を省くと、書きと読みで対象が違う

`security` は **keychain 位置引数を省くと、`add` は既定キーチェーンに書き、
`find`/`delete` は検索リスト全体を走る**。`withKeychainTarget` は euid==0 の
ときだけ System keychain を明示するので、非 root の Set/Delete はこの非対称を
そのまま踏む。

実測でこれが exit 195 の出どころだった。root 側 (デーモンと `sudo waired init`
の root 部分) は `-A` 付きで System keychain に項目を作る。非 root の子が
login keychain への書き込みに失敗して掃除に入ると、位置引数が無いので
検索リストを走り、**root が作った System keychain の項目に届いて、それを
書き換えられずに `wrPermErr` を返す**。`password has been deleted.` は、
消せた分についての `security` 自身の文言。

### 真因は分類ではなく hop の作り方だった

`sudo waired init` がユーザーへ降りる `runLinkAllAsUser` は `runuser -u` /
`sudo -u` だけを使っていた。これらは権限は落とすが、子プロセスを **root の
bootstrap 名前空間に残す**。securityd のセッションエージェントはそこに居ないので、
login keychain への書き込みは常に `errSecInteractionNotAllowed` になる。

同じリポジトリの `internal/platform/browser/desktopuser.go` の `darwinHopArgv` は
`open(1)` のために既に正解を持っていた: `launchctl asuser <uid>` で名前空間へ入れ、
内側の `sudo -u` で権限を落とす。2段が両方とも必要。

**名前空間だけを変数にした実測** (macOS 26.6.2、コンソールにユーザーがログイン中。
同じ uid・同じユーザー・同じコマンド・同じ login keychain):

| 実行コンテキスト | `add-generic-password` |
|---|---|
| SSH セッション (`gui/<uid>` の外) | **exit 36** `(<default>) User interaction is not allowed.` |
| `gui/<uid>` の内側 | **exit 0**・項目が着地 |

`gui/<uid>` 側は、使い捨ての LaunchAgent を `launchctl bootstrap gui/<uid>` で
投入して実行した。**自分の gui ドメインへの bootstrap は root を必要としない**ので、
`launchctl asuser` (root が要る) を使わずに機構だけを分離できる。検証手段として
覚えておく価値がある。

GUI ログインセッションが無いホストでは入る名前空間が無いので、判定は
`launchctl print gui/<uid>` (installer の `darwin_start_app` と同じプローブ) で
分岐し、無ければ今日と同じ argv に落とす。

## Refs

- https://github.com/waired-ai/waired-agent/issues/799
- internal/platform/keychain/security_outcome.go
- internal/platform/browser/desktopuser.go (`darwinHopArgv`)
- packaging/install/install.sh (`darwin_tray_launch_plan` / `darwin_start_app`)
