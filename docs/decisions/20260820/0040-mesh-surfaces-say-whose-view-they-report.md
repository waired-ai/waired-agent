---
status: accepted
---

# メッシュの面は「誰の視点か」を言う (20260820 00:40)

## Status

Accepted

## Context

waired-agent#849。どのピアにも到達できないホストが、`waired peers list` で
全ピアを `WORKER-CAPABLE: yes` と表示し、ルータもその全部を候補に選び、
最後に返る 503 は「ピアの準備ができていない」と言う。実際に壊れているのは
そのホスト自身の経路だが、それを言う面が一つも無い。

2026-08-19 の実機で、孤立ホストの `waired doctor` は
`✗ device key` と `⚠ mesh peers — 5/5 reported reachable, but only 0 answered
an overlay ping` を正しく出していた。同じホストの `peers list` は 5 行すべて
`yes`。つまり**エージェントは真実を持っていて、面によって言ったり言わなかったり
していた**。

機構は、面が見ている事実の出どころが違うこと。

- `WORKER-CAPABLE` の yes/no は `signer.InferenceState.Reachable`、すなわち
  **ピアが自分の loopback エンジンを叩いた結果**。コントロールプレーン経由で
  届くので、データプレーンが死んでいるホストにも問題なく届き続ける。
- 条件名 `unreachable` も同じ値から来ており、「こちらから届かない」とは
  一度も意味していなかった。
- viewer 自身のデータプレーン事実は `/waired/v1/status` に per-peer で
  publish 済み（`direct_sample_count` / `relay_sample_count` /
  `current_path` / §8.5 済みの `display_id`）だが、`peers list` は読んでいない。

`waired doctor` の側は 2026-08-12 のオーナー裁定「measure it」
(waired-ai/waired#1137) で実測に変わっている。#849 はその裁定の**取り残された面**。

## Decision

1. **`waired peers list` は viewer 自身の事実を、ピアの申告とは別の事実として
   表の下に述べる。** 既存の stale 注記と同じ作法（該当する行があるときだけ出す）。
   材料は `/waired/v1/status` の既存フィールドで、**新しい wire フィールドは
   足さない**。読みはベストエフォート（失敗・非 200・古い daemon はすべて
   「注記を出さない」に落とす）で、`mgmtReadRoute` を通す（#785）。
2. **`no (unreachable)` を `no (engine not answering)` に改める。**
   誰の探索が失敗したのかを語に含める。`inferencemesh.ConditionLabel` は
   メニュー／docs 向けに 3 条件を "unavailable" に畳む契約なので触らない。
3. **ノード鍵の乖離は名指しする。** 乖離しているときは他が何も動かないので、
   ピアの注記をこの 1 行に差し替える。判定は `waired doctor` と同じ
   `management.NodeKeyAgreementDiverged` を読むので、2 つの面が食い違えない。
4. **メッシュの 503 は、探索したピアと結果の内訳を本文に載せる。**
   `router.ErrPeersDidNotAnswer` を `%w` で包むので `errors.Is` に依存する
   ステータス表・テレメトリ・explain 対応は不変。ピア名は必ず
   `candidateDisplayID` を通す（本文はそのまま 503 body になるため、
   spec §8.5 / #739）。
5. **「容量で詰まった」を名乗る条件を「`/healthz` に答えたピアが最低 1 件ある
   こと」に改める。** 従来は「全部 `ProbeTransportError`」を要求していたため、
   1 台でも 401/403 を返すと、誰も使えないメッシュが
   `waired_all_peers_overloaded` に化けていた。#624 の誤診と同型。

## Consequences

採らなかったもの（いずれも意図的）:

- **到達性でルーティングを絞る／並べ替える。** #849 本文が明示的に求めていない
  うえ、waired-ai/waired#729 が「disco の pong は WireGuard データプレーンを
  通らないので、その沈黙を veto にするとデータプレーンが健全なピアを
  ブラックホール化する」と裁定済み。注記は手掛かりであって断定ではない。
- **`peers list` で実測 ping を打つ。** 測るのは `waired doctor` の役目
  （8 秒予算）。一覧は速いままにする。
- **`observabilityState` の `PeersReachable`（`!Stale && !Silent`）の定義変更。**
  公開済みの数え方であり、doctor の実測行が既に正しい対比を出している。
- **`doctor_mesh_probe.go` の probe 対象フィルタへの反映。**
  返事の無いピアこそ測る価値があるので、そこに効かせると逆になる。
- **`inferencemesh.PeerView` へのフィールド追加。** 初期案では disco の 3 値
  （最近 pong / 沈黙 / 一度も無し）の 3 番目を集約層で持ち上げる形だったが、
  `/waired/v1/status` に同じ証拠が既にあり、猶予窓・`firstSeenAt` の引き継ぎ・
  tray / management / mock-mgmt への波及をすべて避けられるので取り下げた。

副次の修正: `router.NewLocalCandidate` が `Selection.PeerDisplayID` を
Candidate に引き継いでいなかった。実 DeviceID しか名前が無い候補を作るため、
本文にピア名を載せた時点で spec §8.5 のガードが落ちて判明した。

## Refs

- https://github.com/waired-ai/waired-agent/issues/849
- https://github.com/waired-ai/waired-agent/issues/624
- docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md
