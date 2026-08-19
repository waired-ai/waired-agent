---
status: accepted
---

# `/model` の項目はノードを名指せる — route 軸と node 軸を分ける (20260820 02:00)

## Status

Accepted。オーナー裁定（2026-08-20、waired-ai/waired#1227 レーン L64 の設計確認）。
`docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md` を**置き換えない**
（下記 Consequences で衝突しない理由を述べる）。
`docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md` の peer-only
fail-closed 裁定を、そのまま `/model` 面に適用する。

## Context

オーナーの v0.0.3-rc2 実機レビュー（waired-ai/waired#1223）の要求:

> `/model` での選択肢を整理したい。Waired Peer を新しく追加してほしい。peer での推論に
> 限定するモードとして。

これまで `/model` の Waired 項目 4 つはすべて **route**（このターンが端末を出るか）
だけを決めていた。`internal/proxy/intercept` の `directiveRoute` が返す値も
`auto` / `waired` / `anthropic` の 3 つで、**どの Waired ノードが応えるか**は
`waired worker` の永続設定が一手に決めていた
（`docs/decisions/20260709/0940-claude-per-class-auto-waired-anthropic.md`:
「どこで動かすか(route)」と「どの Waired ノードで(worker)」を分離）。

要求はこの分離をまたぐ。「peer に限定」は route ではなくノードの話でありながら、
選ぶ場所は `/model` だからである。

## Decision

1. **`/model` の項目はノードを名指してよい。ただし route 軸は増やさない。**
   `directiveRoute` の戻り値は 3 つのまま（`claude-waired-peer` は `routeWaired`）。
   「端末を出るか」と「どの Waired ノードか」は別の問いで、後者はメッシュを持つ層
   （`cmd/waired-agent`）で解く。2026-07-09 の route/worker 分離は**維持**され、
   worker 側の答えを `/model` からも一度だけ言えるようになる、という拡張。

2. **ノード指定はリクエスト単位で運ぶ。永続設定は書かない。**
   `router.Request.NodeDirective` に**クライアントの元の id** を載せる
   （`MinContextWindow` と同じ座席）。`Model` から読んではならない: Anthropic id は
   カタログに無いので最初の選択が `ErrModelNotFound` になり、`ResolveUnknownModel`
   が `Model` を既定 alias に**上書きしてから再試行する**。`Model` 由来の実装は
   1 回目だけ正しく 2 回目に消える＝実質全リクエストで壊れる。
   `claudeSelector` は reader しか持たないので、`/model` の選択が
   `waired worker` の設定を動かすことは構造上ない。

3. **`Waired peer` は fail-closed。** peer が 1 台も応えられないとき、手元で黙って
   実行せず失敗する。`docs/decisions/20260801/1840-...` §3 の peer-only と同じ意味で、
   Anthropic へ抜けることもない（`routeWaired` なので）。黙ってローカルに落ちるのは
   waired-agent#325 が取り除いた欠陥そのもの。

4. **id の接頭辞は「セッションの窓を誰が決めるか」で選ぶ。** `claude-waired-peer`。
   `claude-` は Claude Code が id 文字列だけで窓を決める（既定 200k）ことを意味し、
   非 `claude-` は `CLAUDE_CODE_MAX_CONTEXT_TOKENS`＝**この端末**の窓を継承する。
   後者は peer の窓として常に誤りなので、`anthropic-waired-local` と同じ綴りには
   しない。一方 `RequiredWindowFor` は **0**: ノードを名指した要求に窓の約束を課すと、
   まさに選ばれた機械で turn を拒否する（`anthropic-waired-local` と同じ理屈）。
   接頭辞（クライアント側の窓算出）と `RequiredWindowFor`（serving ノードへの要求）は
   別の問いであり、両者を「iff」で結ぶ不変条件は置かない。

5. **並び順は `Waired local` の直後。** picker は 10 行で `… +N models` に折り畳まれ、
   Waired 行が見えるのは実質 4 行（実機計測、waired-ai/waired#1223）。local と peer は
   どちらも「ノードを名指す」項目なので隣接させ、新機能を折り返しの上に置く。
   代わりに `Waired cloud` が折り返し直下へ下がる。

## Consequences

- **2026-08-19 の裁定と衝突しない。** あちらが否定したのは「Anthropic id を
  カタログモデルとして解決する」ことで、`ResolveUnknownModel` は既定 alias だけを
  返すようになった。`claude-waired-peer` が鍵にするのは**ノード**であり、モデルでは
  ない。ノードが決まったあとに何のモデルが答えるかは、あちらの裁定どおり
  「そのノードが載せているもの」のまま。したがって `supersedes` は張らない。
- **エンジンを持たないノードで意味を持つ。** L63 で engine-less ノードもゲートウェイを
  通るようになったので、この項目はそこで初めて「ローカル以外を選べる」を成立させる。
- **窓は妥協が残る。** peer の実際の窓（`InferenceState.ContextWindow`）は id では
  表現できない。過大なプロンプトは serving 側の窓チェック（waired-agent#436）が
  400 で止める。実フリートは全ノードが 200704 を申告しているため、Claude Code の
  200k 既定は下回る側に倒れており安全側。
- **リクエスト単位の上書きは今後も 1 か所。** `selectWithWorkerPref` が Select と
  SelectK の唯一の隘路なので、両入口が自動的に揃う。

## Refs

- waired-ai/waired#1223（オーナーのレビュー）/ waired-ai/waired#1227 レーン L64
- waired-ai/waired-agent#830
- `docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md`
- `docs/decisions/20260801/1840-tray-routing-split-and-peer-only.md`
- `docs/decisions/20260709/0940-claude-per-class-auto-waired-anthropic.md`
