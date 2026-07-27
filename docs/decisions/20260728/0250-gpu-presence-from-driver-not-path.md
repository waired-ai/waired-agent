---
status: accepted
---

# GPU の存在判定はドライバに訊く。CLI が $PATH にあるかは判定に使わない (20260728 02:50)

## Status
Accepted

## Context
Windows ホスト(RTX 3060 Ti / 8GB)で 3B モデルが選ばれ、推論が CPU 前提でサイジング
された(#67)。`detectNvidia` が `exec.LookPath("nvidia-smi")` の 1 行だけで NVIDIA の
有無を決めており、見つからないと **エラーですらなく「GPU なし」** を返していたため。

`GPUs` 空 → `PrimaryGPUVendor=""` → `ResolveOllamaBackend` の `case ""` → `BackendCPU`、
モデル選定は VRAM ではなく RAM ゲートに落ちる。ログにもエラーにも何も出ない。

構造的な原因は、**waired と ollama が別の質問をしていた**こと:

| | 質問 | サービスアカウントでの答え |
|---|---|---|
| ollama | ドライバライブラリ(`nvml.dll` / `nvcuda.dll` / `libcuda.so`)をロードして列挙できるか | 列挙できる |
| waired (旧) | `nvidia-smi` という名前が `$PATH` にあるか | 無いことがある |

Windows の LocalSystem サービスはユーザ PATH を継承しない。**engine 側で既に**
「ollama の挙動に合わせる」作業(#40 / #68 / #290 の backend steering、
`scripts/install/ollama-windows.ps1` の `Get-DetectedGPUs` = WMI 検出)は入っていたが、
それらは全て **検出の下流**で `PrimaryGPUVendor` が既知である前提だった。同じホストを
インストーラは NVIDIA と見なし、agent は見なさない、という食い違いが残っていた。

この PATH-only 判定は repo 内 5 回目で、`scripts/ci/lookpathguard`(#209)がその再発防止に
存在する。ところが #67 の該当行は「GPU vendor driver tool; its presence IS the
driver-installed signal」という理由で **宣言済み = 祝福されて凍結**されていた。ガードが
バグを固定していた。

## Decision
- **存在判定はドライバに訊く。** `detectNvidia` は次の 3 段で、どれかが当たれば
  vendor=nvidia:
  1. `nvidia-smi` — `$WAIRED_NVIDIA_SMI` → `$PATH` → OS 別の既知パス(Windows は
     System32 / DriverStore glob / NVSMI、Linux は distro パスと WSL の
     `/usr/lib/wsl/lib`)。`$PATH` は **チェーンの 1 ステップ**であって判定ではない。
  2. **NVML**(Windows、`nvml.dll` を `NewLazySystemDLL` でロード、cgo 不要) —
     ollama 自身の真実源。name / VRAM / compute cap / UUID が取れる。
  3. **OS のデバイス台帳**(Windows: 表示クラスレジストリ VEN_10DE、Linux:
     `/proc/driver/nvidia/gpus` → sysfs PCI)。
- **「不在」と「不明」を別の答えにする**(`VendorDetector` の契約を改訂)。全段が空振り
  したときだけ静かに「GPU なし」。**アダプタはあるのに列挙できない**場合は必ずエラーを
  返し、`Profile.Errors` に出す。「静かに CPU」は今後どの経路でも起きない。
- **列挙できたが VRAM が読めない**場合は「デバイス + soft warning」で返す(`detectAMD` の
  既存契約と同型)。下流は `hostfit` が「予算不明」として**判定を保留**する
  (`OllamaResident` は Fits、`EstimateOllamaDecode` は主張しない、engine picker は
  vLLM を選ばない)ので、0GB のカードとして扱われることはない。
- **レジストリ層はドライバ実体があるときだけ**デバイスとして採用する(`nvcuda.dll` の
  存在を確認)。表示クラスのキーは取り外したカードより長生きするため、これが無いと
  phantom GPU を作る。
- `nvidia-smi` のタイムアウトを 3s → 10s(Windows のコールド NVML 初期化)。
  フィールドを拒否する旧ドライバ向けに `compute_cap` を落とした 1 回の再試行を追加。
  どちらも「非ゼロ終了 = GPU 無し」と読んでいた経路を塞ぐもの。

## Consequences
- 検出は `runtime.GOOS` を受け取る純関数(`nvidiaSMICandidates`)と純合成関数
  (`classifyNvidia`)に分かれ、GPU の無いマシンから 3 OS 分をテーブルテストできる。
  #67 の失敗はこれまで**どのテストからも到達できなかった**。
- macOS には NVIDIA ドライバ経路が無いのでスタブ。明示的な `$WAIRED_NVIDIA_SMI` だけは
  共通チェーンで効く。
- Linux には NVML 相当が無い(`dlopen` は cgo を要し、agent は全て `CGO_ENABLED=0`)。
  カーネルの台帳で存在は分かるが VRAM は読めないので、その場合は警告付きで返す。
- `lookpathguard` の GPU ブロックの理由文を「チェーンの 1 ステップ」に差し替えた。
  `cmd/waired/setup_install.go` の vLLM 事前チェックも `hardware.NVIDIADriverPresent`
  経由に変更(昇格実行の executor は PATH がユーザのものではない)。
  `internal/runtime/vllm.go` は Linux 限定 + `internal/hardware` を意図的に import しない
  パッケージなので現状維持。
- `agent.json` に固定済みの小さいモデルは自動では戻さない。ベンチ後の
  `upgradeFromBench` が生きたプロファイルから上位モデルを提案する既存経路で復帰する。

## Refs
- https://github.com/waired-ai/waired-agent/issues/67
- 同じ PATH-only バグ class: #179 / #238 / #209(`scripts/ci/lookpathguard`)
- 下流の ollama 寄せ: `internal/runtime/ollama_backend.go`(#40 / #68 / #290)
- 予算不明時の判定保留: `proto/hostfit`(`OllamaResident` / `EstimateOllamaDecode`)
