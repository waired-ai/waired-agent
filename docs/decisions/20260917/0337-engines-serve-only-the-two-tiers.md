---
status: accepted
supersedes:
  - docs/decisions/20260916/2146-retire-with-no-successor-and-drop-sub-200k-machinery.md
  - docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md
  - docs/decisions/20260808/1907-price-capacity-at-the-served-window.md
  - docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md
superseded_by:
  - docs/decisions/20260920/2345-an-explicit-choice-below-the-coding-window-is-warned-not-refused.md
---

# エンジンは 200,704 か 1,048,576 のコンテキストウィンドウだけを配信し、宣言する (20260917 03:37)

## Status

Accepted。オーナー決定(2026-09-16、waired-ai/waired-agent#1396 に記録)。

`docs/decisions/20260920/2345-an-explicit-choice-below-the-coding-window-is-warned-not-refused.md`(オーナー裁定 2026-09-20)が、**「明示的に選ばれたビルドを起動前に断る」の 2 か所だけを置き換える**(決定 1 の最後の文と、「まだ入っていないもの」の 1 つ目)。断らずに警告と既定 No の確認にする。エンジンが 200,704 か 1,048,576 だけを配信・宣言することは変わらない。エンジン(ollama と vLLM)が配信し宣言するコンテキストウィンドウは 200,704 か 1,048,576 だけで、200,704 を保てないホストやモデルは配信しない。例外は CI 専用の `internal_only` のモデル(granite4-350m、ネイティブ 32k)で、自身のコンテキストウィンドウで配信してよく、一覧・推奨・ピッカーには出さない(同日のオーナー回答)。実装は waired-ai/waired-agent#1434。

行の側(Waired の行はどれも 200,704 か 1,048,576 のセッション)は `docs/decisions/20260916/2350-every-waired-row-is-a-200k-or-1m-session.md`。この記録はエンジンの側。

次の記録を**部分的に**置き換える。記録の `## Status` に鏡の一文を置いた。

- `docs/decisions/20260916/2146-retire-with-no-successor-and-drop-sub-200k-machinery.md` の「残るもの」のうち「vLLM の `--max-model-len` による切り詰め」。
- `docs/decisions/20260916/0340-catalog-reference-host-rank-and-admission.md` 決定 4 の「残すのは」のうち、同じ「vLLM の `--max-model-len` による切り詰め」。
- `docs/decisions/20260808/1907-price-capacity-at-the-served-window.md` 決定 1 の「rung 未満のモデルについてはモデル自身の窓」。当てはまるのは `internal_only` のモデルだけになる。
- `docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md` Consequences の「ハードゲートは確実 OOM のためだけにある」。vLLM の自動選択では、200,704 を保てないビルドも選ばない。

## Context

2350 で行の側を 2 通りにしたあとも、エンジンの側には 2 つの間の値と 200,704 未満の値が残っていた(origin/main 222514f6 で確認)。

- **vLLM。** `computeVLLMTuning` は `--max-model-len` を、KV プールの推定値とモデル自身のコンテキストウィンドウの小さいほうにしていた。262,144 のモデルは、プールが足りれば 262,144 で、足りなければ 1024 の倍数の推定値(24 GB のカードで 124,928 など)で配信した。
- **宣言。** `DeclaredContextWindow` は 200,704 以上の値をそのまま返した。262,144 で配信するホストは 262,144 を宣言した。
- **サイズ情報が無いとき。** vLLM はモデル自身のコンテキストウィンドウ(検証していない 1M を含む)で起動した。ollama は `OLLAMA_CONTEXT_LENGTH` を渡さず、エンジンの既定に任せた。ollama 0.33.3 の既定は VRAM から決まり、VRAM が無ければ 4,096、実測では 32,768〜262,144 だった(`docs/knowledges/20260906/0230-ollama-pin-0333.md` §5)。
- **推奨。** `modelrank.RankModels` の段 1(vLLM のプールが 200,704 を保てるか)は、候補が空になるなら飛ばす段だった。保てるビルドが 1 つも無いホストでは保てないビルドが選ばれ、切り詰めて配信された。飛ばす形にしたのは waired-ai/waired#1056 決定 1(断るのは確実な OOM のときだけ)による。

2350 のあと、200,704 未満で配信するコンピュータはどの Waired の行にも答えない。上の経路は、そういうビルドを選んで配信し続けていた。

## Decision

1. **vLLM は 2 つのどちらかで配信する。**
   - モデル自身のコンテキストウィンドウが 1,048,576 に届き、KV プールがそれを保てるなら 1,048,576。
   - そうでなく、プールが 200,704 を保てるなら 200,704。モデル自身のコンテキストウィンドウが 262,144 でも 200,704 にする。
   - どちらも保てなければ `ModelTuning.WindowFits` を false にし、理由を警告に書く(`vllmBelowTierWarning`)。エンジンは推定値で起動するが、宣言は 0 なので、どの Waired の行にも答えない。明示的に選ばれたビルドを起動前に断るのは waired-ai/waired-agent#1435。
   - サイズ情報が無いときは 200,704 で起動する。モデル自身のコンテキストウィンドウが 200,704 未満(`internal_only` だけ)ならその値。
2. **ollama は、サイズ情報が無いときも `OLLAMA_CONTEXT_LENGTH=200704` を渡す。** 保てると示せていないので `WindowFits` は false のまま。モデル自身のコンテキストウィンドウが 200,704 未満のモデル(`internal_only` だけ)は、これまでどおりエンジンの既定に任せる。サイズ情報があるときは、もともと `OllamaPlannedRungFor` の段(200,704 / 1,048,576)で配信しているので変えない。
3. **宣言は 2 つの値に丸める。** `DeclaredContextWindow` は、1,048,576 以上なら 1,048,576、200,704 以上なら 200,704、それ未満なら 0 を返す(`declaredTier`)。いまの ollama と vLLM のチューニングはどちらも 2 つの値で決めるので、いまは値がそのまま返る。丸めは、2 つの値で決めない経路ができたときの安全網。
4. **vLLM の自動選択では、段 1 を飛ばさない。** `PreferredModelID` が空のとき、プールが 200,704 を保てないビルドは候補から外し、1 つも残らなければ `ErrHardwareInsufficient` を返す。ollama の段 1 は、容量の判定がもともと 200,704 の段で価格を付けているので、何も外さない。段 2(推奨)と段 3(実測の速さ)は、これまでどおり空になるなら飛ばす。

## Consequences

- **vLLM のホスト。** 262,144 のモデルを 262,144 で配信していたホストは、この変更を含む版では `--max-model-len 200704` で起動し、200,704 を宣言する。1M のモデルは、プールが 1,048,576 を保てるときだけ 1,048,576 になる。200,704 を保てるビルドが無いホストでは、`RankModels` が `ErrHardwareInsufficient` を返す。インストール時の選択(`SelectInstallModel`)は、そのときローカル推論を設定しない(既存の経路)。自動のエンジン選択は vLLM を選ばない(`router.VLLMAutoSelectable` は false、waired-ai/waired-agent#1311)ので、これが効くのは vLLM を明示的に選んだホスト。
- **明示的に選んだビルドがプールに収まらないとき。** エンジンは起動するが宣言は 0 で、`waired status` のエンジンの通知と `waired runtimes ls` に警告が出る。docs-site の `reference/model-catalog` にもそう書いた。
- **ollama のホスト。** サイズ情報のあるカタログのビルドでは、配信するコンテキストウィンドウは変わらない。
- **私設側の記録を散文で置き換える。** waired-ai/waired#1056 決定 1(断るのは確実な OOM のときだけ。`waired` の `docs/decisions/20260803/1332-hard-vs-soft-model-limits.md`)は、vLLM の自動選択について追い越された。waired-ai/waired の決定 20260808/2325(容量以外は警告だけ)と 20260705/1640(vLLM は縮めて警告する)も、コンテキストウィンドウについて同じ。リポジトリをまたぐ supersede は散文でしか書けない。
- **反転したテスト**(レビュアーが見えるように列挙):
  - `cmd/waired-agent/inference_vllm_tuning_test.go`: 切り詰めた値で配信するテストを、200,704 未満は配信しない、262,144 のモデルは 200,704、1M はプールが保てるときだけ、に書き換えた。
  - `cmd/waired-agent/inference_declared_window_test.go`: 262,144 は 200,704 を宣言する。
  - `cmd/waired-agent/inference_ollama_tuning_test.go`: サイズ情報が無いときは 200,704 を渡す。
  - `internal/router/coding_floor_test.go`: 200,704 を保てるビルドが無いときのベストエフォートの選択は、`ErrHardwareInsufficient` になった。
- **まだ入っていないもの。**
  - 明示的に選んだビルドを、200,704 を保てないなら断り、選択画面で「実行できない」と示す(waired-ai/waired-agent#1435)。`docs/decisions/20260828/1730-vllm-sizing-moves-into-hostfit.md` 決定 4 の「容量だけが断ってよい」に触るので、その変更で記録する。
  - `OLLAMA_CONTEXT_LENGTH` が効かなかったエンジン(以前の実行から採用したものなど)について、verify は気づくが、記録と宣言は予定したコンテキストウィンドウのまま(waired-ai/waired-agent#1436)。

## Refs
- waired-ai/waired-agent#1434, #1396, #1435, #1436
- `docs/decisions/20260916/2350-every-waired-row-is-a-200k-or-1m-session.md`
- `cmd/waired-agent/inference_vllm_tuning.go`, `cmd/waired-agent/inference.go`, `cmd/waired-agent/inference_ollama_tuning.go`, `proto/modelrank/rank.go`
