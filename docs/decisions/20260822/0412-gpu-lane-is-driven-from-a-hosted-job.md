---
status: accepted
supersedes:
  - docs/decisions/20260821/2142-gpu-lane-runs-on-a-per-run-l4-vm.md
---

# GPU レーンは hosted ジョブが VM を駆動する (20260822 04:12)

## Status

Accepted。`docs/decisions/20260821/2142-gpu-lane-runs-on-a-per-run-l4-vm.md`
の **決定 2（`needs: gpu-up` を queue ガードにする）と決定 3（登録を
ephemeral/JIT にしない）を置き換える**。あちらの決定 1（実行ごとに VM を
作る・理由は public リポであること）・決定 4（課金の上限を GitHub に
依存させない）・決定 5（有効なのに skip された夜は赤）は**維持**する。

## Context

2142 は「VM をセルフホストランナーとして登録する」形で実装され、
マージされた。実行はされていない。準備を進める過程で、その形に固有の
コストと、実行を待たずに分かる欠陥が出た。

**1. public リポに登録するための PAT が要る。** 登録トークンを発行するには
`administration: write` を持つ fine-grained PAT が必要で、runbook はそれを
GCP Secret Manager に置き、`waired-ai/waired-agent` だけにスコープし、
GitHub secret にも VM にも渡さない、という手順で扱っていた。手順が
これだけ要るのは、**public リポジトリに runner を登録できる資格情報**が
本質的に強いからである。

**2. `runs-on` の宛先が無いことを GitHub は skip で答えない。** 24 時間
queue する。2142 はこれを `needs: gpu-up` で塞いだが、それは
「ラベルに解決させてから、解決しなかったら前段で落とす」という迂回で
あって、ラベルを使わなければ最初から起きない。

**3. 1 回の dispatch が GPU ジョブを 2 本選べるので JIT にできない、という
制約は、ランナーを使うことから来ていた。** ジョブが VM を直接駆動するなら
分離境界は最初から VM なので、この制約自体が消える。

そして実測で、**マージ済みの作成コードには初回 dispatch を壊す欠陥が
2 つ**あった。どちらもレーンが一度も走っていないので誰も踏んでいない。

- `gcloud compute instances create --source-instance-template=X --disk=...`
  は**テンプレートのブートディスクを捨てる**。焼いた 50 GB のイメージでは
  なく debian-12 の 10 GB で起動する。L4 も `g2-standard-4` も残るので、
  **GPU は在るのに CUDA も Go も ollama も無い VM** が健全に見える。
- `--metadata` も同様に**テンプレートの metadata マップを置き換える**。
  テンプレートが持っていた 3 キーは毎回消えていた。`enable-guest-attributes`
  だけは**プロジェクトレベルにも設定されていた**ため生き延びたが、
  `google-logging-enabled` / `google-monitoring-enabled` は消えていた。

## Decision

**1. GPU を要るジョブは hosted runner で動き、VM を API で駆動する。**
`vllm-install` と `agentgrade` は `ubuntu-24.04` で走り、それぞれ自分の VM を
作り・待ち・回収し・`if: always()` のステップで消す。`gpu-up` / `gpu-down`
ジョブは無くなる。

セルフホストランナーを登録しないので **PAT は要らない**。`runs-on` に
自前ラベルが無いので **24 時間 queue する経路が構造的に無い**。
2142 の決定 2 と 3 はどちらも、この形では**述べる対象が存在しない**。

**2. VM は inbound チャネルを持たない。押す側になる。**
`gcloud compute ssh` は WIF impersonated principal で動かず
(private 側 `docs/records/20260508.md`)、それを可能にする IAM
(`roles/compute.osAdminLogin` / `roles/iap.tunnelResourceAccessor`) は
2026-05 に `github-ci` から**意図的に削除**されている。したがって
制御は guest attributes、ログは GCS で **VM から出す**。testnet が
2026-05 に到達したのと同じ向きである。

ポーリング間隔は 20 秒に決めた。guest attributes は
**1 インスタンスあたり毎分 10 クエリ**が上限で、VM 側の heartbeat が
毎分 1 回入るため。1 回のポーリングで名前空間ごと 1 API 呼び出しにする
(キーごとに読むと上限を使い切る)。

**3. レーン本体は焼かない。** イメージが焼くのは supervisor —— 権限境界・
metadata server の遮断・キャッシュディスク・clone・publish —— だけで、
`scripts/ci/gpu-lane-run.sh` は**試験対象のコミットから取ってくる**。

2142 は「GPU ホストで何が動くかを変えるには private 側のレビューが要る」を
帰結に挙げた。**その性質は保たれる**: 特権境界は依然 private 側にある。
変わるのは、レーン本体が**それを実行するテストと同じコミットで動く**こと
である。焼き込むと、週次のイメージ再ビルドまで**古いレーンが新しい sha に
対して pass を返す** —— このレーンの歴史がまさにその型なので、採らない。

**4. キャッシュディスクは vLLM レーンのものにする。**
zonal PD は 1 インスタンスにしか rw で付かないので、2 台構成では共有
できない。agentgrade は付けずに走る。そして
**「ディスクが無ければ黙って cold」をやめる** —— 期待して現れなければ
失敗、期待していないのに在っても失敗。黙って cold に落ちる経路は、
~15 GB の再取得とその外部障害を vLLM の結果として報告する。

**5. 「作れた」を「正しく作れた」と読まない。** create の直後に、
ブートディスクの sourceImage と `enable-guest-attributes` を
`describe` で確認する。上の欠陥 2 件はどちらも
**machine type も GPU も正しい RUNNING の VM** を作ったので、
問い合わせる以外に区別する方法が無い。

## Consequences

* `gpu-up` / `gpu-down` ジョブと `GPU_RUNNER_PAT_SECRET` は消える。
  Secret Manager の入れ物と IAM、reaper の runner レコード掃引も消える。
* `report` の `needs` から `gpu-up` が消える。決定 5 の skip 検出は
  `vllm-install` に対してそのまま残る。
* reaper は名前の正規表現ではなく**テンプレートが打つ
  `role=gpu-ci-runner` ラベル**で絞る。名前は public 側が決めるもので、
  実際に一度変わった(レーンごとに 1 台なので `-<lane>` が付いた)。
  自分が所有しない命名規則に正しさを預けた reaper は、その規則が
  動いたときに黙る。
* `max_run_seconds` を 10800 → 7200 に下げた。reaper の年齢下限が
  200 分で GCE 側の上限が 180 分だったため、**reaper は上限が効かなかった
  VM しか拾えなかった**。下限 150 分・上限 120 分に整える。
* ログは **Cloud Logging ではなく GCS**。あちらは read-your-writes が無く、
  `gcloud logging read` が既定の `--limit=1000` を超えると黙って切り捨て、
  `_Default` の保持は 30 日で agentgrade レポートの 90 日に足りない。
  進捗の経路としては正しく、**唯一の複製**としては正しくない。
* hosted ジョブが VM を 60〜90 分待つ。public リポなので hosted runner の
  実行時間は無料で、コストは増えない。
* VM が返す値は**要求と突き合わせる**: checkout した sha、実際に走らせた
  ターゲット、起動したイメージ、成果物ごとの sha256。どれか 1 つでも
  欠けるか食い違えば赤。これらが無いと、緑は「何かが成功した」以上の
  ことを意味しない。

## Refs
- waired-ai/waired#590 (ephemeral GCP L4 で GPU e2e を回す実装 issue)
- waired-ai/waired#1229 (レーンが全 schedule で skip されていた件)
- docs/decisions/20260821/2142-gpu-lane-runs-on-a-per-run-l4-vm.md
- docs/decisions/20260723/1910-gpu-vllm-install-serve-ci-lane.md
