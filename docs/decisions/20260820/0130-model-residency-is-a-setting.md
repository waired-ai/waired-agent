---
status: accepted
---

# モデル常駐はタイマーではなく設定にする — 既定は「降ろさない」 (20260820 01:30)

## Status

Accepted。オーナー裁定（2026-08-20、waired-agent#861）。4ホストの実測で根拠を固定。

## Context

`OLLAMA_KEEP_ALIVE` は `60m` のハードコード定数だった。切れると次のリクエストは
**重みの再ロードと full prefill の両方**を払う。実測（ollama 0.32.13、実 Claude Code の
30,359 トークンのボディ）:

| ホスト | cold 合計 | weights load | prefill | 常駐時 |
|---|---|---|---|---|
| 離散GPU 24GB / 35B-A3B | 16.87 s | 7.04 | 9.53 | 0.47 s |
| 統合メモリ 48GB / 35B-A3B | 55.84 s | 14.90 | 40.37 | 1.15 s |
| 離散GPU 8GB / 4B | 43.36 s | 9.79 | 32.02 | 3.62 s |

**prefill が 57〜77% を占める。** そして計測開始時点で3台中2台が既に失効しており、
エージェントは健全だった＝実運用で普通に到達する状態である。

方針を決めた3点:

1. **温め直しで戻せるのは weights 側だけ。** 現行 `warmServingModel` はプロンプト無しの
   ロードなので `load_duration` しか消えず、回収率は 16〜28%。安定 prefix を再生する
   案は、これらのホストが配るモデルでは**回収ゼロ**だった（効いたのは 0.8b のみで、
   同一ホスト上で 4b は効かない＝モデル依存。判別子は #866 で未確定）。
   KV キャッシュは実リクエストのトークン列で引かれるため、失効後の復元には
   **その実リクエストの再送＝ユーザーのプロンプトの永続化**が要る。
2. **`keep_alive: -1` でも waired 内部では調停済み。** 3 OS すべてで、別モデルを
   1 回要求しただけで常駐は即退去した（`infruntime.MaxResidentModels = 1`、
   オーナー裁定 2026-08-10 / waired-agent#644 /
   docs/decisions/20260811/2340-one-model-resident-at-a-time.md）。
3. **保持の実害は限定的。** 観測できたのは離散GPUで空き VRAM が枯れること（他の GPU
   仕事が始められない）のみ。統合メモリのホストでは、外部から 90〜105 GiB のメモリ圧を
   かけても重み・KV とも退去せず、cold prefill も 1.3% しか動かなかった。

競合の型も同じ側を指す。**暗黙にロードされたものにはタイマー、明示的にロードされた
ものにはタイマー無し** — LM Studio は JIT ロードに 60 分 TTL、`lms load` の手動ロードには
TTL 無し。llama.cpp `llama-server` は `--sleep-idle-seconds` 既定 `-1`（無効）、
vLLM の Sleep Mode は明示呼び出しのみ。waired の active model は init のピッカーや
CP の `desired_model_id` で明示的に選ばれたものなので、この分類では後者に属する。

なお `Inference.IdleTimeout` は `agent.json` / 環境変数 / `-inference-idle-timeout`
フラグまで揃っていながら**消費者がゼロ**で、既定値 10 分は 60 分の定数と食い違っていた。

## Decision

1. **モデル常駐は `Inference.IdleTimeout` が決める。** 値はエンジンに渡す keep-alive
   （`OLLAMA_KEEP_ALIVE` と per-request `keep_alive` の両方）。
   **0 または負は「アイドルでは降ろさない」**で、これを既定とする。
2. **モデル・OS・ホストクラスで分岐しない。** 1 つの数値、同一コードパス。
   保持コストが構成によって違うことは実測で分かっているが、分岐したロジックは
   検証面が組合せ的に増え、モデル依存の分岐は判別子が未確定である（#866）。
3. **失効後の温め直しは追加しない。** 有限値を設定した運用者に対して失効直後に
   載せ直すのは意図の裏切りになる。`warmServingModel` の既存4トリガ
   （起動 / reconcile / unpark / ベンチ後）＝「常駐しているはずなのにしていない」
   場面は変更しない。
4. **常駐をユーザーに見せる**（waired-agent#879）。`waired status` / tray /
   ピアの `/healthz` に、重みが (V)RAM にあるかどうかを出す。
5. **エンジンを止めずにメモリを返す手段を用意する**（`waired inference unload`）。
   既定が保持である以上、解放弁が要る。従来は `waired inference engine stop` しか
   なく、それは serve する能力ごと止める。

## Consequences

- 既定のホストは、一度ロードしたモデルをアイドルでは降ろさなくなる。
  離散GPUホストでは、その間 GPU の空き容量は他の用途に使えない。
- `-inference-idle-timeout` / `WAIRED_INFERENCE_IDLE_TIMEOUT` / `agent.json` の
  `idle_timeout` が初めて実効を持つ。既定値は 10 分から 0（無期限）へ変わるが、
  従来この値を読む消費者は存在しなかったので、実挙動としては 60 分 → 無期限の変更である。
- 既存テスト2件の期待値を反転させた（`OLLAMA_KEEP_ALIVE=60m` → `-1`、
  `IdleTimeout` 既定 10m → 0）。
- ピアの `/healthz` に `model_resident` が増える。**署名付きマップではなく
  ライブHTTPプローブ**なので capability ゲートは不要。古いピアは省略し、nil は
  「未観測」であって「冷えている」ではない。

### 見直し条件（未計測のまま残したもの）

以下のどちらかが計測できたら、既定値を再考する:

- **離散GPUで非ollamaのGPU消費者と競合したときの挙動。** CUDA ツールキットも
  第二エンジンも無く、ホストを変更せずに VRAM 圧を作れなかった。保持の実害候補は
  ここに集中している。
- **macOS で swap に落ちた他プロセスの体感遅延。** 16 GiB の圧で swap が 0→6.87 GB
  発生することは測ったが、落とされた側の遅延は測っていない。

統合メモリ Windows の「退去しない」という結果には前提がある: ページファイルが
8,192 → 83,862 MB まで自動拡張できる構成（ディスク空き 1.38 TB）での観測であり、
固定サイズや逼迫時は未観測。

## Refs

- waired-ai/waired-agent#861（本裁定を求めた issue、実測コメントつき）
- waired-ai/waired-agent#879（常駐の可視化）、waired-ai/waired-agent#880（ルータ側）
- waired-ai/waired-agent#866（prefix 再利用の判別子が未確定）
- docs/decisions/20260811/2340-one-model-resident-at-a-time.md
