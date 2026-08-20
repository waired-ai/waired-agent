---
status: accepted
---

# vLLM の engine.log は再試行の全回を残す — 世代交代でなく追記で (20260821 03:21)

## Status

Accepted。waired-ai/waired-agent#878 が挙げた 3 つの設計上の問いに対する回答。
**挙動が変わる**（ファイルの中身と、上限の意味が変わる）。

## Context

`bootstrapVLLM` は `EnsureRunning` を最大 3 回試す。一方 vLLM アダプタの
`openEngineLog` は spawn ごとに `O_TRUNC` で開き直していたので、
**3 回失敗した起動は 3 回目のログしか残さない**。1 回目が一時的な理由
（ポート衝突など）で落ち、3 回目が本物の設定不備で落ちた場合、
読み手には 3 回目の理由しか見えない。

ollama アダプタは同じ関数名で違う方針を採っている。spawn のたびに
`engine.log` → `engine.log.1` へ **改名（1 世代だけ保持）** する
（waired-ai/waired-agent#29）。この非対称は 2 か所に実害を出していた。

* `internal/platform/logdump` は `engine.log` と `engine.log.1` を
  ollama/vllm の両方から集めるが、vLLM 側の `.1` は**原理的に存在しえない**
  ので、あの収集は名指しした 2 エンジンの半分で死んでいた。
* `cmd/waired-agent/inference_vllm_tuning.go` の
  `parseVLLMKVCapacityTokens` は自分のコメントで逆を宣言していた
  ——「再試行ループが複数の起動を 1 ファイルに書きうるので最後の出現が勝つ」。
  これは ollama では真、vLLM では偽だった。

## Decision

**1. 追記する。世代交代はしない。** vLLM の再試行は数秒間隔で並ぶ
**1 つの診断**であって、ollama の respawn（時間を空けたクラッシュ復旧）とは
別物である。spawn ごとに世代交代させても 1 回目は同じように読めなくなる
——それは #878 が挙げた動機そのものを潰せない。3 回とも、順番どおり、
区切り付きで 1 ファイルに残す。

**2. 上限は spawn でなくファイルを縛る。** `cappedWriter` を現在の
ファイルサイズから開始させる。8 MiB の予算を試行ごとに配り直すと、
クラッシュループするエンジンが `engine.log` を無制限に伸ばせてしまう
——truncate をやめる以上、ここは同時に決めないと片手落ちになる。

**3. 上限に達していたら、そのとき 1 世代だけ回す。** ディスク上のコストは
`2 × engineLogMaxBytes` に収まり、これは ollama が守っている境界と同じ。
副産物として、vLLM でも `engine.log.1` が初めて存在しうるようになり、
logdump の収集が両エンジンで生きる。

**4. 区切り行（banner）を書き、読み手は「直近の spawn」に絞る。**
`internal/runtime.LastEngineLogSpawn` が最後の banner 以降を返す。
banner の無いログは全体を返す——#878 以前に書かれたファイルも
ollama の `engine.log` も、中身は 1 spawn ぶんだからである。

絞りが必要なのは、追記が **古い数字を現在の測定値に見せかける**新しい
失敗様態を持ち込むからで、これは追記の代償として明示的に払う。

| 読み手 | 絞らないと起きること |
|---|---|
| `parseVLLMKVCapacityTokens` | 今は載っていないサイジングで測られた KV 容量を、現行エンジンの検証済み容量として `Verified` にする |
| `vllmStartupDiagnosis` | `switch` の**先に並んだ枝**が、どの試行由来かに関わらず勝つ。一時的だった 1 回目の理由が、ループが実際に諦めた理由を上書きする |

## Consequences

* `VLLMConfig.LogDir` の契約が変わった（「spawn ごとに truncate」→
  「banner の後ろに追記、上限で 1 回だけ世代交代」）。`OllamaConfig.LogDir`
  とはもう同じ契約ではないので、doc コメントで相互に理由を指す。
* banner の文字列は **書き手（internal/runtime）と読み手（cmd/waired-agent）
  をまたぐ書式契約**になった。cmd 側のテストは定数を import せず
  リテラルで綴る——書式が動いたときに読み手側が黙って絞らなくなるのでなく、
  落ちるようにするため。
* `parseVLLMKVCapacityTokens` の「最後の出現が勝つ」は
  **spawn の内側**の規則に縮んだ。既存の「banner の無いログ」ケースは
  後方互換として残る。
* e2e (`internal/e2e/inference`) の `parseKVCapacity` も同じ絞りを入れた。
  GPU レーンは今日動いていないが、動いたときに前の spawn のプールと
  比べてしまう形になっていた。
* ollama 側は**触っていない**。あちらの 1 世代方針は #29 / #642 で
  批准済みで、respawn の性質が違う。統一は目的ではない。
