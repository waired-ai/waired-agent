---
status: accepted
---

# 放置されたモデル質問は自動ダウンロードに進まず恒久停止する (20260809 13:57)

## Status
Accepted

## Context

#379 の boot pre-pull hold は「ウィザードが操作中のままモデル未指名で
60 分(`prePullHoldMax`)経過 → fallback の自動ダウンロードに進む」という
挙動だった。当時はモデルなしホストが壊れた状態だったため、「放置されても
最終的に使える状態へ収束させる」が正当だった。

#586 で (1) 端末 init にモデルピッカーが付き、同じ 60 分の質問中クレーム
(`POST /inference/model-choice-pending`、期限はサーバー側で刻印)が増え、
(2) 「エンジンのみ・モデルなし」が正常状態になった(「0) Don't download a
model now」は exit 0 の完了)。この前提変化を受けてオーナーに確認したところ、
当初は「60 分で自動 DL に進む」を承認(2026-08-09 午前)、その後同日に
「CLI/ブラウザどちらも放置された場合は自動進行せずタイムアウトで中断」
「中断は永続化し、答えが来るまで恒久停止」へ裁定を更新した(いずれも
waired-agent#586 のコメントに記録)。

論点は再起動の穴: 質問中クレームも wizard-driving もプロセス内の状態
なので、「そのブートで中断」だけでは daemon 再起動時に never-asked アーム
(spec §11.1 の無人ホスト自動 pull)に合流し、翌ブートで数十 GB の
ダウンロードが勝手に始まる。

## Decision

- 「質問したが未回答」を `Preference.Unanswered`(preferred-model.json)と
  して**永続化**する。書くのは 2 つの期限切れアームのみ:
  ブラウザ(`awaitPrePullRelease` の ceiling)と端末クレーム
  (`awaitModelChoice` の期限切れ)。既に何らかの回答(モデル/None)が
  あれば書かない。
- このレコード(または in-process フラグ)がある限り、bundled fallback
  pre-pull は**全ブートで stand down** する。解除は実際の回答のみ:
  モデル選択(`SwapPreferredModel` / setup reconciler / ピッカー)または
  None 選択。ファイルは回答で上書きされる。
- **質問していないホストは従来どおり**: `--non-interactive`・fleet
  (auth key)・CP 無応答・通常再起動は spec §11.1 の自動 pull を維持。
  ピッカーの明示スキップ(ブラウザ takeover・catalog 不達・ステップ6で
  オフ)はクレーム撤回=「質問は来ない」であり、これも従来どおり進む。
- `TestPrePullHold_DrivingForeverGivesUpAtTheCeiling`(#379 挙動の記録、
  テスト自身が「contract ではない」と明記していた)は本裁定で反転し、
  `TestPrePullHold_DrivingForeverStandsDownAtTheCeiling` になった。

## Consequences

- 放置されたインストールは「エンジンのみ・モデルなし」で止まり、勝手な
  大容量ダウンロードは一切起きない。モデルは navi / `waired models pull` /
  再 init(#599)で選ばれた時点で来る。
- status は未回答ホストで従来どおり `awaiting_model` を報告する
  (`no_model_selected` は明示的な None 選択のみ)。未回答状態の可視化が
  必要になれば別途フィールドを足す。
- 永続化の書き込みに失敗した場合の stand down はそのプロセス限り
  (次の再起動で自動 pull に戻る)。Warn ログでその旨を明示する。

## Refs

- https://github.com/waired-ai/waired-agent/issues/586 (裁定コメント)
- https://github.com/waired-ai/waired-agent/issues/599 (再 init 完全再実行)
- https://github.com/waired-ai/waired-agent/pull/600
- waired-ai/waired#1067 (2026-08-08 オーナー裁定群)
