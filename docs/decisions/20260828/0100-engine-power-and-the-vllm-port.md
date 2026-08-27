---
status: accepted
---

# エンジン電源は「動いていない」を言い分け、vLLM は waired 所有ポートに移る (20260828 01:00)

## Status

Accepted。オーナー裁定 (2026-08-27, waired-ai/waired#1283 L75)。
`docs/decisions/20260821/1308-engine-power-is-per-engine.md` の続き —
あちらは軸が**どのエンジンに効くか**を決めた。ここで決めるのは
**その軸が何を報告するか**と、vLLM がどのポートに立つか。

## Context

0.0.3-rc4 の実機検証 (Linux + NVIDIA、ウィザードから vLLM opt-in、
:8000 を docker コンテナが占有) で 3 つの判断が必要になった。

**1. vLLM は vLLM 自身の既定 8000 に無条件で bind していた。** ollama は
#489 以来 waired 所有のポート (9475) を持っている。8000 は開発機で最も
取られやすいポートの一つで、しかも vLLM では致命的である ——
adopt の経路が無いので API サーバは bind に失敗して終了し、bootstrap の
再試行はすべて同じ理由で失敗する。実機ではそれが「ローカル AI の無い健康な
ホスト」になり、面には何も出なかった (waired-agent#1026)。

**2. `decideEnginePower` の 2 つの腕が同じ事実に違う答えを返していた。**
ollama 腕は `StateFailed` / `StateStopped` / `StateNotStarted` を全部
`running` に落とす。つまりクラッシュして復旧予算も尽きたホストが
`Engine power: running` と報告し、tray は**居ないプロセスに対して
「Stop inference engine」を出していた**。vLLM 腕 (#881) は 3 つとも
`stopped` と答える —— メモリについては真だが、原因については黙っている
(waired-agent#964)。

**3. 明示 start が soft トグルを 2 通りに扱っていた。** ollama 腕は
`EnsureRunning` を直接呼ぶので、`waired inference off` にしたデバイスでも
エンジンが上がる。vLLM 腕は `requestEngineStart` 経由で `errInferenceOff`
になるが、ディスパッチされた goroutine の中なので誰にも見えない。CLI は
どちらでも `engine start ok.` と出していた。

## Decision

**1. vLLM も waired 所有のポートを持つ。** `VLLMPortAuto` (0) /
`DefaultVLLMBundledPort` (9479) / `legacyVLLMDefaultPort` (8000) /
`ResolvedVLLMPort()` —— ollama と同じ構成で、legacy の flip も含む。
`Defaults()` は agent.json 書き出し時に直列化されるので、既存ホストは
literal 8000 を持っており「選んでいない」と区別できない。11434 の flip が
書かれたのと同じ状況である。オペレータが自分で選んだポートは残る。

9479 は waired のループバック帯 (9472〜9478) の空き枠で、
waired-ai/waired#1277 が第 2 ゲートウェイを撤去して空けたもの。

**カーネル割当 (:0) は採らない。** この番号は config だけを持つ
プロセス外の読み手が使う —— ベンチマーク、深部ベンチ、メッシュ probe の
対象、`engineListening` の他エンジン検出。動的ポートにすると、まずその
全員に実際の bind 値を配る仕掛けが要る (waired-agent#1024 と同じ形)。
しかも再起動のたびに動く。得るものが無い。

**2. 電源軸に `failed` を足し、両エンジンで使う。**
「起動を待っている」と「誰かが原因に対処するのを待っている」は別の状態で、
面が必要とするのはその区別である。tray は両方に同じ Start 行を出す ——
明示 start は give-up ラッチの規定のリセットでもある (#29/#946) ——
が、その隣の警告行が「押せ」と「読め」の違いになる。

`stopped` は「誰かが止めた、あるいはまだ起きていない」に絞る。
park が最優先なのは変えない: わざと止めたエンジンの故障を報告すると、
設定である事象の原因を探しに行かせることになる。

**3. hard 軸は persisted な soft トグルを上書きしない。**
`waired inference off` は方針であり、その場限りの電源操作がそれを黙って
覆すのはおかしい。両エンジンとも断る。断りは 500 ではなく **409** ——
故障ではなく拒否だからで、`management_public_share_toggle.go` が
capability 欠如に対して既に取っている形と同じ。CLI は `ok.` の代わりに
理由を出す。

## Consequences

- 既存ホストの vLLM は次回のデーモン起動で 9479 に移る。`agent.json` に
  自分でポートを書いたオペレータは影響を受けない。
- `EnginePowerState` の列挙値が 1 つ増えた。読み手 (tray の 3 腕、CLI の
  print) は本 PR で対応済み。古い読み手は未知の値を「stopped 以外」として
  扱うので、今までの `running` より悪くはならない。
- `TestEngineController_StopThenStart` と `decideEnginePower` の
  vllm-failed 行を反転した (CLAUDE.md §Test discipline)。
- ollama ホストで `waired inference off` のまま `engine start` を打つと
  エンジンが上がらなくなる —— 挙動の変更であり、理由を面に出す。

## Refs

- waired-ai/waired-agent#1026, #964
- waired-ai/waired-agent/pull/1050
- `docs/decisions/20260821/1308-engine-power-is-per-engine.md`
- waired-ai/waired#1283 (L75), waired-ai/waired#1277 (9479 が空いた経緯)
