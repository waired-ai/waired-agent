# Windows のバージョンリソースは書き手が 2 つあり、規約が食い違う (20260905 20:00)

## Issue

waired-agent#1209(出荷 PE の VERSIONINFO 欠落)で、Go 側(goversioninfo)と
Inno Setup 側の両方にバージョンリソースを入れた。**同じ「VERSIONINFO」でも
この 2 つは書き方が違い**、片方の規約で読むともう片方は空に見える。読み書き
どちらでも踏む場所なので残す。

## Learnings

### 1. Inno の `VersionInfo*` は `AppVersion` から既定されない

`jrsoftware/issrc`(tag `is-6_7_3`)の `Projects/Src/Compiler.SetupCompiler.pas`
で確認した既定の導出(`:8240-8292`):

| ディレクティブ | 既定 | 型 |
|---|---|---|
| `VersionInfoVersion` | **既定なし = 0.0.0.0 のまま** | 数値 4 つのみ |
| `VersionInfoProductVersion` | `VersionInfoVersion` | 数値 4 つのみ |
| `VersionInfoTextVersion` | `VersionInfoVersion` に渡した**生の文字列** | 自由文字列 |
| `VersionInfoProductTextVersion` | `AppVersion`(ProductVersion 未指定時) | 自由文字列 |
| `VersionInfoDescription` | `AppName + " Setup"` | 文字列 |
| `VersionInfoProductName` | `AppName` | 文字列 |
| `VersionInfoCompany` | `AppPublisher` | 文字列 |

数値側は `StrToVersionNumbers` を通し、通らなければ `Invalid`
= **コンパイルエラー**(`:3313-3327`)。`0.0.3-rc1` のような semver をそのまま
書くとビルドが落ちる。よって semver を渡す `AppVersion` とは**別の define**が
要る。逆に、**指定しなければ黙って 0.0.0.0 のまま出荷される** — これが #1209
で 4 本目の出荷 PE(`WairedSetup-*.exe`)が見落とされていた理由。

### 2. 文字列ブロックの言語 ID が生成系で違う

- **goversioninfo**: `040904B0`(US English + Unicode)
- **Inno Setup**: `000004b0`(language-neutral)。
  `Projects/Src/Compiler.ExeUpdateFunc.pas:634-640` が全キーをこれで書く

`VerQueryValue` に `\StringFileInfo\040904B0\FileDescription` を決め打ちで
渡すと、**Inno が作った exe からは何も返らない**(逆も同じ)。正しくは
`\VarFileInfo\Translation` を先に引いて langID+charset を組み立てる。
`installtest-windows.ps1` の `[Waired.VerInfo]::Str` はその形にしてある。

### 2b. Inno は文字列を「その場で上書き」するので空白が付く

`UpdateStringValue`(同 `:634-640`)はテンプレート Setup.exe のリソースを
**リサイズせず上書き**する。元のスロットより短い値を書くと**残りが空白で
埋まる**。実測(run 33961906715): `FileDescription` が `Waired Setup` +
空白 48 個、`FileVersion` が `0.0.0-c30cfe4` + 空白 7 個。

Explorer もその空白ごと表示する。`.iss` 側に直すところは無いので、
**読む側が末尾の空白と NUL を落とす**。これを知らずに完全一致で比較すると、
誰も書いていない値と突き合わせることになる(このリポジトリでは実際に 1 回
落ちた)。

### 3. キーの綴りも 1 つ違う

Inno は `OriginalFileName`、Win32 の慣行は `OriginalFilename`。
goversioninfo は後者。両対応にするか、そのキーで主張しないかのどちらか。

### 4. `VerQueryValue` の長さの単位はブロックによって違う

- 文字列サブブロック → **文字数**(だから NUL を落とすのは `len - 1`)
- `\VarFileInfo\Translation` → **バイト数**(4 バイトずつ進める)
- `\`(VS_FIXEDFILEINFO)→ バイト数。`dwFileVersionMS` はオフセット 8

### 5. goversioninfo v1.5.0 の細かい挙動

- 数値は `-ver-major/-minor/-patch/-build`、文字列は `-file-version` /
  `-product-version`。**両方渡せる**ので、固定ブロックに 4 数値・文字列ブロック
  に semver をそのまま、という分担ができる
- `versioninfo.json` の **空文字列の値はリソースに書かれない**
  (`LegalCopyright: ""` はキーごと消える)。生成物を走査して照合する側は、
  空値のキーを「在るはず」として扱ってはいけない
- `.syso` は COFF オブジェクト。magic は `64 86 01 00` で、
  `scripts/ci/tracked-binary-guard.sh` の ELF/PE/Mach-O のどれにも当たらない
  ため、追跡されていても素通りする

### 6. `.syso` のファイル名接尾辞はビルド制約

`resource_windows_amd64.syso` は **windows/amd64 にしかリンクされない**。
windows/arm64 のターゲットを足した日に、そのビルドだけ名前も版数も無い PE に
なる — しかも何も言わない。`scripts/install/windows_versioninfo_test.go` は
Makefile の `GOOS=windows GOARCH=<arch>` を走査して、その日に落ちるように
してある。

### 7. 生成は「同じターゲットの兄弟」ではなく「ビルドの前提」に置く

`make` は**同一ターゲットの prerequisite の順序を `-j` の下で保証しない**。
`dist-windows-installer: versioninfo build-agent-windows` と書くと、並列時に
`go build` が先に走って**版数の入っていない `.syso` をリンクしうる**。
`build-agent-windows: versioninfo` にすれば順序が決まる。失敗の形が
「0.0.0.0 で出荷される・誰も気づかない」なので、確率的な配線にしてはいけない。

### 8. 生成物をコミットしているのに、生成し直したか誰も見ていなかった

`cmd/waired-tray/resource_windows_amd64.syso` は #59 以来コミットされて
いたが、`go generate` を回す CI ステップも `git diff --exit-code` 型の検査も
無い。`versioninfo.json` を編集して再生成し忘れれば、**古い文字列が永久に
出荷される**。照合は生成器を呼ばずにできる — `.syso` を UTF-16LE として走査
すればキーと値がそのまま並んでいる。ただし**集合で照合してはいけない**:
`FileDescription` を `waired` に書き換えても、その文字列は `InternalName` の
値として既にリソースの中に在るので通ってしまう(実際に通った)。**キーの次に
値が来る隣接**で見る。

### 9. 「.NET はこのリソースを空に読むことがある」は再現しなかった

`cmd/waired-tray/versioninfo.go` は waired#810 以来
「`System.Diagnostics.FileVersionInfo` が同じリソースを空に読むことがある —
既知の wrapper の癖」と書いていたが、**測られていなかった**。
run 33961906715(windows-latest)で 3 本すべてについて Win32 と .NET を
並べて読んだ結果、**両者は一致した**(`Waired (CLI)` / `Waired Agent` /
`Waired`)。出荷物の性質ではない。

一致しない日は外部ツールの大半が空を見ることになるので、この比較は
記録ではなく**アサート**にしてある。

### 10. ハッシュを軸にした対照実験では、リソースを変えると build ID も動く

L99 の #1191 H3 用に「リソース有り/無し」のペアを同一コミット・同一 flag で
焼いたとき、差分は `.rsrc` だけだと報告したが**誤り**だった。先方の
セクション照合では `.text` も 79 バイト違い、中身は **Go の build ID** だった
(`.syso` がリンク入力に入るため)。13 MB 中 79 バイトなので第 2 変数としては
無意味だが、**「1 軸だけ違う」と書くときは build ID を数えていないか確認する**。

関連して: mark-of-the-web は代替データストリームなので**内容ハッシュが素の版と
同一**になり、端末側のハッシュ単位キャッシュに同じ答えを食わされる。
同じ窓の中では独立した軸として成立しない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1209
- https://github.com/waired-ai/waired-agent/pull/1222
- https://github.com/jrsoftware/issrc/tree/is-6_7_3
- docs/decisions/20260822/2216-sac-signing-requirement-is-testable.md
