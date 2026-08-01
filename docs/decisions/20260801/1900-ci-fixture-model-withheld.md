---
status: accepted
---

# CI 用モデルはカタログに置き、提示だけしない (20260801 19:00)

## Status

Accepted

## Context

waired-ai/waired-agent#322 の計測で、qwen2.5-coder は 0.5b / 1.5b / 3b / 7b / 14b の
全サイズで、全 tool 呼び出し・全試行が失敗することが分かった。世代ごとの不適合であり、
退役対象になる。

ところが **qwen2.5-coder-0.5b は per-PR の routing sentinel（#496）が pin している**。
3-OS で毎 PR 走るゲートなので、0.4GB という安さが選定理由だった。退役させると
このゲートが壊れる。

「CI 用モデルをカタログの外に出す」案を先に検討したが、**成立しない**。sentinel は
実インストール済み daemon に `--inference-bundled-model-id=<カタログ id>` を渡し
（`installtest-run.sh` / `installtest-macos.sh` / `installtest-windows.ps1` /
`installtest-daemon-engine.sh` の計 7 箇所）、daemon は埋め込みカタログで解決する。
`RankModels` の `PreferredModelID` はカタログに無い id を `ErrModelNotFound` で弾く。
カタログ外にするには daemon にカタログ外 id を受け付けさせるしかなく、それは
**タイポした model id を弾く検証を、テストフィクスチャのために弱める**ことになる。

## Decision

**CI 用フィクスチャはカタログに置き、提示面からだけ外す。**

1. `Manifest.InternalOnly`（理由文字列、additive）を追加。非空なら「出荷はするが
   提示しない」。
2. **`BundledManifests()` の既定を「提示するモデルのみ」に変更**し、全件が要る
   4 箇所だけが `BundledManifestsIncludingInternal()` を明示的に呼ぶ。
3. フィクスチャは **granite4-350m** にする。`waired/tiny` エイリアスを移す。

### フィクスチャ選定でまず間違えた点

最初は tool 適合性（プローブ 0/6）を根拠に qwen3.5-0.8b を選んだ。**これは誤り**で、
routing sentinel は tool を一切使わない — `driveAnthropic` / `driveOpenAI` が送るのは
"Reply with one word: hi" だけで、証明したいのは「gateway 経由でローカルモデルに
届いたか」である。フィクスチャに要るのは**小ささと、止まること**だった。

実測（"Reply with one word: hi" への応答）:

| モデル | 生成トークン | 所要 | サイズ |
|---|---|---|---|
| qwen3.5:0.8b | **17,787** | **58.5s** | 1.0 GB |
| qwen2.5-coder:0.5b | 2 | 1.6s | 397 MB |
| **granite4:350m** | **2** | **1.6s** | 708 MB |

qwen3.5 は thinking モデルで native window も 262k。boot benchmark が
`/api/generate` で時間切れになり、per-PR ゲートが落ちた。タイムアウトを上げれば
「通る」が、それは毎 PR × 3-OS のゲートを分単位に伸ばし、実在の性質を隠すだけである。

**granite4-350m を選んだ理由**は速度とサイズだけではない。qwen2.5-coder:0.5b は
397 MB とさらに安いが tool 呼び出しが 6/6 で全滅するため、「実際にツールを使わせる」
CI レグを将来足す路線が最初から閉じる。granite は greeting と search が 3 試行とも
clean で、read-file だけ 1/3 で「自分にはファイルにアクセスできない」と能力を
過小申告する — フォーマット違反ではない。352M のモデルとして妥当な挙動であり、
退役ワークリストにも載らない。コストは 397 MB → 708 MB。

### なぜ既定を反転させるのか

提示面は全て `catalog.BundledManifests()` を通る — 自動選定、インストール時選定、
**低スペック機向けの下位フォールバック提案**、tray カタログ、CP の device catalog、
`models ls --detail`、生成ドキュメント。CP も同じ関数を通る
（`bundledManifestsFunc = catalog.BundledManifests`）ので CP 側の変更が要らない。

各面に「フィルタを掛け忘れないでね」と要求する設計にしなかったのは、**失敗の向きが
非対称**だから。関門で絞れば、忘れた面は**出しすぎではなく出さなすぎ**になり、回復
可能である。逆向きの既定だと、一度の忘れが「誰にも渡すべきでないモデルが誰かの
推奨になる」という、まさにこの仕組みが防ごうとしている欠陥そのものになる。

取り違えた場合は解決系が静かに止まるが、それは次の PR で **sentinel が落ちる**ので
静かではない。

### なぜ bool ではなく理由文字列か

agent-grade ストアの `unmeasurable` マップと同じ規律。**正当化を要求されない例外は、
誰も見直さない例外になる**。テストで `"true"` のような「理由のふりをしたフラグ」を
弾く。

### 提示可否は品質とは直交する

`quality_tier` と install quality floor が「推奨に値するか」、agent-grade ストアが
「そもそもハーネスを駆動できるか」、そして `internal_only` が「我々が提供すべきものか」
を答える。**granite4-350m は実際に構造化 tool call を出せるが、352M が出す成果は誰にも渡す
価値がない。** 通ることと提供に値することは別である。

## Consequences

- sentinel のダウンロードが 397 MB → 708 MB。3-OS 毎 PR で +0.9GB/回。
- **低スペック機の回帰テストを実カタログで固定した**。合成マニフェストでは
  production の配線を証明しないため、`catalog.BundledManifests()` の実出力を
  `SelectBundledModel` に流し、1/2/4/8/16/32/64 GB のいずれでも withheld モデルが
  `ModelID` にも `BelowFloorModelID` にも現れないことを確認する。後者が本命で、
  下位フォールバックは「他に何も無いホストに floor 未満を提示する」経路であり、
  withheld な小型モデルはまさにそこで拾われる形をしている。
- **明示 pin は通る**。CI がこれに依存するので、後から「修正」してフィルタを
  掛けないようテストで固定した。
- CP は proto を bump するまで withheld モデルを提示し続ける。bump が完了条件。
- qwen2.5-coder 4 件（0.5b を含む）の実削除のブロッカーが解消した。実削除には
  #200 の退役機構が先行必須。

## Refs

- waired-ai/waired-agent#322 — agent-harness 適合性の計測
- waired-ai/waired-agent#496 — routing sentinel
- waired-ai/waired-agent#200 — 退役機構
- waired-ai/waired#1002 — L15
- docs/decisions/20260801/1410-agent-harness-grade-measured.md — 計測方法の決定
