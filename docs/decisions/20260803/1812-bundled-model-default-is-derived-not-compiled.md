---
status: accepted
---

# 同梱モデルの既定値はコンパイル時に持たず、ホストから導出する (20260803 18:12)

## Status
Accepted

## Context

`agentconfig.Defaults()` は同梱モデル id をコンパイル時定数として持っていた。

```go
BundledModelID: "qwen2.5-coder-7b-instruct",   // internal/agentconfig/config.go:559
```

**この値はピッカーが決して出さない値だった。** qwen2.5-coder-7b は 32,768
トークン窓で、`hostfit.NativeContextFloorTokens = 200000`（#624）が
install-time の自動選定から除外する。waired-ai/waired#1031 が「窓はルーティング
契約である」として sub-floor 窓の救済 re-rank を削除して以来、これは仕様として
固定されている。実カタログに対して合成 CPU プロファイルで
`setup.SelectBundledModel` を回した結果：

| ホスト RAM | 選ばれるモデル | tier |
|---|---|---|
| 2–3 GB | qwen3.5-0.8b（フロア未満の任意提示） | 12 |
| 4–6 GB | qwen3.5-2b（同上） | 27 |
| 8 GB | qwen3.5-4b | 42 |
| 12–16 GB | qwen3.5-9b | 52 |
| 24 GB | qwen3.6-27b | 70 |

**どのサイズでも qwen2.5 系は 1 つも選ばれない。** 陳腐化していたのではなく、
それを生むはずの機構から到達不能だった。

そして到達不能な値が使われる経路があった。`shouldAutoSelectBundledModel` は
`inference-` で始まる**任意の**フラグ、`WAIRED_INFERENCE_*` の**任意の**環境変数で
選定を丸ごと飛ばす。fresh install の 4 GB ホストに
`--inference-ollama-port 11500` を渡すと、ハードウェアプロファイルは取られず、
アンダースペック無効化も走らず、このホストが動かせないモデルの 4.7 GB を pull する。

**両方揃って初めてバグになる。** 導出された id なら退避先が無害であり、ゲートが
狭ければ退避先に到達しない。

さらに `BundledModelInputs.Pinned` / `.Forced` は、本番コードから一度も true に
されていなかった。構築箇所は `cmd/waired-agent/bundled_model_select.go` の 1 つ
だけで、コメントは "neither pinned nor forced"。オペレータの意図を表すための
2 フィールドが死に、その代役を「関数ごとスキップ」が務めていた。

## Decision

**1. コンパイル時の既定モデル id を持たない。** 値はハードウェア選定
（`setup.SelectBundledModel`、#517）の**出力**か、オペレータの明示的なピンで
あって、ホストを見る前に定数として決められる種類のものではない。空は
「まだ選ばれていない」という実在の状態で、pre-pull と vLLM ターゲットは
どちらも空 id で推測せずに降りる。

**2. スキップではなく意図を解決する。** モデルを名指す信号と、推論を動かすか
どうかの信号だけがゲートに届く：

| 信号 | 結果 |
|---|---|
| `--disable-inference` / `WAIRED_INFERENCE_ENABLED=false` | Skip |
| `--inference-preferred-model-id` | Skip（#306: オペレータ自身のモデルが serving 経路を持つ） |
| `--inference-bundled-model-id` | `Pinned` — 選定は走る |
| `--inference-enabled=true` | `Forced` — 選定は走る |
| ポート / キャッシュ / TTFB / vLLM ノブ / ollama source | 影響なし |

エンジンの配線とモデルの選定は別の問いである。ポート番号はどのモデルが
そのマシンに合うかを語れない。

**3. Forced のアンダースペックには id が要る。** 推論を強制 ON にすることは
「動かすかどうか」の宣言であって「どれを動かすか」の宣言ではない。コンパイル時
既定が無くなった以上、`SelectBundledModel` の Forced 分岐は
`BelowFloorModelID` —— そのホストが実際にロードできる唯一のモデル —— に落ちる
必要がある。落ちなければ、何も serve しないまま推論が有効になる。

**4. 別名ピンは id と同じモデルである（#380）。** `cfg.BundledModelID` は
任意のカタログ別名を受けるのに、3 箇所が生の設定値を解決済み id と比較して
いた。`bundledModelID()` / `isBundledModel()` に一本化する。

## Consequences

- fresh install の agent.json に書かれる id は、必ずそのホストで選ばれた
  ものになる。「既定値がたまたま残った」状態が無くなる。
- 無関係なフラグ 1 つでアンダースペック無効化が飛ぶ経路が閉じる。
- `Pinned` / `Forced` が本番で使われるようになり、ピンしたオペレータは
  `SelectBundledModel` の文脈フロア警告を受け取れるようになる（今まではその
  経路ごと飛ばされていた）。
- 選定より前・pull より前の窓では `waired/default` が解決できない状態が
  ありうる。その窓は推論がまだ動いていない状態であり、Active が決まれば
  `st.Active.ModelID` が勝つ。「間違ったモデルを引く」から「何も引かない」への
  変化で、後者のほうが正直である。

## Boundary — #465 との境界

#465（waired-ai/waired#1056 の批准済み決定）は「アンダースペックはラッチでは
なく推奨、既定 OFF ＋動くオプトイン」を決めており、`BelowFloorModelID` の
オプトインを**両サーフェス**（CLI・ブラウザ）に配線するか、フィールドごと削除
するかを扱う。ここで入れたのは **Forced な boot 経路だけ**の narrow な配線で、
対話オプトイン・コピー・管理 API・ブラウザには触れていない。

同じ理由で、`InstallQualityFloorTier` は動かさない。フロアを下げて 4–6 GB
ホストを救うのは同じ問題への別解であり、#465 が別の答えを採用済みである。

## Refs

- #470（本件）、#380（別名ピン）
- #465 / #451 / #200（32k ネイティブ勢の扱いと退役）
- waired-ai/waired#1031（窓は契約）、#624（文脈フロア）、#517（install-time 選定）、#306（preferred が serving 経路を持つ）
