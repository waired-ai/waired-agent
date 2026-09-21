---
status: accepted
supersedes:
  - docs/decisions/20260816/2243-update-converges-the-bundled-engine.md
---

# 同梱エンジンの最初の起動は、起動時の更新が終わってからにする (20260921 19:37)

## Status

Accepted。waired-agent#1511。
`docs/decisions/20260816/2243-update-converges-the-bundled-engine.md` の 2 か所を置き換える。

- Decision の段落「デーモン側は動いているエンジンを止めない。ディスク上のバイナリを置き換えるのは実行中でも安全で…」。
- Consequences の「server 行が優先なので、これまで正しかった答えは変わらない」。

2243 の残り(完全一致の pin、無いホストには入れない、2 つの経路)は変えない。

## Context

2243 は、デーモン起動時の更新処理が走っているエンジンの下で同梱の ollama を入れ替えても安全だとしていた。
根拠は「走っているプロセスは自分の inode を握る」ことだった。これは `ollama` の実行ファイル 1 つについては正しいが、エンジン全体については正しくなかった(コードを読んで判明)。

- **runner と lib は読み込みのたびに開かれる。** `ollama serve` はモデルを読み込むたびに、自分の実行ファイルの場所から runner を起動し、`lib/ollama` を読む(ollama v0.34.2 `ml/path.go` の `LibOllamaPath` は `os.Executable` から決まり、Linux の Go は `(deleted)` を外すので、入れ替え後は新しいパスを指す)。
- **入れ替えの途中や後に読み込むと壊れる。** 入れ替えの途中の読み込みは `lib/` の欠けた木を見る。入れ替えの後の読み込みは、古いサーバーの下で新しい版の runner と lib を動かす。
- **Windows は戻らない。** 入れ替えは古い中身を名前順に消すので `lib` が先に消え、実行中でロックされた `ollama.exe` の削除で失敗し、巻き戻さなかった。
- **デーモンは pin と違うエンジンも起動する。** `ExpectedVersion` は、ポート衝突時に孤児を引き取るかどうかの判断にしか効かない。そのため `apt upgrade` の後は、古いエンジンの起動と入れ替えが同時に走る。

同じ調査で、起動時の更新処理にあと 3 つの問題が見つかった。

- **版の読み違い。** `--version` は `OLLAMA_HOST` の既定(`127.0.0.1:11434`)のサーバーに版を聞く。waired のエンジンは別のポートなので、答えるのは利用者自身の Ollama である。その版を同梱エンジンの版と読むと、起動のたびに pin を落とし直す。
- **ROCm オーバーレイが消える。** デーモンの更新処理はオーバーレイを取らず、`lib/` を丸ごと置き換えていた。そのため AMD ホストでは、pin が動くたびに ROCm が消えた。
- **作業ディレクトリの共有。** apt 経路では、デーモンの更新処理と install.sh の CLI の更新処理が同時に走り、同じ `.stage` を使っていた。

## Decision

1. **最初の起動は、起動時の更新処理(ollama の分)が終わるまで待つ。** adapter の `StartGate` に待たせる。
   - 起動直後はまだ何も応答していないので、止めるものが無い。
   - pin に合っているホストでは、待つのは `--version` 1 回分だけである。
   - pin から外れたホストは、ダウンロードの間ローカル推論が遅れる。ただし、外れたエンジンはもともとこの版が約束するものを出せない(2243 の完全一致)。
   - 更新が失敗したら門を開け、今までどおり古いエンジンで起動する。
   - 待っている間に Stop / Park されても、エンジンの失敗には数えない(`ErrEngineStartHeld`)。
   - 別案として、次の起動まで置いておいて入れ替える、あるいは空いたときにエンジンを再起動する、も考えた。前者は新しい版が次の再起動まで(何日も)届かない #826 の形に戻る。後者は応答中のエンジンを無断で落とす。どちらも採らない。
2. **版は実行ファイル自身の行で読む。** `Warning: client version is X` が出ていればそれを採る。この行は、サーバーの版と違うか、サーバーが無いときに出る。
3. **オーバーレイを取るかどうかは 1 つの関数で決める**(`setup.OllamaROCmOverlayWanted`)。CLI の 3 OS もデーモンもこれを使う。`WAIRED_OLLAMA_GPU_MODE` は今までどおり Windows でだけ読む。
4. **インストールはファイルロックで直列にする**(`<BaseDir>/.install.lock`)。Linux と macOS は flock。Windows は install.ps1 が更新の前にサービスを止めるので重ならず、ロックは持たない。更新処理はロックを取ったあとに版を読み直す。
5. **入れ替えは途中の失敗から戻す。** 古い中身は消さずに横へ動かし、失敗したら元に戻す。Windows は実行中の exe の名前変更は許すので、横へ動かす段は通る。

## Consequences

- `apt upgrade` で pin が動いたあとのローカル推論は、新しい版のダウンロードが終わってから始まる。docs-site の update ページ(en / ja)に 1 文足した。
- 手で `waired runtimes install ollama` を実行すると、動いているエンジンの下で入れ替わりうる。macOS の `darwin_update` にも、エージェントの再起動までの数秒がある。この 2 つは残る。入れ替え自体は 5 のとおり戻せる形になった。
- `TestParseOllamaVersion_ServerLineWins` は反転した(`TheBinarysOwnVersionWins`)。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1511
- `docs/decisions/20260816/2243-update-converges-the-bundled-engine.md`
- `docs/decisions/20260820/0008-vllm-converges-on-the-whole-pin-set.md`(vLLM 側の同じ型、#1431)
- ollama v0.34.2 `cmd/cmd.go` の `versionHandler`、`ml/path.go`
