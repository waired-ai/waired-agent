---
status: accepted
---

# macOS の LaunchDaemon を ProcessType=Standard で動かす (20260922 02:30)

## Status
Accepted

## Context

waired-agent#1521:
- 同梱の ollama の版が変わった直後の最初の起動で、ollama 自身の GPU 検出が 30 秒の watchdog で打ち切られた(Mac mini M4)。
- エンジンは GPU を 0 台と判断し、次の再起動まで CPU として動いた(`total_vram="0 B"`、mmap 無効)。

原因(2026-09-22 に M4 Mac mini で確かめた):

- **ollama 側の仕様**(v0.34 の `discover/runner.go`):
  - GPU 検出は llama-server を起動し、`system_info:` の行が出るのを 30 秒だけ待つ。Windows は 90 秒、他の OS は 30 秒で固定。
  - 時間切れのときは llama-server を kill する。再試行はせず、検出の結果はプロセスの間そのまま使う。
- **何に時間がかかっていたか**:
  - llama-server は起動時に ggml の Metal shader を source から compile させる。compile するのは macOS の `MTLCompilerService` で、依頼元の QoS で走る。
  - cache が冷えていると、この compile が 30 秒を超えた。kill された依頼の途中経過は残らないので、すぐ再起動しても同じ所で打ち切られた(実機で確認)。
- **遅くなる原因は plist の設定だった**:
  - waired の LaunchDaemon の plist は `ProcessType=Background` だった。
  - agent が起動するもの(ollama serve、llama-server)はすべてこの class を継ぎ、background QoS で動いていた。
  - 同じ bundle、同じ冷えた cache でも、端末から手で起動した llama-server は 12 秒で検出を終えた。plist を Standard にすると、product の起動でも 12〜14 秒で終わった。
- **設定の由来**: Background は、repo の最初の取り込み以来のもの。「App Nap を避ける」という理由が書かれていたが、App Nap はアプリの仕組みで、LaunchDaemon には当てはまらない。
- **他の OS**: Linux の systemd unit も Windows のサービスも、優先度を下げる設定を持たない。

## Decision

オーナー決定(2026-09-22、#1521)。LaunchDaemon の plist の `ProcessType` を `Standard` にする。

- **Standard の意味**: キーが無い場合と同じ。launchd は軽い制限をかけ、実機では engine が utility QoS で動いた。前面のアプリ(user-initiated や default)より下の優先度のままである。
- **Interactive を選ばない理由**: Apple は「応答性がこれに依存するときだけ」使うものとしている。今回はそれに当たらない。
- **既存のインストール**: 旧版からの更新経路は考えない(リリース前、オーナーの指示)。plist は新規インストールと `waired-agent install` で書かれる。
- **採らなかった案**:
  - **打ち切りを見たら、すぐ 1 回だけ再起動する**(最初にオーナーが選んだ案): 実機で効かなかった。2 回目の起動も 30 秒で打ち切られた。
  - **モデルの読み込みを待ってから再起動する**: 症状は止まるが、初回の読み込みが 5 倍遅いまま残る。
  - **起動の前に llama-server を 1 回空で走らせる**: ollama 内部の引数に依存する。原因の QoS も残る。

## Consequences

計測は各条件 3 回、中央値。他に動いているものは無い状態(共有は off)。電力は、生成の 3 秒後から 1 秒ごとに 5 回採った `powermetrics` の平均。

| | M4 Mac mini 16 GB (qwen3.5-4b) Background → Standard | M5 Pro MacBook Pro 48 GB, AC 電源 (qwen3.6-35b-a3b) Background → Standard |
|---|---|---|
| 冷えた cache での GPU 検出 | 30.4 s、3/3 回打ち切り → 14.2 s、0/3 | 30.3 s、3/3 → 12.1 s、0/3 |
| 冷えた cache でのモデル読み込み | 121.5 s → 24.3 s | 103.1 s → 24.3 s |
| 温まった cache でのモデル読み込み | 7.36 s → 2.37 s | 7.22 s → 1.84 s |
| 生成速度 | 26.0 → 26.8 tok/s | 59.8 → 77.7 tok/s |
| 前面の CPU 負荷と同時の生成速度 | 21.3 → 22.4 tok/s | 39.8 → 47.5 tok/s |
| 前面の CPU 負荷(生成と同時、単独比) | 10.25 s / 9.84 s → 10.11 s / 9.90 s | +15 % → +16 %(単独値そのものが回の間で 15 % ずれた) |
| 生成中の合計電力 | 8.75 W → 9.23 W(+5.5 %、1 token あたり +3 %) | 28.2 W → 38.9 W(+38 %、1 token あたり +8 %) |

- **初回の検出**: 打ち切られなくなった。モデルの読み込みは 3〜5 倍速くなった。
- **前面のアプリ**: 生成中の CPU 負荷の遅れ方は、ProcessType で変わらなかった。
- **代償は生成中の電力**:
  - 生成が速くなった分に加え、1 token あたりでも M4 で +3 %、M5 Pro で +8 %。
  - ノート型の Mac をバッテリーで使うと、持ち時間が縮む見込みがある(バッテリー駆動での計測はしていない)。
- **macOS のセキュリティ側の反応**:
  - Background Task Management の記録は `[enabled, allowed]` のまま。拒否も無効化も起きなかった。
  - launchd の `spawn type` は `background (5)` から `daemon (3)` になった。
  - plist を書き換えると、BTM は項目を登録し直し、ログイン中の利用者に通知(「バックグラウンド項目が追加されました」)を出す準備をした。新規インストールで出る通知と同じもの。
- **検出の照合**: #1514 の照合(打ち切られた検出は WARN の `engine_discovery_error` に出る)は、このまま残す。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1521
- https://github.com/waired-ai/waired-agent/pull/1514
- `internal/platform/service/service_darwin.go`(`renderLaunchDaemonPlist`)
- `docs/knowledges/20260921/2210-engine-version-move-and-restore.md`(最初の観測)
- launchd.plist(5) の `ProcessType`
