---
status: accepted
supersedes:
  - docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md
---

# Linux のサービスユーザーを render グループに入れる (20260922 14:30)

## Status

Accepted。waired-agent#1535。
`docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md`
の Consequences にある「`render` グループを常時与える案は採らない」の
**1 文だけ**を改める。そのほかの部分は変えない。

## Context

- #1466（#459）では、render ノードの ioctl で読む事実を扱った（AMD の FUSION、のちに
  Intel の単体 GPU の VRAM）。デーモンはそのノードを開けない。そこで `sudo waired init`
  が 1 回だけ読んで `runtime/gpu-topology.json` に残す形にした。理由は「変わらない
  事実のために常時の特権を増やさない」だった。
- その判断は、プロファイラの読み取りだけを見ていた。推論エンジン（ollama・vLLM）は、
  資格情報を変えずにデーモンの子として起動し（`internal/runtime/spawner_unix.go`）、
  同じ `waired` で動く。エンジンが GPU で計算するには、次のノードを開く必要がある。
  - AMD: ROCm は `/dev/kfd` と `renderD*`、Vulkan は `renderD*`。
  - Intel: Vulkan で `renderD*`。
- systemd の既定の mode は 0666（v236 以降、`group-render-mode`）。しかし Debian と
  Ubuntu は `-Dgroup-render-mode=0660` でビルドしている。そのためサポート対象
  （Ubuntu 24.04 以降・Debian trixie 以降）では、`renderD*` と `/dev/kfd` は
  `0660 root:render` になる。
- 実機で確かめた。機械は RTX PRO 4000 と AMD の iGPU を載せた Linux 機で、
  Ubuntu 26.04・systemd 259。製品の ollama 0.34.0 を `waired` で起動した。
  - グループが無いと、AMD の iGPU は検出に現れず、ログにも何も出ない。
  - `render` を付けると、iGPU が Vulkan の装置として現れる。既定の設定では
    `dropping integrated GPU` で外れ、CUDA だけで動く。これは今と同じ結果になる。
- 上流の ollama の installer は、`ollama` ユーザーを `render` と `video` に入れている
  （グループがある場合）。ROCm と Intel の docs も、`render` への参加を求めている。

## Decision

- サービスユーザー `waired` を `render` に入れる。グループが無い機械では入れずに
  先へ進み、グループを作ることはしない。
  - .deb の postinst: configure のたびに `usermod -a -G render waired` を実行する。
    upgrade のときも実行されるので、既存の install も直る。
  - `waired-agent install`: `ensureGPUGroups` で同じことをする。
- `video` には入れない（オーナーの判断、2026-09-22、#1535）。`video` は
  Web カメラ（`/dev/video*`）と画面出力（`/dev/dri/card*`）にも届く。
  サポート対象のディストリでは、計算には要らない。
- unit に `SupplementaryGroups=` は書かない。
  - 無いグループを名指すと、unit が起動しない（exit 216/GROUP）。
  - systemd は `User=` の補助グループをグループ DB から付ける（systemd.exec(5)）。
    グループに入れて再起動すれば足りる。
- `gpu-topology.json`（`sudo waired init` が残す読み取り）は残す。
  - グループがあれば、デーモンがその場で読む。その場の読み取りが優先される。
  - 残した読み取りは、サービスユーザーがグループに入っていない機械のための下限になる。

## Consequences

- Linux の AMD / Intel の GPU を、エンジンが開けるようになる。
  - 実機で確かめたのは、AMD の iGPU が検出に現れるところまで（既定では ollama 自身が外す）。
  - 単体 GPU・Strix Halo・Intel Arc で実際に計算できるかは、#1495・#1496・#1497 の
    計測で確かめる。
- デーモンは FUSION と Intel の VRAM を自分で読むようになる。多くの機械では
  `gpu-topology.json` の読み取りを使わなくなる。
- 未解決の点（#1535 に記録）: ollama の Vulkan は、CAP_PERFMON か root でないと
  空き VRAM を読めない。`NoNewPrivileges=yes` はファイル capability も無効にする。
  capability を与えるかは、別に判断する。
- Windows（LocalSystem）と macOS（root）には、同じ問題は無い。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1535
- https://github.com/waired-ai/waired-agent/issues/1534
- docs/decisions/20260920/2000-host-provenance-is-derived-at-chip-granularity.md
- docs/knowledges/20260920/2100-linux-gpu-facts-the-daemon-cannot-read.md
- https://github.com/systemd/systemd/blob/e362a4efcbe7366d03f6b60959b752440fe1f69b/rules.d/50-udev-default.rules.in#L61-L63
- https://salsa.debian.org/systemd-team/systemd/-/blob/debian/master/debian/rules
- https://github.com/ollama/ollama/blob/6383a0fa9cbf97494b847226e189f6e36b401a08/scripts/install.sh#L197-L230
- https://rocm.docs.amd.com/projects/install-on-linux/en/latest/install/prerequisites.html
- internal/platform/service/service_linux.go, packaging/debian/waired/postinst
