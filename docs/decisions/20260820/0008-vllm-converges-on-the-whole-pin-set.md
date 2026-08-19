---
status: accepted
---

# vLLM も update で pin に揃える。判定は版ひとつでなく pin の組 (20260820 00:08)

## Status
Accepted

## Context

#826 が Ollama に converge を入れたとき、vLLM は「未対応」と記録して残した
（`docs/decisions/20260816/2243-update-converges-the-bundled-engine.md` の
Consequences: 「venv の再構築は ~6 GB で、いつ走ってよいかの判断が別途要る」）。
その判断がここである。

残された状態はこうだった。`VLLMPinnedVersion` は**新規インストールが何を入れるか**
しか述べておらず、**すでに入っているホストが何を動かしているか**とは無関係だった。
converge が無く、`waired runtimes upgrade vllm` は
「not supported yet」を返すだけで、ズレを知らせる警告も 1 つも無かった
（`RuntimeStatus` の `PinnedVersion` / `VersionWarning` は ollama の枝でしか
埋まっていなかった）。したがって **vLLM を入れたホストは、セットアップ時の
リリースに無期限に留まり、そのことが製品のどこからも見えなかった。**

### 「古いだけ」ではない

Ollama の pin は #489 の完全一致規則があるので、pin が動けば取り残されたホストは
**serve しなくなる**。vLLM にそういう明示規則は無いが、結果は同じになりうる。

製品が vLLM に渡すものは、pin したリリース自身のレジストリから読み出して書かれて
いる。`cmd/waired-agent/inference_vllm_toolparser.go` は
「vLLM は未登録の `--tool-call-parser` 名を起動時に拒否する。typo はツール呼び出し
だけでなくエンジン全体を失う」と自ら記録しており、`vllm.go` はマップに載っている
モデルなら無条件にそのフラグを渡す。**その名前を知らない古い venv では、当該モデルは
まったく serve できない。** 同じ機構が起動フラグにも当てはまる（未知のフラグは
argparse の exit 2 で、`EnsureRunning` のリトライが全部落ちる）。`TransformersConstraint`
と `VLLMVerifyImports` も同じリリースに合わせて書かれている。

つまり vLLM でも「pin と違う venv は未検証」であり、そこは Ollama と同じ扱いでよい。

### Ollama と同じでない 3 点

1. **converge しても serve は止まらない。** vLLM のインストールは
   `<base>/<version>/.venv` を**新しく作り**、最後に `current` symlink を張り替える。
   使用中の venv は編集されず、失敗しても手つかずで残る。Ollama のバイナリ上書きに
   要る「走っているプロセスは自分の inode を握る」という議論が、こちらには要らない。
2. **ディスクが二重に要る。** 新旧が同時に載るうえ、古いほうを消す仕組みは
   `waired runtimes uninstall` しか無かった。converge を入れるなら回収も要る。
3. **バージョンはディレクトリ名から読める。** サブプロセス不要。ただし
   ディレクトリ名は vLLM の版だけなので、**付随 pin が単独で動いたホストは
   名前の上では最新に見える。**

## Decision

**すでに入っている venv を pin の組に揃える。無いホストには入れない。**

- **判定の単位は「pin の組」**: `VLLMPinnedVersion` /
  `HFTransferPinnedVersion` / `TransformersConstraint` / `VLLMPythonVersion`。
  インストール時にこの組を venv の隣（バージョンディレクトリ内）へ記録し、
  converge はその記録と比較する。ディレクトリ名だけを鍵にすると、
  付随 pin の移動が既存ホストに永久に届かない。
  付随 pin だけの差分は既存ディレクトリへの pip 実行なので torch は再取得されず安い。
- **ただしインタプリタだけは converge が閉じられない**ので、その差分は
  実行せず「保留」と言う（`waired runtimes install vllm` を案内する）。
  理由は下の「実機で判明したこと」。venv を作り直せば閉じられるが、
  作り直しは使用中の環境を消すことであり、converge がやってはならないことである。
  実行しないだけでなく**記録も書き換えない** — 書き換えれば
  「venv が持っていない組」を記録した状態になり、次回以降ズレが見えなくなる。
- **`install` と converge を別の意図として分ける**（`InstallOpts.Recreate`）。
  `waired runtimes install vllm` は「ここに綺麗な環境を置け」なので作り直す。
  converge は「ここにあるものを揃えろ」なので**既存の環境に対して pip を回すだけ**で、
  環境を消さない。
- **記録が無い venv は「ズレの証拠なし」として放置する**（#843 以前の全ホスト）。
  ファイルが無いことを理由に ~6 GB を再構築するのは、当のホストが悪いことを
  何もしていないのに課される費用になる。vLLM の版が pin と違えば従来どおり発火する。
- **完全一致。上げるだけでなく下げる。** 判定は「pin より古いか」ではなく
  「pin と違うか」。pin は巻き戻ることがあり、この版のパーサ表と起動フラグは
  pin したリリースから採ったものなので、新しすぎる venv も同じだけ未検証である。
- **比較は文字列一致で、`internal/version` を通さない。** Ollama がパーサを通すのは
  `ollama --version` が人間向けの散文を出すからで、こちらは両側とも製品が書いた
  文字列（ディレクトリ名＝要求した版、記録＝入れた定数）である。吸収すべき表記ゆれが
  無いばかりか、吸収すると害になる — PyPI の post リリース（`0.24.0.post1`）は
  dotted core 比較が捨てる部分でしか base と違わず、そこへ pin を動かすのはたいてい
  何かを直すためである。
- **経路は #826 と同じ 2 つ。** インストーラの update モード
  （`common_converge_engine`、Linux のみ — 他 2 OS に converge する vLLM は無い）と、
  デーモン起動時のバックグラウンド。両者は同じ純粋関数
  （`DecideVLLMConverge`）と同じ orchestration（`ConvergeVLLM`）を通り、
  install の実体だけが seam で差し替わる。
- **空き容量の事前確認を入れ、足りなければ「保留」と言う。**「やることが無い」と
  「やるべきだが今はできない」を同じ顔にしない（`Blocked`）。**読めなかった場合は
  通す** — statfs の失敗は「ディスクが一杯である」証拠ではないし、install 自身が
  ENOSPC を持っている。
- **成功して symlink を張り替えたあとにのみ、旧 venv を回収する。** 先に消すと、
  install が失敗したときに消えるのは「まだ serve に使っている venv」になる。
  回収の失敗は converge の失敗ではない（エンジンはどちらでも pin に在る）ので、
  結果は別建てで報告する。
- **ズレを見えるようにする。** `RuntimeStatus` に ollama と同じ
  `PinnedVersion` / `VersionWarning` を出す。converge が直すのが本筋だが、
  converge が走れなかったホストが黙るのは #843 が指摘した状態そのものである。
  誘導先は `runtimes install vllm` ではなく converge の動詞
  （前者は ~6 GB の確認を尋ね直すが、その問いはとうに答えられている）。

## 実機で判明したこと（この決定を変えた2件）

sv-mag (RTX PRO 4000 Blackwell) に実際に venv を作って converge を回したところ、
**設計の前提が2つとも偽**だった。どちらもユニットテストでは出ない
（fake runner が `uv venv` を既存ディレクトリに対して成功させていたため）。

1. **`uv venv` は既存の環境に対して失敗する。** `A virtual environment already
   exists at ...` で exit 2、`--clear` を使えというヒント付き。`vllm_install.go`
   のコメントは「同じインタプリタなら uv は成功して何もしないので冪等性がタダで
   手に入る」と書いていたが、この製品が解決する uv ではそうならない。
   → **同じバージョンディレクトリへの再入は必ずここで落ちていた**
   （`waired runtimes install vllm` の再実行も含む）。
2. **`maybeRollback` が「自分が作っていないディレクトリ」を消していた。**
   1 の失敗を受けて `os.RemoveAll(versionDir)` が走り、**動作中の venv が消え、
   `current` がぶら下がったリンクになった**。実機で実際にこの状態を作ってしまった。
   「half-built な venv を消して次回を綺麗にする」という doc の主張は、
   *その呼び出しが half-build したもの*にしか当てはまらない。

→ 「converge は使用中の環境に触らない」という本決定の中心的な主張は、
**この2つを直すまで成立していなかった**。`Recreate` の導入と
「作っていないものは消さない」規則はここから来ている。
インタプリタ差分を保留にするのも同じ理由 — 閉じるには作り直しが要るためである。

3. **uv は cwd から上へ設定ファイルを探す**ので、インストーラは
   「呼び出した人がいたディレクトリ」の影響を受けていた。サービスユーザーで
   実行すると `failed to open file /home/<someone>/uv.toml: Permission denied`
   で落ちる。中立な cwd からなら同じコマンドが完走することを実機で確認し、
   **特権ではなく cwd の問題**だと確定させた。systemd 配下では
   `WorkingDirectory` が `/` なのでデーモン経路には出ないが、
   `sudo waired runtimes install vllm` を任意のディレクトリで叩く人には出る。
   `uv.toml` が在るディレクトリからなら、**誰も意図していない設定で解決される**。
   → `UV_NO_CONFIG=1` を全ステージの env に足した（実機の uv で塞がることを確認）。
   ここで作る venv は上の引数だけで定義される。

## Consequences

* **vLLM を入れた Linux ホストでは、pin を動かす update が長くなる。**
  約 4 GB のダウンロードと 5〜15 分。インストーラ経路では利用者が待つ側にいるので
  段階進捗が出る。デーモン経路は起動をブロックせず背景で走り、効くのは次の
  エンジン起動から。**vLLM を入れていないホストには何も起きない**
  （symlink を 1 回読むだけ）。
* **`VLLMPinnedVersion` を動かす意味が変わった。** 以前は「新規インストールが
  何を入れるか」だったが、いまは**フリート全体が次の update で再構築される**。
  Renovate の vllm バンプに、パーサ名と起動フラグが新リリースにも在ることを
  確認させる注記を付けた（どちらも欠けるとエンジンが起動しない）。
* **pin の定数が 1 か所になった**（`internal/runtime/vllm_pins.go`、ビルドタグ無し）。
  `VLLMPinnedVersion` は Linux 実装と darwin / windows のスタブに 3 つ複製されて
  いた。converge はどの OS でも組を必要とするので、複製を残す理由が消えた。
  Renovate の `managerFilePatterns` も 3 → 1 になった。
* **`.failed-<ts>` は名前で除外する。** `maybeRollback` はディレクトリを
  `.venv` ごと rename するため、形はバージョンディレクトリと区別がつかない。
  `KeepFailed` で明示的に残したものを、回収処理が勝手に「検分済み」と決めない。
* **`<base>/python` を消してはいけない。** #778 が uv の管理インタプリタを
  BaseDir 配下へ移したのは共有するためで、回収対象の判定は「`.venv` を持つこと」に
  している。
* **`waired runtimes upgrade vllm` が動くようになった**（従来はエラー）。
  確認プロンプトは出さない — インストーラが非対話で呼ぶ経路であり、
  判定そのものが確認だからである。

## Refs
- https://github.com/waired-ai/waired-agent/issues/843
- https://github.com/waired-ai/waired-agent/issues/826
- https://github.com/waired-ai/waired-agent/issues/778
- https://github.com/waired-ai/waired-agent/issues/410
- docs/decisions/20260816/2243-update-converges-the-bundled-engine.md
