---
status: accepted
supersedes:
  - docs/decisions/20260829/1100-measurement-provenance-is-derived-or-declared.md
superseded_by:
  - docs/decisions/20260921/0300-nvidia-single-pool-parts-are-named.md
  - docs/decisions/20260921/1600-amd-parts-are-named-by-their-isa-target.md
  - docs/decisions/20260922/1430-linux-service-user-joins-render.md
---

# 測定の出自は、チップの粒度で事実から導く (20260920 20:00)

## Status

Accepted。waired-agent#1455、および #459 の Ask 1・2。
`docs/decisions/20260829/1100-measurement-provenance-is-derived-or-declared.md`
の §2 を**部分的に**改める（`--host` が §2 の「語彙で縛る」側から §1 の
「観測できるものは導出する」側へ移る）。§1・§3・§4・§5 は不変。

この記録の **§4 と §6 は、その後
`docs/decisions/20260921/0300-nvidia-single-pool-parts-are-named.md` が
部分的に改めた** — §4 が #459 に繰り延べた「方針の一般化」はそこで解決し、
§6 の「`GPU.Model` は使わない」は **NVIDIA のユニファイド部品に限って**
解かれた（`ComputeCap` の `12.1` が GB10 と RTX Spark N1X の 2 機種を
指してしまい、部品を特定できないため）。§1・§2・§3・§5・§7 は不変。

**§6 の AMD の行は、さらに
`docs/decisions/20260921/1600-amd-parts-are-named-by-their-isa-target.md` が
部分的に改めた** — 「AMD は `CPU.Model` が部品を名指す」は APU に限った文になり、
ディスクリートカードは ISA ターゲット（`gfx1100` など）で名指す（#1485）。
`GPU.Model` を使わない点は AMD についても変わらない。

**Consequences の「`render` グループを常時与える案は採らない」の 1 文は、
`docs/decisions/20260922/1430-linux-service-user-joins-render.md` が改めた**（#1535）。推論エンジンも同じサービスユーザーで
動き、AMD / Intel の GPU で計算するのに render ノードが要るため。
installer がサービスユーザーを `render` に入れる。残りは不変。

## Context

カタログの出自の語彙 `catalog.HostClasses` は、綴りが**ベンダ名 + メモリ容量**
だった（`nvidia-24gb-discrete` / `apple-unified-64gb` / `amd-unified-128gb`）。
DGX Spark の記録を入れようとすると `nvidia-unified-128gb` を足すことになり、
**リストが機械の名簿に育つ**。

容量は軸としても間違っている。参照機（`amd-unified-128gb`）の実測と
llama.cpp 公式の GB10 ベンチを同じ深さで並べると prefill が約 2 倍違い、
一方 `apple-unified-64gb` は帯域が 4.5 倍違う部品を 1 つの名前に畳む。

**いちばん鋭い例はこのプロジェクト自身の 2 台で、旧綴りはどちらも同じ名前で呼ぶ**
（2026-09-21 に確認）:

| | カード | 容量 | compute cap | 帯域 |
|---|---|---|---|---|
| CI の GPU レーン | NVIDIA L4 | 24 GB GDDR6 | 8.9 | **300 GB/s** |
| Linux のフリート機 | NVIDIA RTX PRO 4000 Blackwell | 24 GB GDDR7 | 12.0 | **672 GB/s** |

どちらも `nvidia-24gb-discrete`。**帯域は 2.24 倍違う**ので、片方で測った秒数は
もう片方について何も言わない — それこそが名前が伝えるべき唯一のことである。
導出すれば `discrete-nvidia-sm89` と `discrete-nvidia-sm120` に分かれる。
レーンの素性は `installtest-inference.yml` が自ら書いている
（"this lane brings its own hardware: a g2-standard-4 with one L4"）。

根の問題は語彙の綴りではなく 1 つ下の層にあった。**`UnifiedMemory` という
フラグが 3 OS で別々の家族名判定から立っていた** — macOS は `GOARCH`、
Linux と Windows は `strings.Contains(cpuModel, "ryzen ai max")`。

## Decision

### 1. 概念の名前は `integrated`。造語しない

「ユニファイドメモリか」を訊く述語は、どの実装にも存在しない。あるのは
**「integrated GPU か」**という真偽値で、ggml（`GGML_BACKEND_DEVICE_TYPE_IGPU`、
doc は *"integrated GPU device using host memory"*）、CUDA
（`cudaDeviceProp::integrated`、*"Device is integrated as opposed to discrete"*）、
ollama、vLLM、PyTorch、Vulkan / Level Zero / HIP / Metal の 6 系統で一致する。

**コヒーレンシの軸とは別**。NVIDIA の GH200 は `integrated == 0` で完全に
コヒーレント（Grace の LPDDR5X と Hopper の HBM3 は別プール）、GB10 と
Jetson は `integrated == 1`。`nvidia-smi -q` の `Addressing Mode`（ATS / HMM /
None）はページテーブルの機構の項目であって、メモリのトポロジの項目ではない。

proto の `UnifiedMemory` は additive-only なので綴りは変えない。

### 2. 共通なのは「問い」であって「読み口」ではない

3 OS を 1 本の API で貫く述語は存在しない。共通にするのは問いの方 —
**「アクセラレータのメモリと OS の RAM が、1 つの物理プールの 2 通りの
読み方になっていないか」**（#459 の Ask 2 の言い換え）。規則の文を 1 か所に
置き、OS ごとに答え方だけが違う形にする。

| OS / ベンダ | 読む事実 |
|---|---|
| Apple Silicon | `GOARCH == "arm64"`。**「macOS だから」ではない** — macOS 26 はまだ専用 VRAM を持つ Intel Mac を 3 機種 support している |
| Linux / AMD | `DRM_IOCTL_AMDGPU_INFO` の `AMDGPU_IDS_FLAGS_FUSION`。Mesa が `has_dedicated_vram` を決めるのに読むのと同じビット |
| Windows | アダプタの報告メモリ ≤（SMBIOS の搭載量 − OS 可視量）なら、そのメモリは RAM から切り出された |
| Linux / NVIDIA | 無い。cgo 無効で CUDA は呼べず、NVML に該当項目は無い。**未知に倒す** |

### 3. 答えは三値。沈黙を「ディスクリート」と読まない

シェルから届く事実はどれも**沈黙で失敗する**（ドライバが揃わない GB10 は
Vulkan で何も返さず、render ノードが開けない Linux は答えを返さない）。
沈黙を「ディスクリート」と読むことが #459 の欠陥そのものなので、
`Integrated` + `IntegratedKnown` の三値にし、併合を非対称にする —
**知っている情報源は上書きでき、知らない情報源は昇格しかできない**。

### 4. 報告と方針は別のスイッチ

`GPU.Integrated` は**報告**で、`UnifiedMemory` は**方針**。今回は報告だけを
事実にし、方針は動かさない。クラスは予算の規則を伴っていて、統合だと
分かっただけでは `UsableVRAMMB` を言えないからで、予算の無いまま
`UnifiedMemory` を立てると `EffectiveVRAMMB` が 0 になり「CPU のみ」と
表示される。llama.cpp も同じ 2 枚を分けている（CUDA バックエンドは
デバイス種別を live な `cudaDeviceProp` から報告し、スケジューラは別に
ゲートされたフラグを読む）。**方針の一般化は #459 に残る。**

### 5. 鍵は `<topology>-<vendor>-<chip>`。容量は名前に入れない

```
unified-amd-ryzen-ai-max-395    discrete-nvidia-sm120
unified-apple-m4-max            unified-nvidia-sm121    cpu-none
```

検証はリストの包含ではなく**文法**になる。誰も打ち込まないので、機械の名前は
原理的に入らない — 決定 1100 の Consequences が「レビューの機会」として
守ろうとしていたものが、機構として保証される。

容量と帯域は**記録の数値欄**として運ぶ。「同じ 128 GB だから比べてよい」と
ラベルに言わせる代わりに、読み手が数値の違いを見られるようにする。

### 6. チップの粒度はベンダごとに違う。PCI ペアを併走させる

- **Apple と AMD** は `CPU.Model` が部品を名指す（#251 が帯域表を引いている
  のと同じ文字列）。チップ型番の粒度。
- **NVIDIA は違う**。aarch64 Linux では `/proc/cpuinfo` に `model name` 行が
  無く `CPU.Model` は**空**で、Grace / GB10 はまさにそれに当たる。だから
  `ComputeCap` を使うが、これは**アーキテクチャであって部品ではない** —
  RTX PRO 4000 Blackwell と RTX 5090 はどちらも 12.0 を報告し、帯域は 3 倍近く違う。

そこで **PCI の vendor:device ペア**を記録の別欄として併走させる。ベンダが
割り当て、両 OS が特権なしで公開し（Linux は sysfs の `vendor` / `device`、
Windows は `MatchingDeviceId`）、同じ部品に同じ値を返し、上の 2 つを分ける。
**部品を名指すのであって個体を名指さない**ので、公開リポジトリで安全。

**`GPU.Model` は使わない。** doc が "free-form; do not parse" と書いており、
#251 がこの種の判断のために `CPU.Model` を選んだ経緯がある。

### 7. 古い綴りは凍結して残す。記録は綴り替えない

`LegacyHostClasses` の 3 語は**読むためだけ**に残す。理由は決定 1100 の
「出荷済みの記録は書き換えない」に尽きる。**2 つの鍵はいずれも分かっている**
（参照機は実機から `unified-amd-ryzen-ai-max-395`、GPU レーンは L4 なので
`discrete-nvidia-sm89`）が、**分かっていることは書き換える理由にならない** —
一括の綴り替えは、測定を伴わずに出自を編集することになる。

> **訂正（2026-09-21）**: この節は当初「GPU レーンの派生キーは読めていない」と
> 書いていた。レーンの素性は `installtest-inference.yml` に書かれており
> （L4）、L4 の compute capability は NVIDIA が公開している（8.9）。
> 綴り替えない結論は変わらないが、理由が違う。

**旧綴りは既存の store の続きには使えるが、新しい store を始めることには
使えない。** 宣言で凍結したものが、使用によって生き続けるのを防ぐ。

## Consequences

- 機械が増えてもリストは伸びない。GB10 の記録は `unified-nvidia-sm121` を
  名乗り、どこにも行を足さない。
- `turnspeeds --import` の `--host` は**任意**になり、**スナップショットと
  食い違えば拒否**される。異なる 2 台のスナップショットを混ぜる import も拒否。
- **Linux では、製品はほとんどの場合 `integrated` を「未知」と報告する。**
  render ノードは `0660 root:render` で、デーモンは `User=waired` /
  `SupplementaryGroups=` 空だから。`sudo waired init` が 1 回読んで永続化する
  のが次の段で、`host-memory.json` と同じ形になる。**`render` グループを
  常時与える案は採らない** — 変わらない事実のために常時の特権を増やさない。
- 検出の一般化は #459 の残りを**解かない**。予算の規則（`strixHaloUMA` の
  96 GiB の BIOS 上限）は依然として家族固有の事実で、`ClassDiscrete` の
  3 つの誤った仮定も残る。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1455
- https://github.com/waired-ai/waired-agent/issues/459
- docs/decisions/20260829/1100-measurement-provenance-is-derived-or-declared.md
- docs/decisions/20260728/0200-uma-bandwidth-spec-vs-measured.md
- docs/knowledges/20260805/1610-igpu-classification-three-layers.md
- internal/hardware/integrated.go, internal/hardware/hostkey.go, internal/hardware/pciid.go
