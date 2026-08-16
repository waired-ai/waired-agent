---
status: accepted
---

# update は同梱エンジンを pin に揃える。無いホストには入れない (20260816 22:43)

## Status
Accepted

## Context

`waired update` はエンジンに触らない設計だった。CLI のヘルプが
「Ollama is notify-only」と述べ、`install.sh` / `install.ps1` は
「Ollama: managed separately; not modified by update.」と出すだけだった。

これが問題になるのは、**pin が完全一致判定**だからである。#489 が
「waired が応答に使う Ollama は waired 自身が管理するものだけ」と決め、その
Consequences が「版数警告は完全一致のみになった」と記録している。
`ollamaVersionWarning` は `live != OllamaPinnedVersion`。したがって
**pin が動くたびに、更新を取り込んだ全ホストが必ず取り残される。**

v0.0.3-rc2 で実際に起きた。ollama 0.31.1 は qwen3.8 を pull すらできない
（レジストリが 412 を返す）ので pin を 0.32.13 に上げたが、更新を当てた
dogfood ホストではこうなった:

```
● qwen3.6-27b   ✓ fits · 1.1 GB of context cache in system RAM
  qwen3.8-27b   ✗ needs ollama ≥ 0.32.13 (running 0.31.1)
```

モデルは出荷され、ホストは更新を取り込み、それでも使えない。解消には
`waired runtimes install ollama` と agent 再起動を手で叩く必要があった。

### 経路は 2 つ要る

トレイは 3 OS とも `waired update --yes` を elevation 付きで実行している
（`actions_linux.go:206` / `actions_darwin.go:107` /
`actions_windows.go:100`）ので、update 経路を直せばトレイも直る。

一方 **Linux の素の `apt upgrade waired` は update 経路を通らない。**
Linux の `waired update` の実体は apt であり、`.deb` の postinst は
アップグレード時に agent を restart するがエンジンのことは知らない。
APT リポジトリを提供している以上、この経路は普通に使われる。

## Decision

**すでに入っているエンジンを pin に揃える。無いホストには入れない。**

- **入れない理由**: エンジンを入れるのは `waired init` だけ（#138）。
  「このパソコンでモデルを動かすか」を訊いてから入れる設計であり、
  更新を契機に 1.4 GB が降ってきてその問いに勝手に答えてはならない。
  `--skip-ollama` のホストは更新しても何も落ちてこない。
- **オプトアウトは作らない**（オーナー裁定 20260816）。完全一致 pin の下では
  pin と違うエンジンはすでに壊れている。選べるようにする対象ではない。
- **上げるだけでなく下げる**。pin は巻き戻ることがあり（エンジンのリリースが
  取り下げられた場合）、agent の規則は完全一致なので、下げる converge も
  同じだけ必要。判定は「pin より古いか」ではなく「pin と違うか」。
- **バージョンが読めないエンジンは入れ直す**。読めない以上は判断できず、
  そのエンジンは何も serve していない。

実装は 2 か所、方針は 1 か所:

1. **インストーラの update モード**（`common_converge_engine` /
   `Converge-Engine`）。バイナリ入れ替えの後・サービス再起動の前に走らせるので、
   サービスは揃ったエンジンで上がる。elevation は update がすでに持っている。
2. **デーモンの起動時 converge**（`startEngineConverge`）。1 を通らない経路の
   受け皿。elevation は不要 — エンジンディレクトリはサービスユーザー所有
   （`/var/lib/waired/runtimes/ollama` が `waired:waired`、ユニットは
   `User=waired`）で実機確認済み。

方針そのもの（`DecideOllamaConverge`）は `internal/runtime` に純粋関数として
1 つだけ置き、両者が同じ表を通る。orchestration も 1 つ
（`ConvergeOllama`）で、install の実体だけを seam で差し替える — CLI 側は
進捗描画・ROCm オーバーレイ・state dir の所有権返却を持ち、デーモン側は
素の取得だけだから。

**デーモン側は動いているエンジンを止めない。** ディスク上のバイナリを
置き換えるのは実行中でも安全で（走っているプロセスは自分の inode を握る）、
アダプタの `BinaryResolver` は `EnsureRunning` ごとに引かれるので次の
エンジン起動で拾われる。利用者が待っている経路（1）はどのみちサービスを
再起動するので即座に効く。ここでの選択は「遅れて揃う」か「応答中かもしれない
エンジンを無断で落とす」かであり、前者のほうが害が小さい。

## Consequences

* `waired runtimes upgrade <engine>` が増えた。`install` とは別の動詞なのは、
  答える問いが違うから — `install` は「ここにエンジンを置け」、`upgrade` は
  「ここのエンジンをこの版に揃えろ」であり、エンジンが無いホストで差が出る。
  vllm は未対応（venv の再構築は ~6 GB で、いつ走ってよいかの判断が別途要る）。
* **`hardware.ParseEngineVersion` が停止中エンジンのバージョンを読めていなかった。**
  `ollama --version` の出力はサーバが応答しているかで変わる: 応答していれば
  `ollama version is X`、していなければ
  `Warning: could not connect to a running Ollama instance` +
  `Warning: client version is X`。パーサは前者しか見ていなかったので、
  **停止中のエンジンは製品のどこからもバージョンが見えなかった**。
  実機で 0.31.1 と 0.32.13 の両方を採取して確認し、client 行も読むようにした
  （server 行が優先なので、これまで正しかった答えは変わらない）。
  既存テストはこの 2 行を 1 つの出力に並べたフィクスチャを「実際の出力」と
  称して置いており、実バイナリはその形を出さない — 手書きの出力を実応答として
  載せた例。実測値に差し替えた。
* インストーラの converge は**失敗しても update は成功扱い**。GitHub が遅い
  という理由で更新が巻き戻るほうが、警告を 1 本残して終わるより悪い。
  警告文言は製品がすでに出しているものと同じ。
* ローカル推論のオン/オフではゲートしない。トグルは再起動なしで変わる
  実行時状態（#465）で、pin と違うエンジンはオンにした瞬間に serve できない。
  ダウンロードが走るのは「すでにエンジンが入っている」ホストだけであり、
  それ自体がこのホストがモデルを動かすと決めた記録になっている。

## Refs
- https://github.com/waired-ai/waired-agent/issues/826
- https://github.com/waired-ai/waired-agent/issues/489
- https://github.com/waired-ai/waired-agent/issues/138
- https://github.com/waired-ai/waired-agent/issues/823
- docs/decisions/20260804/1941-waired-managed-engine-is-the-only-source.md
