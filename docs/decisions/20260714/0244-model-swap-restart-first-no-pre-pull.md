---
status: accepted
superseded_by:
  - docs/decisions/20260813/2123-model-swap-applies-in-process.md
---

# モデル切替の restart-first を維持し、pre-restart の pull を廃止 (20260714 02:44)

## Status
Accepted — ただし部分的に超えられている。

「**pre-restart の pull を廃止する**」は今も有効（restart fallback も pull を
投げない）。「**restart-first を維持する**」のほうは waired#812 の in-process
swap で覆り、再起動は fallback になった:
`docs/decisions/20260813/2123-model-swap-applies-in-process.md`。

以下の本文は 20260714 時点の記録として凍結する（当時の判断の理由を残すため、
書き換えない）。

## Context
waired#774: ベンチマーク後のモデル切替受諾が fire-and-forget で、インストール終了時に
マシンが使える状態にならない。`/inference/preferred-model` ハンドラは (a) リクエスト
コンテキストで background pull を起動し、(b) pull を待たず即座にエージェント再起動を
スケジュールしていた。再起動が (a) の pull を数ミリ秒でキャンセルするため、この pull は
実質機能せず、キャンセル時に一時的な failed 状態を書き残す。

## Decision
再起動を pull 完了まで遅延させるのではなく、restart-first を維持し、ハンドラからの
pre-restart pull 呼び出しを削除する。CLI 側 (`waitForModelSwitch`) がフォアグラウンドで
`/inference/status` を model ID 指定でポーリングし、進捗表示・Enter でのバックグラウンド化を
提供する。

## Consequences
- 実際の pull は再起動後の `bootstrapPreferredModel`(#347)が一元的に実行し、
  `activatePreferredIfNeeded` が完了後にのみ Active を切り替える。ダウンロード中も
  旧モデルが配信を継続するため、restart 遅延による可用性向上はない。
- ハンドラ内に pull 完了監視という第二の適用パスを持たずに済む。
- キャンセル起因の一時的 failed 状態のレースが元から消える。CLI 側は防御として
  failed の 3 連続観測までを過渡状態として扱う。
- 再起動直後は management API が数秒落ちるため、CLI のポーリングは到達不能を
  許容して継続する。

## Refs
- https://github.com/waired-ai/waired/issues/774
- internal/management/inference_preferred_model.go
- cmd/waired/init_pull.go (waitForModelSwitch)
- cmd/waired-agent/inference.go (bootstrapPreferredModel / activatePreferredIfNeeded)
