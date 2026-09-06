# 次に ollama を上げるとき、AMD バックエンドについて見直すこと (20260906 17:00)

## Issue

waired は一部のホストで GPU バックエンドを自分で指定する
(`ResolveOllamaBackend`、internal/runtime/ollama_backend.go)。Strix Halo の
Windows の腕は Vulkan を名指しし、Linux の腕は ROCm を試して駄目なら
Vulkan へ落ちる。この選択は upstream の事実の上に建っていて、その事実は
予告なく動く。waired-agent#1233 の調査 (2026-09-06) では、コードに書かれた
理由のうち 2 つが、何も失敗しないまま古びていた — 「ROCm has no Windows
APU support」と、オーバーレイの版刻印「v6.1」である。

これは提案ではない。次に `OllamaPinnedVersion` を動かす人が、同じことを
掘り直さずに済むための、**見直す項目と、その結果を読むのに要る背景**の
一覧である。各節は「次の bump は何を見るか」と「見たものをどう解釈するか」
に答える。計測そのものは
`docs/knowledges/20260906/0430-rocm-runs-on-strix-halo-windows.md` にあり、
本記録はその「次はどこを見るか」の半分。前回までの bump の点検項目は
`docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md` と
`docs/knowledges/20260906/0230-ollama-pin-0333.md` にあり、本記録はそこに
AMD の項を足す。

### 1. Strix Halo の腕を決めている upstream のスレッド

Strix Halo on Windows の腕が Vulkan を名指しする理由は、性能差ではなく、
下の 4 本である。**4 本とも 0.33.3 の時点で open** (2026-09-06 確認)。
「腕が Vulkan を名指しする」には期限があり、その期限はこれらのスレッドに
結び付いていて、他の何にも結び付いていない。

| スレッド | 何が起きるか | ここで重い理由 |
|---|---|---|
| ollama/ollama#17895 — "ROCm backend returns wrong output for prompts above ~4k tokens on gfx1151 (Strix Halo); Vulkan and CPU are correct on the same machine" | 2026-08-20 起票、2026-08-28 時点で open、label は bug。報告者は agentic なワークロードで踏んだ。約 3,500〜4,000 プロンプトトークンを超えるとモデルがプロンプトの前半を読まなくなり、約 8k を超えると別々のプロンプトにバイト単位で同一の答えを返す — ときに前のリクエストの文面を含んで。ログには何も出ない。"fluent, confident, wrong answers"。0.32.5〜0.32.14、Debian 13 と Windows 11、qwen3.5:9b / qwen3.6:35b / gemma4:31b、同梱の `rocm_v7_2` で再現 | コーディングエージェントのリクエストはすべて 4k を超える。エラー無しの誤答は、`waired doctor` も、agent-harness の grade も、request-shape の matrix も捕まえない |
| ollama/ollama#17847 | gfx1151 の ROCm が、連続するリクエストの間で KV の状態を漏らす。プロンプト A と B を交互に送ると B の答えが A の内容を語る、3/3。`OLLAMA_NUM_PARALLEL=1`、リクエストごとの nonce でプレフィックス再利用を潰した上で、`HSA_OVERRIDE_GFX_VERSION=11.0.0` 下でも。同じ機械の CPU と Vulkan は clean | 同上。しかも並列度 1 で起きるので、#621 の tuning が `OLLAMA_NUM_PARALLEL=1` を選ぶホストでも消えない |
| ollama/ollama#17498 | gfx1151 の ROCm が Gemma 4 12B の出力を約 1,200 プロンプトトークンあたりから壊す | 4k より手前で始まるモデルがある、という記録 |
| ollama/ollama#17870 | Vulkan 側の対抗馬。長いプロンプトの prefill が amdgpu の compute ring の watchdog timeout を起こし、カーネルがキューをリセットして Vulkan が `ErrorDeviceLost` を報告する。`num_batch=128` で回避できる | 種類の違いが選択を決める: **Vulkan はリクエストを失敗させ、ROCm は間違って答える**。前者は見える |

#17870 には AMD の外でも使える細部が 1 つある: デバイスが失われている間も
`/api/tags` は 200 で応え続ける。ステータスコードだけを見る health check
はこれを捕まえない。`docs/knowledges/20260906/0230-ollama-pin-0333.md` §7
の、メモリ不足のエンジンが HTTP 200 を返した観測と同じ形である。

次の bump が見ること: 4 本をそれぞれ読み直し、**閉じたかどうかを記録する**。
閉じていれば腕を見直す条件が 1 つ満たされたことになり、閉じていなければ
腕はそのまま — どちらでも、確認した日付と版を
`amdROCmSupportedRes` の上の `!!! MAINTENANCE` ブロックに残す。閉じた
場合に腕を変えるかどうかは、そのとき §5 の測り方で採った数字と、修正が
入った版を pin していることの両方が要る。スレッドが閉じたこと自体は、
pin している版にその修正が入っていることの証拠ではない。

### 2. ROCm 7.2.4 は性能の答えで、正しさの答えではない

upstream のガイダンスは、ROCm 7.2.4 が gfx1151 のネイティブ hipBLASLt
カーネルを載せ、`HSA_OVERRIDE_GFX_VERSION` の hack を不要にすると言う。
0.33.3 が同梱するのは `rocm_v7_1` (この機械のオーバーレイから読んだ)。
だが #17895 は `rocm_v7_2` で再現している。つまり ROCm の版が上がることは
§1 の正しさの問題を解決しない。

次の bump が見ること、2 つを**別々に**:

1. オーバーレイがどの `rocm_v*` ディレクトリに展開されるか
   (`ollama-windows-amd64-rocm.zip` の中身を読む。手順は
   `amdROCmSupportedRes` の上のブロック)。
2. #17895 と #17847 が閉じたか (§1)。

1 が動いたことを 2 の証拠として読まないこと。0.31.1 から 0.33.3 まで
オーバーレイは `rocm_v7_1` のまま動かなかった (0430 の補足)。動いた版が
来たとき、それが正しさに効いたかは 2 でしか分からない。

### 3. waired が実際に上書きしているもの、どれがどの種類か

次の読者が導出し直さなくて済むように。3 種類あり、古び方が違う。

| 種類 | 中身 | 何に結び付いているか |
|---|---|---|
| 正しさの補正 (bug workaround) | `OLLAMA_IGPU_ENABLE=1` — 無いと runner は統合 GPU をすべて落とし、`total_vram="0 B"` で CPU に落ちる (2026-09-06 に再確認) | ollama 側の挙動。エンジンが統合 GPU を既定で使うようになれば不要になるが、その日付は無い |
| 正しさの補正 (bug workaround) | Linux の腕の `HSA_OVERRIDE_GFX_VERSION=11.5.1` — 無いと ollama 0.18+ は gfx1151 を黙って発見できず CPU で走る (ollama/ollama#15336、#13589) | upstream が期限を言っている (§2: ROCm 7.2.4)。ただし期限が来たことの確認は、同梱の `rocm_v*` を読んでから |
| 選択の表明 | Strix Halo on Windows の腕が Vulkan を名指しすること | §1 の 4 本 |
| 選択の表明 | `amdROCmSupportedRes` | パッケージングの判断。§4 |
| 性能チューニング | #621 の serve tuning (`OLLAMA_CONTEXT_LENGTH` / `OLLAMA_KV_CACHE_TYPE` / `OLLAMA_NUM_PARALLEL` / `OLLAMA_FLASH_ATTENTION`、cmd/waired-agent/inference_ollama_tuning.go) | バックエンド選択とは無関係。種類を混同しないためだけに載せる |

上 2 行は好みではなく bug workaround で、waired-agent#1079 と同じ扱いを
受ける先例がある — エンジンが自分でできるようになったとき、製品は上書きを
外した (`docs/decisions/20260828/1900-retire-the-forced-generation-batch.md`)。
外すには「エンジンが自分でできるようになった」ことを、pin した版で、
上書き無しで測る必要がある。`OLLAMA_IGPU_ENABLE` については、上書きを
外して起動し、discovery 行に `type=iGPU` が残るかを読むだけで済む
(モデルは要らない。0430 の補足 1 と同じ 14 秒の起動)。

### 4. `amdROCmSupportedRes` はダウンロードの判断で、Linux にはそれが無い

次の読者のための事実。推奨は無い。

- Windows の base archive は CUDA / Vulkan / CPU を載せ、ROCm は別の
  約 250 MB のオーバーレイ (0.33.3 で 247 MB)。`wantROCmOverlay`
  (cmd/waired/runtimes_install_windows.go) は `BackendPlan.WantsROCm` に
  訊く。つまりこのリストが決めるのは、**インストール時にその資産を取って
  くるか** — 訊ける相手のエンジンがまだ機械に無い時点の判断である。
  `WAIRED_OLLAMA_GPU_MODE` が先に読まれ、`rocm` だけがオーバーレイを求める。
- Linux では同じ問いをリスト無しで答える:
  `inst.WantROCmOverlay = detectOllamaGPUVendor(ctx) == "amd"`
  (cmd/waired/runtimes_install_linux.go)。そしてバックエンドの腕は ROCm を
  試し、engage しなければ Vulkan に落ちる。
- 2026-09-06 の計測: `lib/ollama` に両方のバックエンドが在ると、ollama は
  それぞれを発見して自分で選ぶ — Strix Halo で 4 回の再起動、
  `OLLAMA_VULKAN` の有無、HSA override の有無、すべて Vulkan に dispatch
  した (0430 §3)。つまりこのリストはバックエンドの判断ではない。
- リストが実際に何に一致するか、実物の SKU 文字列で
  (internal/runtime/ollama_backend_test.go `TestAMDROCmSupported`、
  および #1233 で当てた 2 つ): RX 7900 XTX / RX 7600 / RX 6800 XT /
  RX 6950 XT / PRO W7900 / PRO W6800 / PRO V620 → true。Radeon 8060S
  (Strix Halo) → false。RX 9070 XT (RDNA4) → false。Radeon 780M と素の
  "Radeon Graphics" → false だが `amdIsIntegratedModel` が先に拾う。
  Strix Halo はそもそもリストに届かない — `ResolveOllamaBackend` は
  `StrixHaloAPU` の腕で先に返る。
- オーバーレイから導出できない理由: オーバーレイは gfx ターゲット
  (gfx1030、gfx1100…) でキーされ、`hardware.GPU` は Windows では
  マーケティング名しか持たない — gfx のフィールドが無い
  (internal/hardware/profiler.go。rocm-smi の側は `GFXTarget` を
  waired#287 が求めているが、入っていない)。regex のリストは、その対応を
  マーケティング名の側に書き写したものである。プロファイラが Windows で
  gfx ターゲットを知るようになれば、リストは資産から導出できるものになり、
  手で維持するものではなくなる。これは条件であって計画ではない。
- RDNA4 (gfx1200/1201) がリストに無いことは waired-agent#1248 に別途
  記録してある。測れるカードがフリートに無い。

次の bump が見ること: オーバーレイの gfx ターゲット集合 (§2 の 1 と同じ
読み取り) と、このリストと、upstream の Windows 対応表の 3 つを並べ、
0.33.3 で食い違っていた形 (0230 §6) がどう動いたかを記録する。

### 5. また測ることになったら、どう測るか

短く書く。数字より方法のほうが重く、#1233 は正しく測るまでに 2 回間違えた
(0430 §4 と補足 2)。

- バックエンドを強制するには、もう一方を `lib/ollama` の**外へ移動**する。
  その場での改名は隠さない — エンジンは名前にかかわらずサブディレクトリを
  列挙する。
- どのバックエンドが実際に応じたかは
  `runner.inference="[{ID:0 Library:...}]"` と
  `load_tensors: <Device>0 model buffer size` で確認する。
  `<Device>_Host` の行はホスト側バッファで、何の証拠にもならない。
- CPU 単独の対照 (両方を外へ移動) を採る。この機械では prefill 158 tok/s、
  decode 19.3、`size_vram 0`、GPU 使用率 0 %。「本当に GPU に載っている」
  をラベルでなく計測にするのはこの対照である。
- 窓の大きさを決める。`num_predict` 64 は decode の窓が約 1.2 秒で、
  セッション間に 22 % の振れを出した — 報告する価値のある差の 2 倍。
  30k 超のプロンプトに 512 トークンで、decode の幅は 2.7 % まで落ちた。
- 1 回の run の中でバックエンドを交互に回し、中央値の隣に最小・最大を
  書く。範囲が重なるなら報告すべき差は無い。
- ハーネスの罠: PowerShell の変数名は大文字小文字を区別しない (`$m` が
  モデルタグの `$M` を上書きし、全リクエストが 404 になった)。BOM の無い
  `.ps1` は Windows PowerShell 5.1 に ANSI コードページとして読まれるので、
  リモートへ送るスクリプトは ASCII だけにし、その機械で構文検査する。
  `/api/version` はモデル一覧が揃う前に応えるので、`/api/tags` にタグが
  現れるまで待つ。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1233
- https://github.com/waired-ai/waired-agent/issues/1247
- https://github.com/waired-ai/waired-agent/issues/1248
- https://github.com/waired-ai/waired-agent/issues/1193
- https://github.com/waired-ai/waired-agent/issues/1079
- https://github.com/waired-ai/waired/issues/1312
- https://github.com/ollama/ollama/issues/17895
- https://github.com/ollama/ollama/issues/17847
- https://github.com/ollama/ollama/issues/17498
- https://github.com/ollama/ollama/issues/17870
- https://github.com/ollama/ollama/issues/15336
- https://github.com/ollama/ollama/issues/13589
- docs/knowledges/20260906/0430-rocm-runs-on-strix-halo-windows.md
- docs/knowledges/20260906/0230-ollama-pin-0333.md
- docs/knowledges/20260829/1600-engine-pins-0332-and-0280.md
- docs/decisions/20260828/1900-retire-the-forced-generation-batch.md
