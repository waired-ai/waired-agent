---
status: accepted
---

# オペレータのキャンセルは、ブートの pre-pull キャンセルとは別の問題 (20260812 02:45)

## Status

Accepted。`docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md`
（以下 1202）が **#379 のキャンセル案を採らないと決めている**。その決定は
**取り消さない**。1202 が対象にしていたのは「ブートで始まった bundled pre-pull を、
後から届いたウィザードの選択で**追い越してキャンセルする**」案であって、
本件は「**オペレータが自分で始めた pull を、自分で止める**」手段である。
両者は同じ機構を使うが、同じ問いではない。

`supersedes` / `superseded_by` は張らない。1202 の保留設計（sticky な stand-down、
決定は同期でディスパッチだけ非同期）はそのまま有効で、差し替えではなく射程の分割だから。

## Context

rc8 実機検証で 2 件出た（waired-agent#633 / waired-agent#641）。

- 6.6 GB の `models pull` を誤って始めたとき、抜ける道が `models rm` しかない。
- その `models rm` は**止めていなかった**。`modelsSnapshot` は `models.downloads` を
  `state.Models` だけから導くので、記録を消すと**走っている唯一の観測点が消える**。
  ジョブは走り続け、完了時に `ready` を書き戻す。実機のタイムラインは
  「t+0 pull → t+5s rm が deleted と応答 → t+20m ready で復活、6.59 GB がエンジンに存在」。
  5 秒で 6.59 GB は落ちないので、戻ってきたのは**元の pull の完了**である。

なお issue 本文の「後の再スキャンがブロブから再導出した」という原因説明は誤り。
`ModelStateReady` を書くのは `cmd/waired-agent/inference.go`（ollama）と
`inference_vllm_linux.go`（HF）の 2 か所だけで、ディスクや `/api/tags` から記録を
再導出する経路はリポジトリに存在しない。ブロブが残っているぶん再 pull が数秒の
no-op になり、mtime が変わらないという観測と整合する。

## Decision

**オペレータ起点のキャンセルを一次操作として持つ。** `waired models cancel <model_id>`
と `DELETE /waired/v1/models/{id}/pull`。`models rm` は削除の前にこれを実行する。

1202 が挙げた 5 つの反論は、いずれも設計で潰す（回避ではなく明示的な回答）。

| 1202 の反論 | 本件での回答 |
|---|---|
| `pullJob` に provenance が無い | `pullStop.requested` に**意図を記録する**。推測しない |
| `errors.Is(context.Canceled)` は使えない（`DefaultRunner.Run` は `cmd.Wait()` を返すので殺された子は `*exec.ExitError`「signal: killed」= OOM kill と区別不能） | **子のエラーを読まない**。上のフラグだけを見る。この反論はそのまま正しいので、依存しない設計にした |
| ディスパッチ直後のキャンセルが nil `CancelFunc` を踏む | `context.WithCancel` を **`beginPull` で公開する前**に作る。窓を守るのではなく無くす |
| `blockingRunner` で素朴なテストが空回りする | 既存フェイクは ctx を観測して記録する。加えて `pull` 以外（`ollama rm`）では**ブロックしない**ようにした。ブロックしたままだと「pull 中に rm」がデッドロックし、書けない失敗になる |
| 新しい終端状態が要る | **新設しない**。キャンセルは記録の削除で表す。`ModelState` は proto / web-admin / NAVI に波及する契約で、この用途に新値は要らない |

**upstream の切断挙動には依存しない。** 1202 の一次的な理由（ピン版 ollama が
クライアント切断で転送を止めるのは内部実装であって契約ではない）は今も正しい。
なので観測される結果を upstream 非依存にした: キャンセルされたジョブは
**`ready` を書かない**し `failed` も書かない。サーバ側が仮に転送を続けても、
カタログは「そのモデルは無い」に落ち着く。

**「そもそも始める理由が無い」はブート経路の議論**であり、`models pull` には当たらない。
`models pull` は人が明示的に始めたもので、始める理由はその人が持っている。

## 併せて直したもの

`DeleteModel` が残していた 2 つの標準指示。どちらも「削除が効かない」の実体だった。

- **`state.Active`**: 削除したモデルを指したまま残ると `activeEngineTag` が `""` を返し、
  `narrowPublishedModels` が「強制すべきものが無い」と読んでプローブ結果を素通しし、
  5 秒後のティックで `/api/tags` の全タグ（host-speed プローブ用モデル含む）を
  advertise する。waired-agent#656 と同じ症状に、#670 が塞いでいない別の扉から到達する。
- **preferred-model**: `bootstrapPreferredModel` は不在の preferred を再 pull する。
  削除直後のモデルはまさに不在なので、次回ブートで戻ってくる。#641 の
  「デーモン再起動を何度か挟んだ後」に一致する。

## Consequences

- **途中まで取得したバイトは回収しない。** ollama の `<blob>-partial` は残り、
  マニフェスト未書き込みのタグを `ollama rm` は名前で指せない。同じモデルを再度
  pull すれば途中から再開する。docs に明記し、回収は別 issue に切る。
- **キャンセルはレースに負けることがある。** 重みが着地した後に届いたキャンセルは
  記録を消さない。消せば「名前の無いバイト」が残り、それは #671 が削除経路で直した
  当の欠陥になる。
- **preferred を消すと bundled fallback が再武装する。** 削除したモデルとは別のモデルで、
  一度も選んでいないホストの既定挙動と同じだが、ダウンロードではあるのでログに残す。
- **`CancelPull` は同期**（ジョブの巻き取りを待つ、上限 5 秒）。直後の `models ls` が
  真実を返すため。CLI の DELETE タイムアウト 10 秒の内側に収めてある。

## Refs
- https://github.com/waired-ai/waired-agent/issues/633
- https://github.com/waired-ai/waired-agent/issues/641
- https://github.com/waired-ai/waired-agent/issues/656
- https://github.com/waired-ai/waired-agent/issues/379
- docs/decisions/20260805/1202-hold-the-bundled-prepull-instead-of-cancelling.md
- docs/knowledges/20260805/1202-ollama-pull-client-disconnect.md
