---
status: accepted
---

# 生きた residency はライブの経路で運ぶ — 署名済み map には載せない (20260828 01:10)

## Status
Accepted

## Context

waired-agent#879 は「アイドル期限切れのモデルがどの面からも見えない」を、
5 つの面を名指しで挙げて起票された。うち 4 つは既に閉じている:

| 面 | 状況 |
|---|---|
| `waired status` | `model loaded:` 行 + 期限。オーナーが sv-mag 実機で確認済み(#879 コメント 2026-08-22) |
| daemon の status JSON | `RuntimeStatus.model_resident*`(#897) |
| ピアの health probe | `HealthSnapshot.model_resident`(#880 → PR#968) |
| `RequestEvent` | `model_residency`(`resident` / `absent` / `other`) |
| tray | 本 PR まで `runtimes["ollama"]` 直読みで vLLM ホストでは死んでいた。トップの Engine 行が `(not loaded)` を運ぶようになった |

残る問いは 1 つだけで、issue 本文が "Why it is worth a field" として立てたもの:
**`proto/signer.InferenceState` に residency のフィールドを足すか**。これは
CP が組み立てて署名する NetworkMap に載る = 全ピアの目に入る、という意味である。

## Decision

**載せない。** 生きた residency はライブの経路(peer health probe)で運び、
署名済み map には置かない。

理由は 3 つ:

1. **判断する側には既に届いている。** residency が実際に決定を変える唯一の
   消費者はピア選択で、それは probe-then-commit がリクエストのたびに
   `/waired/v1/inference/healthz` を叩いて得ている。`isWarmPeer` /
   `isColdPeer`(`internal/gateway/probe.go`)がその答えを読み、**同点のときだけ**
   暖かい側に倒す(#880/#965)。issue が「40 秒かけて再ロードするピアが暖かい
   ピアとバイト同一の `{"engine_ready":true}` を返す」と書いた穴は、ここで
   塞がっている。map に足しても、同じ事実がもう 1 本、より遅く、より古い
   経路で届くだけになる。
2. **map は揮発値の配送路ではない。** keep-alive は既定 1 時間で切れる。
   residency を載せると、全デバイスの entry がその周期で変化し、CP は署名を
   打ち直し、全エージェントが検証し直す。map が運んでいる他の揮発値
   (`Reachable` / `Models` / `LastCheck`)は数日〜数週間動かない性質のもので、
   1 時間ごとに反転する bool とは別物である。
3. **PEER entry に載る新フィールドは fleet の upgrade 順序を作る。**
   `Priority` の doc が明示しているとおり、フィールドを知らないエージェントは
   canonical re-marshal で落として **map 全体の検証に失敗する**ので、
   capability ゲートと「fleet が上がるまで出さない」運用が要る。表示上の
   便益のためにその機構を増やす釣り合いになっていない。
   既にある residency 系 3 フィールド(`ResidencyIdleTimeout` /
   `ResidencyUnsupported` / `LocalResidencyChoiceAt`)がいずれも **push-only**
   (agent → CP のみ、PEER entry には載らない)で、doc に
   「capability 定数を持たない」と書かれているのはこの線引きと同じものである。

結果として、tray の `Peers:` 行とピア行は「答えられるか」と「どのモデルか」を
言い、「暖まっているか」は言わない。これは正確である — 行が主張していない
ことを、行が知らないだけなので。

## Consequences

- proto タグ不要、CP 側変更不要。L78 は waired-agent 1 リポの PR 1 本で閉じる。
- 「どのピアが暖かいか」を人が見る手段は今のところ無い。必要になったときの
  正しい形は map ではなく、**tray/CLI が自分で healthz を引く**か、
  ルータが選択理由として記録したものを observability から読む形である。
  どちらもライブの経路のままで、署名済み map の性質を変えない。
- この判断は「証拠が既に在るなら永続化するな」と同型で、
  `docs/decisions/20260820/0130-model-residency-is-a-setting.md` が
  「residency は設定である」と定めたのを、**観測値のほうは設定ではない**と
  補う形になる。

## Refs
- https://github.com/waired-ai/waired-agent/issues/879
- https://github.com/waired-ai/waired-agent/issues/880 / PR #968(同点を破る)
- https://github.com/waired-ai/waired-agent/issues/861(再ロードが 17〜56 秒)
- docs/decisions/20260820/0130-model-residency-is-a-setting.md
