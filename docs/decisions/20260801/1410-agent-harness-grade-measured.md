---
status: accepted
---

# coding-agent 適合性は計測で決める (20260801 14:10)

## Status

Accepted

## Context

rc7 レビュー (waired-ai/waired#986) で、qwen2.5-coder:14b-instruct-q4_K_M が
Claude Code 越しに tool-call JSON を平文テキストとして出力し、リクエストに
存在しないツールを捏造した。gateway の変換チェーンは正常であることが実証済みで、
原因はモデル側の tool フォーマット不追従。

問題は「なぜ事前に止まらなかったか」だった。カタログの `capabilities` は
21 マニフェスト全部が `["chat","tool_use","json_mode"]` という同一文字列で、
「チャットテンプレートが tools に対応している」しか符号化していない。それを読む
`PickInput.RequireCapability` は production の呼び出し元が 1 つも無く、実質死んで
いた。つまり「coding agent を確実に駆動できるか」という軸がどこにも存在しなかった。

判定方法として 3 案を検討した。

1. **メンテナが有名シリーズの最新モデルを自前でテストして導入する**
2. **世代 / リリース日 / native context window からヒューリスティックに導出する**
3. **実ハーネスに通して計測する**

## Decision

**3（計測）を採る。** ただし per-PR にも nightly にも載せず、**手動 dispatch**
のレーンで走らせる。

### なぜメンテナ判断ではないか

qwen2.5-coder は有名シリーズの coder tune で、`tool_use` を広告し、Ollama
テンプレートも標準の Hermes 形式だった。**事前に見える信号は全部「合格」と
言っていた**。落ちたのは実ハーネス（tool schema 約 27 個 + 大きな system prompt）
に通したときだけ。つまり争点は「人か機械か」ではなく「実ハーネスで走らせたか」で、
そこを外せば人手でも同じく漏れる。

加えてこれは (モデル × 量子化 × エンジン版 × ハーネス) の性質で、モデル単位の
事実ではない。「qwen3.6 を試したら良かった」はカタログが実際に pull する
`q4_K_M` タグには転移しないし、テンプレート更新やエンジン bump でも壊れる。

そして手で入れた値は静かに腐る。**その腐った姿が今の `capabilities` そのもの**で、
21 件同一文字列を事実として断言している。run URL と日付を持つ計測値なら、腐った
ことが見える。

### なぜヒューリスティックではないか

現カタログで過剰発火と過少発火が同時に起きることを実測で確認した。

- gpt-oss-20b: native context 131072 で ~200k floor を割るが、**計測は pass**。
  window ベースの判定なら 16GB 級の主力推奨を誤って落とす。
- qwen3.5-0.8b: native context 262144 で floor を通るが、**計測は fail**。

window サイズと tool フォーマット追従性は独立軸で、安価な代理指標は存在しない。

### なぜ手動 dispatch か

計測は実 GPU に実モデルを常駐させる必要があり、モデルあたり分単位でダウンロードに
依存する。スケジュール実行に載せると、レジストリの一時障害やコールドエンジンが
**モデルの降格として記録される**。waired-ai/waired-agent#203 が boot benchmark 側で
記録しているのと同じ誤りで、そこでは 1 度の失敗がノードを無期限に Capacity=1 へ
落としていた。

プローブ側でも到達不能なエンジンは `fail` ではなく `error` に分類するが、悪い
verdict を記録しない最も確実な方法は、無人で走らせないことである。verdict は
モデルに対する判断なので、人間が要求したときだけ出す。

### 派生する 2 つの設計判断

**fixture は採取せず自作する。** 実クライアントのリクエストには当人の識別子が
乗っており（Claude Code は `metadata.user_id` に device id と session id を入れる）、
本リポジトリは public で「real device identifiers はテスト fixture にも入れない」
規則がある。また第三者エージェントの system prompt はそのベンダーの文章であり、
public repo にテスト入力として取り込むものではない。**採取するのは形状だけ**
(`scripts/dev/measure-agent-request.py` はツール数・スキーマバイト数・ネスト深さ・
prompt サイズのみを報告する)。

**エンジンが解析できない出力はモデルの失格とする。** qwen3.5:4b は Ollama 自身が
パースできない tool-call を出し、エンジンが 500 を返した。これを `error`
（＝未測定）に分類すると、未測定モデルは退役されないため「壊れた出力を出せば
ゲートを回避できる」ことになる。仮にパースエラーがエンジン側の欠陥だったとしても、
ユーザー体験は「このモデルはこのエンジンで動かない」であり、それこそが verdict の
対象である。verdict はエンジン版を保持するので、エンジン側の修正は再計測される。

## Consequences

- `internal/agentgrade` が計測を持ち、`internal/catalog/agentgrade.json` が
  verdict を provenance 付きで保持する。書き込みは
  `catalog-tool agentgrade --import` のみ（手編集は verdict を再び意見に戻す）。
- verdict は fixture revision 単位でしか比較できない。fixture の重量が変われば
  digest が変わり、既存 verdict は coverage gap として表面化する。
- `waired models check-agent` が同じプローブをユーザーに提供する。実装 1 本・
  トリガー 2 つ。
- カタログをどう扱うか（非適合モデルの退役方針）は製品判断であり、本記録の
  対象外。内部の記録を参照。

## Refs

- waired-ai/waired-agent#322 — カタログに agent-harness の軸が無い
- waired-ai/waired-agent#203 — 計測失敗を品質判定として報告する誤り
- waired-ai/waired-agent#200 — 退役機構が存在しない
- waired-ai/waired#986 — rc7 レビュー
