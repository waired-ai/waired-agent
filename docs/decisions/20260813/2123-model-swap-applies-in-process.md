---
status: accepted
supersedes:
  - docs/decisions/20260714/0244-model-swap-restart-first-no-pre-pull.md
---

# モデル切替はプロセス内で適用する。再起動は fallback (20260813 21:23)

## Status

Accepted。waired#812 / waired-agent#64 で出荷済みの挙動を、後追いで記録するもの。

## Context

`20260714/0244` は「restart-first を維持し、ハンドラからの pre-restart pull を
削除する」と決めた。その後 **waired#812（PR waired-agent#64、squash `1fcc2d8`、
2026-07-17）が in-process swap を入れ、常用経路は再起動しなくなった**が、
決定記録は更新されなかった。

結果、**「モデル切替は再起動するのか」を調べる人が最初に読む面が、古い答えを
返し続けた**。実害が出た形で3件観測されている:

- waired#808 — issue 本文が「切替のたびに 25〜35 秒エージェントが落ちる」を前提に
  書かれたまま残り、着手時に4項目中3項目が既に出荷済みだった。
- waired#840 — 「Windows の意図的再起動が SCM 7031 になる」の前提が
  waired-agent#684 で既に解消しており、提案されていた修正案は
  `20260812/0310` で**明示的に不採用**になっていた。
- CLI の `waired models refresh` が「To apply, restart waired-agent」と案内し続け、
  存在しないエンドポイント名まで添えていた（waired-agent#775 で修正）。

`0244` 自体が誤っていたのではなく、**超えられた部分と今も有効な部分が
書き分けられていなかった**ことが問題。

## Decision

出荷挙動を記録する。規範ではなく**現在の挙動の記録**である。

### 1. モデル切替は in-process に適用する（same-engine のみ）

`internal/management/inference_preferred_model.go` が `catalog.ApplyModelSwitch`
を持つとき、そこで適用して `202` + `will_restart:false` を返す。
エージェント・management API・gateway・mesh は落ちない。
実体は `cmd/waired-agent/inference.go` の `SwapPreferredModel` →
`reconcileEngineServe` で、**bounce するのは `ollama serve` だけ**。

重みがまだディスク上に無い場合は `downloading:true` を返し、pull の完了で
bounce が走る。**その間は旧モデルが応答を続ける。**

### 2. 再起動は fallback として残る

以下は今も supervised restart に落ちる（`will_restart:true`）:

- **cross-engine**（ollama↔vLLM）— adapter の再登録が要るため
  `errSwapNeedsRestart`
- 対象に ollama で配信できる variant が無い
- engine が wedged
- `ApplyModelSwitch` が未配線（`--disable-inference` など）

つまり **`waired runtimes refresh` の「エンジン切替には再起動が要る」は今も正しい**。

### 3. 重みを取得できないときは 409 で断り、選択は保持する

`ErrModelSwitchUnavailable` → `409 model_switch_unavailable`。再起動しても
同じ bootstrap が同じ理由で失敗するだけなので落とさない。
**記録した preference は意図的に残す** — pull が再び可能になれば自力で適用される
（waired-agent#257）。

### 4. `0244` のうち今も有効な部分

「**ハンドラは pre-restart pull を投げない**」は今も有効。restart fallback は
今も pull を投げず、post-restart の `bootstrapPreferredModel` が一元的に行う。
超えられたのは「**restart-first を維持する**」の部分だけ。

## Consequences

- `0244` は部分 supersession として `accepted` のまま `superseded_by` を持つ
  （CLAUDE.md §Decision Log）。
- 非同期になった分、**UI は「受け付けた」と「適用が終わった」を言い分ける必要が
  ある**。`202` は受理であって完了ではない。tray がこれを取り違えて、数 GB の
  ダウンロード開始時に「Model switched.」と即答していた（waired#808 /
  waired-agent#769）。
- 同じ理由で `will_restart` と `downloading` は**捨ててはいけない応答フィールド**
  になった。`waired models use`（waired-agent#753）と tray は3応答を出し分ける。

## Refs

- https://github.com/waired-ai/waired/issues/812
- https://github.com/waired-ai/waired-agent/pull/64
- https://github.com/waired-ai/waired/issues/808 / https://github.com/waired-ai/waired-agent/pull/769
- https://github.com/waired-ai/waired-agent/pull/775
- `internal/management/inference_preferred_model.go`
- `cmd/waired-agent/inference.go`（`SwapPreferredModel` / `reconcileEngineServe`）
- `docs/decisions/20260714/0244-model-swap-restart-first-no-pre-pull.md`
