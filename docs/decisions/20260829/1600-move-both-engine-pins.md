---
status: accepted
---

# エンジン pin を両方動かす — ollama 0.33.2 / vLLM 0.28.0 (20260829 16:00)

## Status

Accepted。オーナー裁定 (2026-08-29): ollama 0.32.15 → 0.33.2、
vLLM 0.24.0 → 0.28.0、waired-agent#1133 が提案した GPU 追加測定 2 本を
実施した上で、waired-agent#1131 と合わせて **1 本の PR** で出す。

## Context

waired-agent は推論エンジンを 2 つ同梱し、版をコードで pin している:
`OllamaPinnedVersion` (internal/runtime/ollama_version.go) と、vLLM の
pin **セット** (internal/runtime/vllm_pins.go — `VLLMPinnedVersion` /
`HFTransferPinnedVersion` / `TransformersConstraint` /
`VLLMPythonVersion`)。waired-agent#1132 (ollama) と #1133 (vLLM) が
pin の検証と移動を求め、#1131 (ベンチキャッシュをエンジン版でキーする)
は両 pin issue が「同じ変更で着地させる」と指定していた。

pin 移動が 1 行で済まないのは、この製品が upstream の約束していない挙動を
エンジンから読み出しているため。upstream がその 1 つを変えても何も
エラーにならず、製品の数値と判断が黙って偽になる。何を測ったかは
`docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md` に全部ある。

## Decision

1. **ollama は 0.33.2 へ。** 0.33.0 はこの製品にとって routine な refresh
   ではない。upstream は Claude Code の "tokens left" カウントダウン
   system メッセージを無効化した — ollama はこれをプロンプトの先頭へ
   動かしていて、**毎リクエスト** KV キャッシュを壊していた — うえに、
   prefill の restore point を作り替えて、キャンセルされた prefill が
   越えた地点を保持するようにした。コーディングエージェントの 2 ターン目
   はプレフィックスヒットであるべきで、両方の変更はまさにその経路に
   落ちる。0.33.1 と 0.33.2 は MLX / macOS アプリの修正で製品の経路に
   触れない。0.33.0 でなく 0.33.2 を取るのは列の先頭だからで、間に
   反対する材料が無い。
2. **vLLM は中間版でなく 0.28.0 へ。** pin セットの移動はインストール済み
   全ホストの venv を作り直させる (waired-agent#843) — 一度だけ払う
   コストであり、それを払って既に 4 マイナー遅れの版に着地するのは
   取引として誤り。0.27.1 は「測定が 1 つでも落ちたら」の合意済み
   フォールバックだったが、落ちたものは無かった。
3. **pin セットはタプルとして動かす。** `ConvergeVLLM` はタプルを比較する
   ので、transformers の制約とインタープリタも仮定でなく再確認した。
   どちらも不変で成立: 検証 venv は transformers 5.16.1 を解決し
   (`<6.0` の cap は上端で生きている)、0.28.0 の requires_python
   `<3.15,>=3.10` に 3.12 は入っている。
4. **#1131 はここで着地させる。** エンジン版が動くまでコストがゼロで、
   動いた瞬間に全インストール済みホストのコストになるため: この bump を
   受けたホストは、キーが無ければ、もう走らせていないエンジンが測った
   decode レートと prefill 値を serve し続け、メッシュへ広告し続ける。
   waired-agent#1126 と #1127 がその数値をルーティングの入力にしようと
   している。
5. **variant ごとの `MinEngineVersion` の床は動かさない。** 床が答えるのは
   「この**モデル**は何を必要とするか」であり、エンジン全体の変更を追って
   床を上げると、まだ converge していない全ホストでモデルファミリーごと
   落ちる。
6. **flashinfer / nvcc の PATH 変更は vLLM pin を取るための要件であり、
   独立した改善ではない。** 0.28.0 は flashinfer-cubin の宣言を落とし、
   PATH 上の nvcc が荷重を持つようになった。`VLLMAdapter.processEnv` が
   ホストの CUDA bin を子の PATH に前置する変更無しでは、CUDA は入って
   いるが PATH に無いホスト (Ubuntu の /usr/local/cuda 既定) が次の
   update でローカル推論を失う。経緯と実測は knowledge note §8。

## Consequences

- インストール済みホストは次の update で venv を一度作り直す (#843)。
- 深度ベンチのキャッシュはエンジン版でキーされ、旧版で採った値は一度
  ミスして測り直される。旧エンジンの値なので、それが正しい結果。
- `vllmKVCapacityRe` が読む KV 容量は同一 argv・同一カードで
  393,709 → 339,160 トークン (-14%)。#1126 はこの数値をエンジン版依存の
  値として扱う。
- `docs/knowledges/20260819/2130-local-engine-caches-the-prefix.md` と
  `docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md`
  は 0.33.2 の再実測で**確認された** — #1125 / #1127 はその上で推論を
  続けてよい。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1132
- https://github.com/waired-ai/waired-agent/issues/1133
- https://github.com/waired-ai/waired-agent/issues/1131
- https://github.com/waired-ai/waired-agent/issues/843
- https://github.com/waired-ai/waired-agent/issues/1125
- https://github.com/waired-ai/waired-agent/issues/1126
- https://github.com/waired-ai/waired-agent/issues/1127
- docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md
