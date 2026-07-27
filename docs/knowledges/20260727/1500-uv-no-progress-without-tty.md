# uv はパイプ越しに進捗を一切出さない — vLLM インストールの `Percent` は死んでいた (20260727 15:00)

## Issue

waired-agent#255 は「vLLM インストールの進捗はターミナルには出ているのに、
ウィザードには第二の consumer が無いだけ」という前提で立てられていた。
実測するとその前提が誤りだった。**ターミナル側も無言だった。**

`internal/runtime/vllm_install.go` の `DefaultInstallRunner` は uv の
stdout/stderr を `exec.Cmd` の **パイプ** で受ける。uv は進捗バーを
[indicatif](https://github.com/console-rs/indicatif) で描いており、
indicatif は描画先が TTY でないと自身を無効化する
([astral-sh/uv#9129](https://github.com/astral-sh/uv/issues/9129))。
したがって `extractInstallPercent` が探している `NN%` は
**pip-install ステージの全期間で 1 度も現れない**。

`internal/runtime/vllm_install_test.go` の percent fixture
(`"Downloading torch (700 MB) 47%"` など) は実出力ではなく**創作**で、
リポジトリ内に実 uv 出力のサンプルは 1 つも無かった。だから誰も気付かなかった。

## Learnings

### 1. uv には machine-readable な進捗出力オプションが無い

CLI リファレンスにあるのは `--no-progress` / `UV_NO_PROGRESS`（消す方向）、
`--quiet`、`--verbose`（tracing ログでバイト数は無い）だけ。
`FORCE_COLOR` の類も効かない（#9129 で報告済み）。
**強制的に進捗を出させる手段は無い。**

### 2. しかしパイプでも「実サイズ付きのログ行」は出る

uv 0.11.26 で実際の `uv pip install vllm==0.24.0 ...` を両ストリーム
パイプで走らせて採取した出力（抜粋・逐語）:

```
Resolved 190 packages in 2.47s
Downloading torch (506.1MiB)
Downloading nvidia-nvjitlink (38.8MiB)
 Downloaded outlines-core
Downloading apache-tvm-ffi (2.2MiB)
 Downloaded torch
Prepared 190 packages in 1m 20s
Installed 190 packages in 53ms
```

- `Downloading <pkg> (<size>)` — **先頭スペース無し**、サイズは単位直結
  (`506.1MiB`)。転送の**開始**時に出る。
- ` Downloaded <pkg>` — **先頭スペース 1 つ**。転送の**完了**時。
- 両者のパッケージ名の集合は**完全一致**した（63 行 / 63 行）。
- 告知されるのは uv 自身の閾値を超えたものだけ。190 解決中 **63 個**。
  小さい wheel は無言でダウンロードされる。

これが `SetupStep` の `completed_bytes` / `total_bytes` / `rate_bps` を
**推定なしで**埋められる情報源になる。percent はワイヤに存在しないので、
そもそも運べなかった。

### 3. 実測値（uv 0.11.26 / vllm 0.24.0 / Python 3.12 / linux x86_64）

| 項目 | 実測 |
|---|---|
| 解決パッケージ数 | 190 |
| サイズ告知された数 | 63 |
| **告知サイズ合計（= 実ダウンロード量）** | **4,209,403,494 B ≈ 4.2 GB** |
| 最大 wheel | torch 506.1MiB、flashinfer-cubin 426.8MiB、nvidia-cublas 403.5MiB |
| uv キャッシュ最終サイズ（展開後） | 9,615,521,218 B ≈ 9.0 GiB |
| venv 最終サイズ | 9,348,626,293 B ≈ 8.7 GiB |
| キャッシュ内ファイル数 | 70,464 |

コード内コメントの「~6 GB」はダウンロード量でも venv サイズでもない。
**ダウンロードは ~4.2 GB、出来上がる venv は ~9 GB** が正しい。

### 4. キャッシュのディスク増加は「別の単位」

当初は `UV_CACHE_DIR` を固定してディレクトリサイズをサンプリングする案を
検討した。走査コストは問題にならない（70k ファイルで `du -sb` が 0.08 s）が、
**キャッシュは展開後のサイズ**なのでダウンロード量の約 2.3 倍になる。
告知サイズ（ダウンロード量）と混ぜると単位が食い違い、バーは 100% に
届く前に振り切れる。採用しなかった。

### 5. 落とし穴: GNU `du` は 1 回の呼び出し内でハードリンクを重複排除する

`du -sh venv cache` は cache を 120K と報告したが、`du -sb cache` 単独では
1.08 GB だった。uv は cache から venv へハードリンクするので、
**引数の順で先に出てきた方に全バイトが計上される**。
サイズを比較するときは必ず別々に呼ぶこと。

## Refs

- https://github.com/astral-sh/uv/issues/9129
- https://docs.astral.sh/uv/reference/cli/
- https://github.com/waired-ai/waired-agent/issues/255
- internal/runtime/uv_progress.go — このログ行を読む実装
- internal/runtime/uv_progress_test.go — 上記の逐語行を fixture にしたテスト
