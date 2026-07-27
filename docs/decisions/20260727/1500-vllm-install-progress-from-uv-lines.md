---
status: accepted
---

# vLLM インストールの進捗は uv 自身の告知行から実バイトを読む (20260727 15:00)

## Status
Accepted

## Context
vLLM セットアップは `engine_install: running` のまま最大 45 分
(`setupVLLMInstallTimeout`) 無言だった (#255)。#197/PR #243 が ollama に対して
入れた「installer → sink → lease → CP → ウィザード」の機構は既に全部あり、
§7 の `engine_download` 行も `bytes + rate` 建てで確定済み。欠けていたのは
**バイトそのもの**だった。

`InstallProgress.Percent` があるので当初は「percent をどうワイヤに載せるか」が
論点に見えていたが、実測の結果 **percent は存在しなかった**: uv は stderr が
TTY でないと進捗バーを描かず、`DefaultInstallRunner` はパイプを渡す。
詳細は docs/knowledges/20260727/1500-uv-no-progress-without-tty.md。

## Decision
- **proto は変更しない。** ワイヤに percent フィールドは無く、追加しても
  *運搬*の問題しか解けない。*測定*の問題は残る。唯一 proto が正確に運べる
  「stage 3/5」は pip-install が壁時計の 90〜95% を占めるため 30 分以上
  固まり、症状を解消しない。§7 の denomination
  (`engine_download` = bytes+rate / `engine_install` = indeterminate) も
  そのままでよい。
- **uv の告知行を読む。** パイプでも生き残る
  `Downloading <pkg> (506.1MiB)` / ` Downloaded <pkg>` から
  `completed_bytes` / `total_bytes` を積む
  (`internal/runtime/uv_progress.go`)。推定定数は置かない — 数字は
  uv 自身のもの。`rate_bps` は 20 秒の移動窓の平均（完了イベントは
  wheel 単位で塊になるため、隣接 2 点の差分は wheel のサイズを測ってしまう）。
- **`uv` キャッシュのディスク増加をサンプリングする案は却下。** キャッシュは
  展開後サイズでダウンロード量の約 2.3 倍。告知サイズと混ぜると単位が壊れる。
- **ステージ → §7 行の対応**は ollama と同型:
  `pip-install` → `engine_download`、他の 4 ステージ → `engine_install`。
  新しい step id は作らない (`validSetupExecutorStep` は閉じた allow-list)。
- **ターミナルも同時に直す。** stdout レンダラと daemon sink は対等
  (`teeProgress`)。#255 の副次的発見として、ターミナル側も無言だった。

## Consequences
- ウィザードの vLLM セットアップに `2.1 GB / 4.2 GB · 24.1 MB/s` が出る。
  agent だけの変更で、proto / CP / spec / web ラベル表はいずれも無改修。
- 進捗は **wheel 単位で段階的**に進む。最大の wheel は torch 506 MiB
  (全体の ~12%) なので、遅い回線では数分バーが止まりうる。45 分の完全な
  無音よりは桁違いにましで、#130 の keepalive が `last_check` を進め続けるため
  「オフライン」表示にはならない。より滑らかにするには単位の違う信号を
  混ぜる必要があり、正確さと引き換えになるので採らなかった。
- **uv の出力形式に依存する。** 形式が変われば `total_bytes` が 0 のままになり、
  行は今日と同じ indeterminate に劣化する（壊れず、静かに元に戻る）。
  逐語 fixture のテストが変化を検知する契約になっている。
- `internal/download.parseSize` を `ParseSize` として公開した。uv も
  `506.1MiB` 形式なので単位表は 1 つに保つ。
- 付随して判明: コード内の「~6 GB」は誤り。ダウンロードは ~4.2 GB、
  出来上がる venv は ~9 GB。

## Refs
- https://github.com/waired-ai/waired-agent/issues/255
- https://github.com/waired-ai/waired-agent/issues/197 / PR #243（ollama 側の同型）
- docs/knowledges/20260727/1500-uv-no-progress-without-tty.md
