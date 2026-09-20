---
status: accepted
---

# 長いコンテキストウィンドウは頼まれて初めて出し、そのために取り込んだファイルの上限を書き換える (20260920 23:00)

## Status

Accepted。オーナー決定(2026-09-20、waired-ai/waired#1456)。
`docs/decisions/20260920/0300-catalog-admits-what-was-run-on-the-reference-host.md`
決定 5「YaRN で 1M まで広げられるモデル(既存の Qwen を含む)は、1M を選んだときに
警告を出したうえで出す。設計と実装は waired-ai/waired#1456 で扱い、この記録では決めない」を
実装する記録。

原文(オーナー、2026-09-20):

> YaRNにより1Mが出せるモデルについては、1Mで選択されたときにいちおう警告を出した上で出したいです。これは既存のqwenモデルたちについても同様です

**私設側の記録を散文で置き換える。** `waired-ai/waired` の決定
`docs/decisions/20260803/1332-hard-vs-soft-model-limits.md` は
「YaRN による 200k 拡張は不採用(追加学習なしの外挿は動作レベルで破綻する)」としていた。
これを**配布元が係数と到達値を自ら公表しているモデルに限って**追い越す。gpt-oss 型
(すでに YaRN 済みのモデルに追加で外挿を掛ける)の不採用はそのまま残る。リポジトリを
またぐ supersede は散文でしか書けない。

## Context

### 出せる段の供給が尽きていた

waired-ai/waired-agent#1449 で `glm-5.2` と `deepseek-v4-flash` を退役させたあと、
出荷カタログで 1,048,576 を名乗るモデルは 0 件になった。段の定数・ワイヤ・ルーティング・
CP の丸め・NAVI の表示は全部生きているのに、供給源だけが空という状態だった。

### エンジンの事実(pin された ollama v0.34.0 / llama.cpp b10760 のソースと参照機で確認)

1. **YaRN は環境変数だけで掛かる。** ollama は上流の `llama-server` をそのまま subprocess
   として起動し、`os.Environ()` を丸ごと渡す(`llm/llama_server.go`)。ollama 自身も
   `LLAMA_ARG_FIT_TARGET` で同じ仕組みを使っている。**GGUF に足すキーはゼロ。**
2. **YaRN は IMROPE と合成される。** Vulkan の `rope_funcs.glsl` の `rope_multi()` は、
   IMROPE の分岐でも `rope_norm` / `rope_neox` と同一の `rope_yarn()` を呼ぶ。YaRN は
   rope の変種に後から掛かる層で、Qwen 3.5 系だけ外れる構造になっていない。
3. **到達値はエンジン自身が出す。** `llama-context.cpp` が、独自の YaRN を渡されたとき
   `n_ctx_train = n_ctx_orig_yarn / rope_freq_scale` を再計算する。262,144 ÷ 0.25 =
   **1,048,576**。配布側の慣習も同じで、上流の「1M」GGUF は 1,048,576 を刻んでいる。
4. **唯一の壁は切り詰め。** `llm/server.go` が `num_ctx` を GGUF 自身の `context_length` に
   落とし、`-c` は CLI に明示されるので環境変数では上げられない。
5. **`attn_factor` を渡してはいけない。** エンジンが係数から導き、カーネル側の分を打ち消す。
   渡すと二重に効く。
6. **blob の digest 検証は pull のときだけ。** 読み込み時の検証は無く、既に在る blob は飛ばす。

### 参照機での実測(sv-evox2、Strix Halo 128 GB、q4_0 KV、flash attention on)

出荷 variant `qwen3.6-35b-a3b/mtp-q4-gguf`:

| 窓 | KV | MTP draft KV (f16) | 常駐合計 | 読み込み+応答 |
|---|---|---|---|---|
| 200,704 | 1,102.50 MiB | 392.00 MiB | 23,638 MB | 19.5 s |
| 1,048,576 | 5,760.00 MiB | 2,048.00 MiB | 29,560 MB | 24.1 s |

どちらも正答した。**メモリは制約ではない** — 参照機の上限(見積り合計 約 84,000 MiB、
waired-ai/waired-agent#1443)の十分内側。

**制約は時間だった。** 同じ機械でのプレフィルは 6,144 トークンで 286 tok/s、22,528 で 109、
47,104 で 57、71,680 で **38 tok/s**。経過時間はトークン数の約 n^1.9 で伸びる。
1,048,576 を一度に埋めると数十時間になる。鋭い縁はプレフィックスが外れたときで、
1M 規模で分岐すると再投入が時間単位になる。

## Decision

1. **モデルの「到達できる窓」をカタログが持つ。** `catalog.Manifest.RopeScaling` に配布元の
   スケーリング(種類・係数・元の長さ・配布元が言う上限)を書く。係数は**導出しない** —
   Qwen は 262,144 に対して上限 1,010,000(比 3.85)と書きながら、同じカードの手順で
   係数 4 を渡す。上限はモデルごとに違う(Qwen3.8 系は 1,000,000)。
2. **対象は配布元が拡張を文書化しているモデルだけ。** カードに「extensible up to」も YaRN の
   手順も無いモデル(`qwen3.5-0.8b` / `qwen3.5-2b`)は対象外。出典が無いものを推測で入れない。
3. **長い窓は頼まれたときだけ段に載る。** `DeclarableNativeWindow` と `OllamaServedWindows` は
   `context_length` だけを見る形のまま変えない。`OllamaServedWindowsWith(m, chosen)` が
   名指されたときだけ段を開く。理由: 静的 YaRN は短いプロンプトにも掛かるので、
   **頼んでいない人にとっては別のモデルになる。**
4. **選択は `preferred-model.json` に記録する**(`ContextWindow`)。CP からの指示は
   `signer.InferenceState.DesiredContextWindow`(0 / 200704 / 1048576)。
5. **取り込んだ GGUF の `context_length` を、同じ幅の u32 で書き換える。**
   - 置き場所は `download.Puller.Pull` の中、既存の `stamp` の隣。**pull のときに行い、
     選択のときには行わない** — 選択時に書き換えると動いているエンジンの下でファイルを
     書くことになり、長い窓への切り替えが最悪の瞬間のファイル書き込みに依存する。
   - **代償**: 長い窓を一度も選ばない人のファイルも、配布元のバイトと一致しなくなる。
     これは `stamp` が manifest に対して既にしている取引と同じで、ここに明記する。
   - blob は digest で共有されるので、同じ重みを指す他のタグにも及ぶ。防ぐのではなく
     **記録する**(`TagsSharingBlob` の一覧をログに出す)。
   - 書き換えのあと**ディスクから読み戻して照合する**。落ちなかった書き込みが成功に
     見えると、エンジンは誰も頼んでいない窓で配信することになる。
6. **出せない窓は頼まない。** ollama は `num_ctx` をファイルの値に切り詰めるので、
   ファイルが短い窓しか名乗っていないときに長い窓を頼むと、runner は短い窓を保ったまま
   製品は頼んだ窓を記録して mesh に宣言する(waired-ai/waired-agent#1436 の穴、
   not planned で閉鎖)。**再開はせず**、`ollamaWindowRequestFor` が起動前にファイルの
   上限を読み、足りなければ短い窓で配信して**理由を人に伝える**(`ModelTuning.Warning`)。
7. **宣言の clamp は「到達できる窓」に対して行う。** これが無いと `[1m]` の行に誰も
   答えられない(1,048,576 で配信して mesh には 200,704 と言う)。スケーリングを文書化
   していないモデルは今までどおり学習した長さで止める。
8. **1M の段は意図的スピルでは届かない。** 段の計画の rule 2 は
   `OllamaEffectiveContextFloor`(= 200,704)で止まるので、長い窓は rule 1(実容量で収まる)
   か rule 3(カードを外した同じ機械が届く)でしか通らない。意図してそうしている。
9. **警告には時間のことも書く。** 実測(上記)より、1M の代償は品質だけでなく
   「窓を埋めるのにかかる時間」でもある。オーナー決定 2026-09-20: 計画どおり進め、
   文面に時間を入れる。

## Consequences

- カタログの 9 モデルが 1M に届く(qwen3.5 の 4b / 9b / 27b / 35b-a3b / 122b-a10b、
  qwen3.6 の 27b / 35b-a3b、qwen3.8 の 27b / flash-next)。3 モデルは届かない
  (`qwen3.5-0.8b` / `qwen3.5-2b` / CI 専用の `granite4-350m`)。
- **`ReasonWindowTooSmall` が復活する。** waired-ai/waired-agent#1400 で「誰も produce しない」
  と deprecated になっていたが、1M では問いが再び成立し、カタログの大半が「届かない」と
  答える。長い窓を選んだときに行を伏せる理由はこれで、述語は `hostfit.ReachesWindow`。
- **カタログの 1M の価格付けに項が足りない。** 実測は事前計算より 4.3% 大きく、差は
  MTP draft の KV が f16 のまま窓に比例して増える分(392 → 2,048 MiB)。`hostfit` に足す。
- 1M を一度に埋める時間の実測は、この製品が「1M の窓」として売るものが
  「一度に埋める入力」ではなく「少しずつ育つセッション」であることを意味する。
  プレフィックスが外れたときの代償が、その前提の弱いところ。
- 検証していないこと: **262,144 を超える位置からの検索精度。** プレフィルの時間の壁で
  33 万トークンの針刺し試験は現実的に回せなかった。ソース上は rope が正しく掛かり、
  1M で短い要求には正答する。オーナー判断(2026-09-20): 警告を出すので試験は不要。

## Refs

- waired-ai/waired#1456 / waired-ai/waired#1359 / waired-ai/waired-agent#1435 / waired-ai/waired-agent#451
- waired-ai/waired-agent#1436(宣言の一般解、not planned で閉鎖)
- `docs/decisions/20260920/0300-catalog-admits-what-was-run-on-the-reference-host.md`(決定 5)
- `docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md` / `docs/decisions/20260916/2350-every-waired-row-is-a-200k-or-1m-session.md`
- `docs/knowledges/20260920/2300-serving-past-the-trained-window-on-ollama.md`
- 私設側: `waired-ai/waired` `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`(散文で部分 supersede)
