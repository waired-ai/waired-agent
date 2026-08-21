---
status: accepted
---

# エンジン電源はエンジンごとに持つ / 常駐の軸は vLLM には無い (20260821 13:08)

## Status

Accepted

## Context

`waired inference engine stop` は「今すぐメモリを返せ」というオペレータの操作
である。しかし `engineController` は `*infruntime.OllamaAdapter` を保持しており、
ルートはコントローラが在れば常に登録されるため、vLLM で配信しているホストでは
**動いていない ollama アダプタを park して成功を報告し、vLLM は GPU を握ったまま**
だった (#881)。

無害な no-op ではない。`subsystemFacts` は同じ park ラッチを読み、`subsystemState`
の2番目の腕がそれを `stopped` に変換してメッシュへ押し出す。つまりこの操作は
**まだ応答しているホストからピアのルーティングを剥がしたうえで、メモリは1バイトも
返さなかった**。

根は1か所ではなかった。`agentInferenceProvider` は配信エンジンに関係なく必ず
`OllamaAdapter` を構築する (`inference.go`)。したがってツリー中の
`p.ollama == nil` ガードは**一つも発火せず**、`unload` / `residency` / 常駐プローブ /
`EngineReady` / トレイのメニュー行が、いずれも配信していないエンジンを相手にして
いた (#943 / #944)。

vLLM が ollama と違うのは、**モデルを降ろす軸が存在しない**点である。
`--gpu-memory-utilization` は起動時にプールを確保し、プロセス終了まで手放さない
(この事実はツリー内 `inference_probe.go` に逐語で記録済み)。

## Decision

1. **エンジン電源は配信エンジンに対して働く。** `engineController` はプロバイダを
   受け取り、毎回 `servingEngine()` を問う。ブート時に1つのアダプタを捕まえない
   のは、`adoptEngine` が起動後に答えを変えうるため (#339)。

2. **vLLM の park ラッチはプロバイダが持つ**(ollama はアダプタが持つまま)。
   非対称を承知で選んだ理由は3つ、決め手は3つ目:
   - `bootstrapVLLM` は呼ばれるたびに `VLLMAdapter` を作り直すので、アダプタに
     置いたラッチは次の bootstrap で捨てられる。
   - venv インストール中・重みダウンロード中は**アダプタ自体が存在しない**。
     ollama の `Park` は初回起動前でも成立する仕様であり、この軸も揃える必要がある。
   - `bootstrapVLLM` は**重みダウンロードを跨いで `engineOpMu` を保持する**ので、
     `StopEngine` は同じ mutex を取れない(数十分ブロックする)。park は bootstrap
     自身のチェックの**後**に必ず着地しうる。作り直されたアダプタが
     `VLLMConfig.Parked` でラッチを**ライブに読む**形だけが、その spawn を拒否できる。

3. **`managed` は vLLM では常に true。** vLLM に adopted の経路は無く、ラッチが
   プロバイダに在るためアダプタ不在でも軸は成立する。false にすると管理 API が
   stop を 409 で弾き、「システムが守らない状態を面が報告する」という #881 自身の
   形を再生産する。

4. **決定 `20260820/0130-model-residency-is-a-setting.md` の2軸のうち、第1軸
   (`unload`) は vLLM では空である。** 面は取り繕わず断り、`engine stop` が唯一の
   解放弁であることを名指す (waired#1067)。`residency` の読みは「保持し続ける」を
   返す——それは**このホストでは文字どおり真**だから。書きは
   `ResidencyEffectUnsupported` を返し、値は**保存する**(設定は設定であり、#339 に
   より軸を持つエンジンを後から採用しうる)。

5. **エンジン停止の予算はエンジンごと**に持ち、`internal/management` に置く。
   CLI とトレイは自分の予算をその**上**に取る必要があり、従来は手書きで写した
   15 秒に対してアサートしていた (#945)。

## Consequences

- vLLM ホストで `engine stop` が実際に VRAM を返し、`engine start` が
  `requestEngineStart` 経由で venv・重み・チューニングを解決し直して戻す。
- `subsystem_state` / `EngineReady` / 常駐プローブが配信エンジンを見るようになり、
  ollama の park が vLLM ホストの広告を止める経路が消える。
- `ResidencyResponse.Supported` が増える。`*bool` なので **nil は「古いデーモンで
  何も主張していない」**であり、古いトレイは従来どおり描画する。
- `ErrEngineParked` は `internal/runtime/adapter.go` に移り、文言からエンジン名が
  消えた。この文字列はゲートウェイ 503 の本文としてユーザーに届くため、
  `docs-site/TRANSLATION.md` の「ユーザー向け文面に内部名を出さない」に従う
  (オーナー承認 20260819 / #836 / #850)。`ErrEngineUnrecoverable` は各アダプタが
  自分の名前で包むので、ollama 側の描画は従来とバイト単位で同一。
- 未着手として残したもの: ollama 側 `EngineState` の `StateFailed` が `running` を
  返す件(既存の期待値を反転させるため別変更)、`requestEngineStart` が
  `isInferenceDisabled` で早期に降りる非対称、vLLM の常駐を `observed=true` として
  報告しうる件 (#880 の warm 選好に効く)。

## Refs
- https://github.com/waired-ai/waired-agent/issues/881
- https://github.com/waired-ai/waired-agent/issues/943
- https://github.com/waired-ai/waired-agent/issues/944
- https://github.com/waired-ai/waired-agent/issues/945
- https://github.com/waired-ai/waired-agent/issues/946
- https://github.com/waired-ai/waired-agent/issues/947
- docs/decisions/20260820/0130-model-residency-is-a-setting.md
