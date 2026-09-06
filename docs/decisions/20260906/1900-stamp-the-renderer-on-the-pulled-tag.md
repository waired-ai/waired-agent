---
status: accepted
---

# 引いたタグにレンダラを刻む — コミュニティ量子化を出荷可能にする (20260906 19:00)

## Status

Accepted。waired-agent#1192(Qwen3.8-Flash-Next のカタログ追加、
waired-ai/waired#1312 の L100)。オーナー裁定 2026-09-06。

`catalog.Variant` に `renderer` / `parser` を足し(proto、単独 PR)、
`ollama pull` の直後に `ollama create` で**同名のローカル manifest を
書き換える**。上流(タグの公開者や ollama 本体)には何も依頼しない。

## Context

Qwen3.8-Flash-Next は agent-harness の grade を通ったが、request-shape
マトリクス 6 形のうち 3 形を HTTP 500 で拒否した — `trailing-system` /
`system-after-tool-roundtrip` / `developer-turn`、いずれも Claude Code が
実際に送る形。

原因を上流のソースまで辿ると、待つ先が存在しないことが分かった。

- ollama サーバは `Config.Renderer != ""` だけで分岐し、空なら GGUF 内蔵の
  Jinja に落ちる(`server/prompt.go`)。`model_family` から renderer を
  推論する経路は無い。Qwen の Jinja は非先頭の指示ターンで
  `raise_exception('System message must be at the beginning.')` を投げる。
- **`qwen4exp` 用の renderer は要らない。** ollama 自身の library タグ
  `qwen3.8-flash-next:125b-mlx` が config に `renderer: qwen3.8` /
  `parser: qwen3.5` と書いている。必要なレンダラは pin 中の 0.33.3 に既にある。
  その `normalizeQwen38Messages`(`model/renderers/qwen35.go`)は system と
  developer を配列のどこからでも先頭 1 本に畳む — waired のゲートウェイが
  `normalizeInstructionTurns` でやっているのと同じ操作で、拒否しうる分岐が
  関数内に無い。
- v0.34.0-rc1 と v0.33.3 の renderer 登録名は同一。**エンジンを上げても
  何も変わらない。**

詰まりの正体はタグ 1 本の梱包漏れだった。そしてそれは 1 人の不注意ではない:
ollama.com でこのモデルを持つ 6 namespace の **GGUF タグ 24 本すべて**の
config を読んで、`renderer` を持つものは 1 本も無かった。持っているのは
safetensors/MLX タグだけで、ollama の変換経路が自動で刻むからである。
GGUF の `create` 経路は刻まず、公開者の誰も手で書かない。**次に大型の
コミュニティ量子化を採るときも同じ穴に落ちる。**

## Decision

刻印を waired 側の導入経路に置く。

`ollama create <同じタグ名> -f Modelfile` は既存レイヤをすべて再利用し、
書き換わるのは小さな config オブジェクトだけ。sv-evox2 での実測でディスク
増加は **0.00 GB**、projector と license レイヤ(frob のタグだけが持つ
Qwen Community License 1.0 全文)も残る。同名なので下流の識別子は 1 つも
動かない。

拒否されていた 3 形は、同一ホスト・同一 blob・config 2 行だけの差で
**500 から 200 になった**。agent-harness の grade は pass のまま。

これにより `docs/decisions/20260828/1930-arm-the-request-shape-gate.md` を
**見直す必要が無くなった**。項目 2「コーディングエージェントが送る形を
render できないモデルは offer しない」はそのまま正しく、このモデルは
render できる。ゲートは免除ではなく通過で解ける。

## 採らなかった案

- **公開者に依頼する**(config に 2 行足してもらう)。1 行の依頼で済み、
  通れば刻印機構は要らない。採らなかったのは応答時期が読めないことと、
  上に書いた**構造的な穴が残る**ため — 次のモデルでまた同じ交渉になる。
- **自分の namespace に再公開する。** 78.87 GB のアップロードに加え、
  再配布可否の判断が別途要る。
- **template レイヤを持つ別タグ(`waowao/…:q4`)に乗り換える。** Go template
  は例外を投げないので 6 形とも通るが、111.33 GB は 128 GB ホストの 96 GB
  予算に入らず、「できるだけ軽い量子化を」という裁定に反する。

## Consequences

- `renderer` / `parser` は **`VariantSHA` に入らない。** あの payload は
  frozen で、広げるとフリートと制御プレーンの永続化済み計測が一斉に
  一致しなくなる。結果として刻んだ変種と刻んでいない変種が同じ SHA になる。
  塞ぎ方は SHA ではなく計測側で、`VariantRequestShapes` に `renderer` を
  記録して manifest と突き合わせる(`engine_version` と同じ理由・同じ場所)。
  ただし import 時に manifest の値を写しているので、これが捕まえるのは
  「あとから manifest が変わった」場合だけ。「刻まずに計測したものを
  刻んだ manifest に対して import した」を捕まえるには probe が実際に
  使われた renderer を報告する必要がある — 未実装。
- 刻印に失敗した pull は**失敗として扱う**。カタログが renderer を要ると
  言っているタグを刻まずに serve すると、accepted と記録した形で 500 を
  返すことになる。
- **刻印は `Pull` の内側に畳む。** 既に在るタグへの `ollama pull` は
  重みを動かさずに 2 秒でローカル manifest を公開時の config に書き戻し、
  刻印を消す(実機で確認)。安いので頻繁に起こり得るうえ、消えた状態は
  「3 形で 500」である。呼び出し側が刻む形にすると、pull する経路が
  増えるたびに忘れられる余地ができるので、分離できないようにした。
- カタログは Apache-2.0 / MIT 以外のライセンスを 1 件持つことになる。
  catalog-radar の発見フィルタは Apache-2.0 / MIT のままで正しく、
  この 1 件を前例として読まないよう prompt と PR チェックリストに明記した。
