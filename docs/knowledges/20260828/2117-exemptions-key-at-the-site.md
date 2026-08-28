# ガードの免除は現場に置く (20260828 21:17)

## Issue

`cmd/waired/glyph_format_guard_test.go`（waired-agent#798 (d) の回帰ガード）の
allow-list が `file:line` をキーにしていた。

```go
var glyphFormatAllowList = map[string]string{
	"claude_statusline.go:583": "JSON systemMessage consumed by Claude Code, not a console",
}
```

キーが「他人が編集するファイルの中の座標」なので、免除対象の上に一行入るたびに外れる。
導入後にこのガードに触ったコミットは 3 本あり、**3 本ともこの数字だけを書き換えたもの**だった。

| コミット | キー | 一緒に入った変更 |
|---|---|---|
| `07676bc1` (#936) | 導入、`:334` | — |
| `9da08ce7` (#960) | `334 → 384` | statusline に residency 行（25 ファイル） |
| `5ce64773` (#1042) | `384 → 514` | statusline に peer 行（26 ファイル） |
| `af5f9898` (#1091) | `514 → 583` | statusline にセッション別モデル（9 ファイル） |

理由の文字列は 4 回ともバイト一致。行番号は誰も決めていない座標なので、
**上を伸ばしたレーンが二つ並ぶと必ず同じ 1 行で rebase 衝突する**（#1091 で 2 回）。

## Learnings

### リポジトリ内のガード 23 個のうち、位置をキーにしていたのはこれだけだった

残り 22 個は、編集で動かないものをキーにしている。四つの型がある。

1. **現場のマーカーコメント** — `// grey: <why>`（`internal/gui/tray/tray.go`、
   `scripts/ci/tray-grey-row-guard.sh` が読む）、`//nolint:<linter> // reason`（11 箇所）。
   コードと一緒に動き、理由が読む人の目の前にある。
2. **パス＋式** — `scripts/ci/lookpathguard`、`scripts/ci/mgmtclientguard` の
   `{File, Expr}`。ただし同一ファイル内の同じ式を 1 エントリに畳むので、
   2 つ目の同型サイトは黙って覆われる。
3. **型＋フィールド名** — `scripts/ci/protoconsumer`。コンパイラに固定されるので最も安定だが、
   フィールド名がリポジトリ全体で照合されるため、無関係な同名フィールドが
   チェックを満たしてしまう（そのファイル自身が長く警告している）。
4. **パス接頭辞** — `scripts/ci/testnet-*.txt`。先頭に所属規則が書いてある。

**採ったのは 1。** `// glyph: <why>` を `// grey:` と同じ形で導入した。
理由がテストから 500 行離れた場所ではなく、その行の上に来る。

### 双方向に見ないと stale が残る

旧 allow-list には「エントリが実在のサイトに当たっているか」の確認が無かった。
サイトが動いた／絵文字が消えたエントリは、誰にも読まれずに残る。
`lookpathguard` / `mgmtclientguard` は「宣言はあるが現場が無い」を落とす。
新しいガードも、**理由の無いマーカー**と**何も免除していないマーカー**を落とす。

### 同じガードに「手で同期している／見えていない」欠陥が 3 つ同居していた

* **連結された書式文字列がガードに見えていなかった。** `call.Args[idx]` が
  `*ast.BasicLit` のときだけ検査していたので、Go で長文を書く定番の
  `fmt.Fprintf(w, "a…"+\n"b…", x)` は `*ast.BinaryExpr` として丸ごと素通り。
  `cmd/waired` に 7 箇所あり、**全部が長い警告文**——#798 (d) と同じ形だった。
  `origin/main` の使い捨て worktree で継続行に `⚠` を注入して確認: **緑のまま通った**。
* **`markerGlyphs` が `ascii.go` の `asciiFolder` の手写しだった。**
  「Kept in sync」というコメントだけで突き合わせが無い。片方に絵文字を足して他方に
  足し忘れると、ガードがその絵文字に対して黙って盲目になる——このガードが防ぐはずの欠陥と同型。
  `ascii.go` 側に `statusMarkFolds` を切り出し、`asciiFolder` をそこから組み、
  ガードはそこから導出するようにした。
* **到達性の下限が `seen < 50`、実測 665。** 「パーサが壊れた」は捕まえるが
  「walk が package の 9 割を見なくなった」は捕まえない。400 に上げた。

### ネガティブ証明の型

PR #936 が「すべての新規ガードは元の欠陥に対して落ちることを見てから残した」と書いている作法に従い、
7 ケースを機械的に回す小さなスクリプトで確認した。**7 番が決め手**——
同じ注入を `origin/main` の使い捨て worktree（`git worktree add --detach`）で回し、
旧ガードが緑で通ることを見た。これが無いと「新しく捕まえた」のか
「元から捕まっていた」のか区別がつかない。

注意: 検証スクリプトの中で `git checkout <ref> -- <path>` を使うと **index に載る**ので、
以後の `git checkout -- <path>` はその版を復元してしまい、残りのケースが全部
旧ガードに対して走る（一度踏んだ）。復元は `git checkout HEAD -- <path>` と明示する。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1103
- https://github.com/waired-ai/waired-agent/issues/798
- `cmd/waired/glyph_format_guard_test.go`
- `cmd/waired/ascii.go`（`statusMarkFolds`）
- `scripts/ci/tray-grey-row-guard.sh`（`// grey: <why>` の先例）
- `scripts/ci/mgmtclientguard/exemptions.go` / `scripts/ci/protoconsumer/exemptions.go`
