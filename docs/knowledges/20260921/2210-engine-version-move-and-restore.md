# 同梱エンジンの版が動く差し替えを元に戻すと残るもの (20260921 22:10)

## Issue

自前ビルドに差し替えて同梱エンジン(ollama)の pin が動くと、起動時の版合わせがエンジンを入れ替える。元のビルドに戻した後も、元に戻らないものと新しくできたものが残る。

2026-09-21 に Linux・Windows・macOS の 3 台で、ollama を 0.33.3 → 0.34.2 → 0.33.3 と往復させて確かめた(#1512・#1514 の実機確認)。

## Learnings

**ビルドの作り方**
- 版合わせそのものを見る検証は、main からビルドする。
- ホストの版に修正だけを cherry-pick したビルドは pin を運ばない。そのため版合わせの経路が走らない(起動待ち、インストールロック、入れ替え)。

**runtime の復元**
- runtime の `bin` は退避から戻すのが確実。Windows は `runtimes\ollama\bin`、ほかは `runtimes/ollama/bin`。
- 元のビルドの起動時の版合わせに任せる方法もある。その場合、ダウンロードが 1 回増える。さらに、戻るかどうかがそのビルドの版合わせの実装次第になる。

**差し替えで新しくできるもの**
- ollama 0.34.x は、最初の起動で `runtimes/ollama/models/metadata/` を作る(モデルごとの JSON)。
  - 0.33.3 はこれを読まないので、害はない。原状に戻すなら消す。
  - 見つけ方: `models` の直下の項目数と mtime を前後で比べる。
- 検証ビルドは `runtimes/ollama/.install.lock` を作る(#1512 のロック)。Linux と macOS はファイル、Windows では作らない。

**Windows の速度キャッシュ**
- 場所は `C:\WINDOWS\system32\config\systemprofile\.cache\waired\bench.json`(サービスが LocalSystem で動くため)。
- このファイルは schema の版を持つ。2026-09-21 時点で、main は v5、09-13 の edge は v4 だった。
- 版の合わないビルドはこのファイルを無視して計り直し、書き換えることがある。差し替えの直後に退避しておく。

**engine.log**
- 起動のたびに 1 世代だけ残してローテートされる(`engine.log.1`)。
- そのため、差し替えと復元で 2 回起動すると、差し替え前の engine.log は残らない。要るなら先に写す。

**macOS の最初の起動**
- 版合わせの直後の最初の起動では、ollama 自身の GPU 検出が 30 秒の watchdog で打ち切られた(`llama-server GPU discovery watchdog timed out`)。
  - そのプロセスの間、エンジンは自分を CPU として扱った(`total_vram="0 B"`、`reason=cpu` で mmap 無効)。
  - それでも、読み込んだモデルは Metal で動いた。
- 次の起動では、同じファイルで 0.3 秒で検出できた。Windows では 1 回目の起動でも 1.6 秒だった。
- ~~原因は確かめていない。~~
- 検証で GPU を判定するときは、この 1 回目を外して 2 回目の起動で見る。
- #1514 以降、この打ち切りは `engine_discovery_error` として agent のログの WARN に出る。

**訂正(20260922):** 原因は確かめた。
- LaunchDaemon の plist が `ProcessType=Background` だったため、engine が起動する llama-server の Metal shader の compile が background QoS で走り、30 秒を超えた。
- 詳細は `docs/decisions/20260922/0230-launchdaemon-runs-as-a-standard-job.md`(#1521)。
- 検証で cache を冷やすには、root の `$(getconf DARWIN_USER_CACHE_DIR)com.apple.metal` と `com.apple.metalfe` を消す。版を動かさなくても再現する。

## Refs
- https://github.com/waired-ai/waired-agent/pull/1512
- https://github.com/waired-ai/waired-agent/pull/1514
