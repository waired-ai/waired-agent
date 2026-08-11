# Windows の rename は「開いているハンドル」で失敗する (20260812 01:20)

## Issue

waired-agent#687（Windows に agent のログファイルを新設）でローテータを設計する際、
「Go の `os.OpenFile` は Windows で `FILE_SHARE_DELETE` を要求しないので、開いたままの
ファイルは rename できない」という前提を置いた。この前提自体は測っていなかったため、
別レーンから CI flake（waired-agent#698）の照会を受けたのを機に実測した。

対象の flake は `rename ...\.state-<n> ...\runtime\state: Access is denied.` で、
原子的書き込み（一時ファイルに書く → 本体へ rename）が失敗するというもの。

## Learnings

WSL 上の実 NTFS で `GOOS=windows` ビルドのプローブを実行した結果（同一プロセス内）:

| ケース | 結果 | エラー |
|---|---|---|
| ソース（動かす側）を開いたまま rename | FAIL | `The process cannot access the file because it is being used by another process.` |
| 宛先（上書きされる側）を**書き込み**で開いたまま | FAIL | `Access is denied.` |
| 宛先を**読み取りのみ**で開いたまま | FAIL | `Access is denied.` |
| 宛先を誰も掴んでいない | OK | — |
| ソースを誰も掴んでいない | OK | — |

要点は3つ。

1. **エラー文字列でどちら側が掴まれているか切り分けられる。**
   `used by another process` ならソース側、`Access is denied` なら宛先側。
   ログにどちらが出ているかで、疑うべきコードが変わる。

2. **読み取り専用オープンでも失敗する。** Go の `os.OpenFile` / `os.Open` は
   Windows で `FILE_SHARE_DELETE` を立てないため、**読み手が1人いるだけで**
   rename による置き換えが弾かれる。「書いている最中だけ危ない」ではない。

3. **これは「たまたま」ではなく構造的なレース。** 読み手と書き手が並走しうる
   コードでは、CI の rerun で通ることがあっても直っていない。rerun 通過を
   ウイルス対策ソフトやインデクサの証拠として扱わないこと — それらを疑うのは
   コード側のレースで説明が付かないと確かめた後にする。

## 波及先

原子的書き込み（tmp に書く → 本体へ rename）を使っている箇所すべてに効く。
Unix では開いたままの unlink / rename が普通に通るため、Linux と macOS の CI では
一切現れない。

waired-agent#687 のログローテータは、この機序を避けるために **rename の前に自分の
ハンドルを閉じる**設計にしている（`internal/platform/logrotate/file.go`）。書き手が
自分自身ひとりで、かつ mutex を保持しているので、閉じている間に取りこぼす行はない。
fd ベースの既存ローテータ（launchd が開いた fd を dup2 で貼り替える方）は rename →
dup2 の順序が必要で、こちらとは要件が逆になっている点に注意。

## Refs

- https://github.com/waired-ai/waired-agent/pull/687
- https://github.com/waired-ai/waired-agent/issues/698
