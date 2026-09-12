---
status: accepted
---

# 「オーナー優先」はアカウントのことで、この 1 台のことではない (20260912 21:30)

## Status

Accepted。オーナー裁定 2026-09-12（waired-agent#1302 の照会への回答）。

既存の裁定を**覆すのではなく、読み方を訂正する**もの。owner-priority の裁定
（waired#899、private 側 `waired/docs/specs/waired_public_share_spec.md` §8.2）の
うち「公開共有の消費者に対して所有者を優先する」半分はそのまま有効で、
「所有者のローカル要求は ceiling で拒否しない／カウンタは capacity を超えうる」
半分が改定される。

private 側の spec 本文と waired#899 にも同じ訂正が要る。リポジトリをまたぐ
supersede は front-matter で表現できないので、この対応は散文で書く
（プロジェクトの CLAUDE.md §Cross-repo rules）。

## Context

`inflightCounter` は 1 つで、この機械のエンジンを占有している要求を数える ——
overlay から来たピアの要求（`capacityGateAdapter` 経由）と、この端末自身の
クライアントの要求（`AdmitLocal` 経由）の両方。waired#899 がその 1 本化を
入れたとき、ローカル側は `AcquireOwner` を通した: **上限を無視して加算し、
決して拒否しない**。

0.0.3-rc6 の実機検証（waired-agent#1302 / #1303）で、その帰結が出た。

- `-np 1` の engine（スロット 1 本）を持つホストで、自ノードのターン 1 本が
  共有カウンタを capacity の外まで押し上げる。
- 同じカウンタを読む自ネットワークのピアは 503 `waired_inference_overloaded`
  になり、`/healthz` の `capacity_used >= capacity_total` を見て候補から外れる。
- すでに走っていたピアのターンと自ノードのターンが同じ engine を取り合い、
  互いのプレフィックスを追い出す。プレフィックス喪失は崖で、appended なら
  2.57 秒のターンが 35.38 秒になる（冷 33.85 秒。
  `docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md`）。

つまり「所有者は自分の機械で断られない」を**この 1 台の意味で**読んだ結果、
自ノードのクライアントが自ネットワークの他ノードを追い出していた。

## Decision

**「オーナー」とは、このアカウントに登録されたノード群のことである。**
オーナー裁定 2026-09-12（原文）:

> 「オーナーは自分の機械で決して断られない / 上限を超えてよい」と裁定した
> ときの「自分の機械」は、自分のアカウントに登録されているノード、いわば
> プライベートネットワーク内のノードを指しています。なのでここには、同一
> デバイスのクライアントから、同じデバイス内の推論エンジンへのリクエストを、
> 他デバイスのクライアントからのリクエストより優先するという意味は含んで
> いません。ですのでこの裁定をもとにして、今回のように自ノードのリクエストが
> あったときに、他ノードのリクエストを追い出すというのは解釈違いで、また
> 裁定の書き方も変更するべきと思います。

1. **自ネットワークのピアの要求と、この端末自身のクライアントの要求は、
   1 つの天井に対する同格の請求者である。** どちらも `Acquire` を通り、
   capacity を超えない。`AcquireOwner`（無条件加算）は廃止する。
2. **owner-priority ラッチは残る。** それが保証するのは**公開共有の消費者に
   対する**優先であって、このアカウントのノード同士の優先ではない。
   peer 側の `capacityGateAdapter` は最初からこの綴りだった
   （`ownerRequest = 公開共有の消費者でないこと`）。訂正が要ったのは
   `AdmitLocal` 側だけである。
3. **ローカル脚は拒否ではなく待ちで受ける。** ローカル脚には逃げ場が無い
   （`docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`）
   ので、満杯で断ると「数秒後には答えられたターン」を落とすことになる。
   `AdmitLocal` は空きスロットを待ち、**要求のコンテキストが終わったときだけ**
   諦める。上限は設けない —— 逃げ場の無いローカル脚に時間の上限を置かないのは
   `docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md` の裁定と
   同じ形である。

## Consequences

- **`capacity_used` が `capacity_total` を超えなくなる。** ピアが読む数字が
  「実負荷の観測値」から「実負荷の観測値であって、かつ天井以内」になる。
  `assignSpeedRanks` の doc は既に「#1126 以後 admission が
  `capacity_used + 1 <= slots` を保証するので除数は有界」と主張していたが、
  `AcquireOwner` がある限りそれは偽だった。この裁定でその主張が本当になる。
- **未計測のホストでは、自分の 2 本目のターンが待つ。**
  `unmeasuredCapacity = 1` なので、計測前のホストで 2 本同時に投げると
  2 本目はスロットを待つ。以前は即 admit され、1 スロットの engine を
  取り合っていた。engine のスロット数は同じで、変わったのは「取り合って
  互いのプレフィックスを捨てるか、順番に走るか」である。
- **待っている間は無音になり得る。** 通常は waired-agent#1302 の順序付けが
  混雑した自ノードを空いているピアの後ろに回すので、この待ちに入るのは
  「ローカルしか候補が無いラウンド」「`local-only`」「自機に解決した行」に
  限られる。ストリーム脚では既存の SSE keepalive がその無音を埋める。
- **private 側に未了の作業が残る**: spec §8.2 の必須要件 2（「ceiling で
  拒否しない」「所有者が自分のマシンで 503 になることは仕様上ない」）と 4 の
  文面訂正、および waired#899 への追記。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1302
- https://github.com/waired-ai/waired-agent/issues/1303
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`
- `docs/decisions/20260821/2142-local-leg-pre-first-byte-wait.md`
- `docs/knowledges/20260819/2330-prefix-reuse-depends-on-architecture.md`
