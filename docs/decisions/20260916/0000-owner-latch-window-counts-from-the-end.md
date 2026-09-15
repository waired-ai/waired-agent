---
status: accepted
---

# 所有者優先ラッチの 30 秒は所有者の要求の終了から数える (20260916 00:00)

## Status

Accepted。オーナー裁定 2026-09-16(waired-agent#1387、案 B)。

`docs/decisions/20260912/2130-owner-priority-is-the-account-not-the-device.md`
の「オーナー」の定義(このアカウントに登録されたコンピュータ)はそのまま。
変わるのはラッチが効いている時間の数え方だけで、あの裁定を置き換えない。

## Context

owner-priority ラッチは、所有者の要求が満杯で断られたとき、または最後の枠を
取ったときに立ち、30 秒間、新しい公開の要求を断る(private 側の Public Share
仕様 §8.2)。これまでは 30 秒を要求の**到着**から数えていた。

実機(2026-09-14、capacity 1、公開の枠 1)で、所有者のターンは 27 秒かかった。
ラッチはターンの終了 3 秒後に切れ、その後に来た guest の要求は通った。
コーディングエージェントの所有者は数秒後に次のターンを投げ、それは guest の
ターンが終わるまで待つ(ローカルの要求は断らず空きを待つ。0912 の裁定)。
ターンが 30 秒を超えると、ラッチはターンの途中で切れる。

同じ面で、さらに 2 つの隙間があった。

- ローカルの要求は、満杯で待っている間はラッチを立てなかった。待っている間に
  guest が枠を空けると、次の guest がその枠を取れた。
- `/healthz` はラッチを映さず、公開の枠が空いていれば public の利用者の probe に
  ready と答えた。利用者はそのホストを選んでから 503 を受けた。

## Decision

1. 満杯の中で走っている所有者の要求がある間、ラッチは立ち続ける。
   その要求が終わった時点から 30 秒、立て直す。断られた要求は、断られた
   時点が終了なので従来と同じ。
2. ローカルの要求が満杯で待ち始めた時点で、ラッチを立てる(待つことが
   このパスでの「断られた」にあたる)。待ちを諦めた時点から 30 秒。
3. ラッチ中、public の利用者への `/healthz` は公開の枠を満杯として報告する。
   所有者自身の負荷は引き続き見せない。

## Consequences

- 所有者が続けて作業している間、guest の新しい要求はほぼ入らない。それが
  所有者優先の意図(オーナー裁定)。実行中の guest の要求は完走させる。
- 期限の書き込みは後ろにだけ動く(時計が少しずれた 2 つの更新が互いを
  縮めない)。
- 所有者の要求が満杯でない機械に入っただけなら、ラッチは立たない。
  unlimited(capacity 0)も立たない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1387
- `docs/decisions/20260912/2130-owner-priority-is-the-account-not-the-device.md`
- `internal/inference/server.go`(`publicAdmission`、`AdmitLocal`、`capacityGateAdapter`)
- `internal/inference/health_handler.go`
