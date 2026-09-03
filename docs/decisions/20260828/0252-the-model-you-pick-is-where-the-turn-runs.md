---
status: accepted
supersedes:
  - docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md
superseded_by:
  - docs/decisions/20260904/0146-waired-does-not-set-claude-codes-default-model.md
---

# `/model` で選んだモデルが、そのターンの実行先を決める (20260828 02:52)

## Status

Accepted。オーナー裁定 (2026-08-28、waired-ai/waired#1283 レーン L81)。
`docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md` §4 の**前提**を
部分的に置き換える — §4 の機構（`ResolveUnknownModel` は `router.DefaultModelAlias` を
返すだけ）はそのまま生きる。変わるのは、その前提である「ユーザーが `/model` で選んだのは
帯であってこのフリートのモデルではない」という読み方のほう。実 Anthropic の id が
ローカル/メッシュへ回るのは、`/model` が Waired の行を指しているときだけになった。

`docs/decisions/20260820/0200-model-picker-can-name-a-node.md` §1（route 軸は増やさない）
と §2（`/model` の選択は永続設定を書かない）は**維持**する。本決定は §2 を改めて確認した
ものでもある。

**§4（既定モデルはユーザー設定に記録する）だけが部分的に超えられた** —
`docs/decisions/20260904/0146-waired-does-not-set-claude-codes-default-model.md`。
§4 が安全だったのは Waired のターンがまだ実 Anthropic API へ運ばれ得たからで、
`20260903/0333` がその経路を撤去した後は、エンジンの無いホストで全ターンを失敗させる。
waired は既定を書かなくなった。§4 の他の判断（managed settings には書かない、操作者が
選んだ値は触らない）と、本記録の他のすべての節は有効。

## Context

0.0.3-rc4 のオーナー手動検証（waired-ai/waired#1280）で 3 つの症状が出た。機構としては
どれも一行に還元される — **directive 表に無い `claude-*` id は「指定なし」と読み替えて、
クラス方針（既定 `auto`）でローカルへ回す**。

1. `/model` で Fable 5 を選んでもローカルの 122B が答え、TUI は「Fable 5」と表示し続ける
2. 何も選んでいない mac で `claude -p hello` にローカル 9B が 269 秒。`modelUsage` は `claude-opus-5[1m]`
3. `claude-waired-cloud[1m]` を選ぶと **waired 自身の id** が表に当たらずローカルへ行き、
   それが「最後に見た実モデル」として記憶されて、以後そのホストの**全フォールバックが 404**

3 は欠陥（waired-agent#1036）。1 と 2 は `TestNonDirectiveFollowsPolicyWhenFlagOn` と
上記 §4 で固定されていた**現行契約**で、オーナーがその変更を要求した（waired-agent#1037）。

### 実測（sv-evox2 / edge 0.0.3-edge.20260827153153 / Claude Code 2.1.245、ワイヤ捕捉）

| | |
|---|---|
| 1 | **`[1m]` はワイヤで剥がれる。** `--model 'claude-waired-cloud[1m]'` → body `claude-waired-cloud` → 404 を再現 |
| 2 | 1M は `anthropic-beta: context-1m-2025-08-07` にだけ残る |
| 3 | Claude Code の既定は **`claude-opus-5`** + 1M ベータ |
| 4 | `/model <id>` はセッション内で効き、**`~/.claude/settings.json` の `model` に即時書き戻される** |
| 5 | managed settings の `model` も効くが、`Managed settings pins … that applies on restart` と表示され**毎起動で引き戻す** |
| 6 | 印字モードに auto 権限の分類器は存在しない（対話 TUI 専用） |
| 7 | 主会話以外の小リクエストが 3 種（quota `max_tokens:1` / 切替直後の `Hi` / タイトル生成）。**いずれもセッションのモデル id を運ぶ** |
| 8 | **Claude Code は自分でモデルを変える。** 安全性判定で弾かれると `Switched to Opus 4.8.` と表示し `claude-opus-4-8` で再送する（主会話と同じ形） |

L82（waired-agent#1041 / #1039）から、Claude Code 2.1.247 の対話 TUI で:

| | |
|---|---|
| 9 | auto 権限の分類器の既定 id は **`claude-sonnet-5`**（セッションのモデルには追随しない） |
| 10 | ただし**降格経路**がある。最初の分類器リクエストが 401 以外で失敗すると `externalSonnet5Probe="demoted"` がラッチし、以後は**セッションのメインモデル id** を運ぶ |
| 11 | 分類器は形で見分けられる。決め手は `tools` 不在 + `stop_sequences` あり。`session_id` は主会話と共有で使えない |

## Decision

### 1. モデル名は実行先の指定である

実 Anthropic のモデル id（`claude-` で始まり waired 族でないもの）は
`routeAnthropic` を強制する。予約 id と同じ位置に落ちるので、route 軸は 3 値のまま。

**クラス方針より優先する。両方向で。** `route=waired` でも Anthropic のモデルを名指せば
Anthropic へ行き、`route=anthropic` でも Waired の行を選べばローカルで動く。

オーナー逐語:「`waired route` や `waired` CLI による操作はグローバル的な設定、`/model` は
そのセッション内での設定でありスコープが異なる。別に上書きできたところでそれは
ユーザーの選択なのだから問題はない」「どちらもあくまでユーザーの操作であり、
**セキュリティポリシーではない**という想定」。

**帰結として `waired` ルートは保証ではなくなる。** ユーザー向け文面はすべて「名指しされ
なかったトラフィックの既定」に書き換えた（`cmd/waired/claude_route.go` のヘルプと hint、
`internal/integration/claudecode/templates/skill_route.md`、docs-site の該当節と日本語ミラー）。
`never contacts Anthropic` は製品からも docs からも消えている。

非 `claude-` の未知 id（他クライアント由来）は**従来どおり方針に従う**。Claude Code の
`/model` の選択ではないものを、拒否するだけの API へ送っても意味がない。

### 2. id の照合は裸形で行う

`[1m]` を除いた形（大小無視・位置不問）で表を引く。**広告する綴りは変えない** —
クライアントは綴りからセッションの窓を決める（`docs/knowledges/20260820/0300-...` fact 5）。

`claude-waired-auto[1m]` は裸形で `claude-waired-auto` に一致するため、**1M の要求は id から
失われる**。したがって 1M は `anthropic-beta` ヘッダからも導出する
（`gateway.RequiredWindowForRequest`）。ヘッダは要求を**広げるだけで狭めない**。ノードを
名指す id（local / peer / public）は、ヘッダがあっても 0 のまま — 機械を名指した要求に
その機械への要求を重ねない、という `20260820/0200` §4 の理由がそのまま効く。

### 3. 「経路の判定」と「waired の id か」を分ける

`directiveRoute` の bool は 3 つの呼び出し元のうち 2 つにとって誤った問いになった。
`rewritePassthroughModel` と `observeMainModel` が訊いているのは「この id をそのまま上流へ
出してよいか」であって「どの経路を強制するか」ではない。混同したままなら、実 Anthropic の
id が passthrough で別の id に書き換えられる — ユーザーが選んでいないモデルの名で答える、
という本レーンが取り除いている当の欠陥を、逆側から作ることになる。

`isWairedOwnedID` は**族で**判定する（id に `waired` を含むか、`waired/` 接頭辞か）。
waired-agent#1036 は表に無い綴りで入ってきたので、表の完全一致に頼らない。

### 4. 既定モデルはユーザー設定に記録する

id が実行先を決める以上、**セッションが始まる id が既定の実行先を決める**。Claude Code 自身の
既定は実 Anthropic のモデル（実測 3）なので、何もしなければ触っていないセッションは全部
Anthropic へ行き、手元のハードは遊ぶ。

`waired claude enable` が `~/.claude/settings.json` の `model` に `claude-waired-auto` を書く。
**Claude Code 自身が「既定に設定」で書くのと同じファイル・同じキー**なので、`/model` で
選び直せばそれが上書きされて残る。**pin ではなく既定**である。

managed settings には**書かない**。実測 5 のとおり Claude Code は毎起動で引き戻し、
そう表示もするので、別のモデルを選んだ操作者は毎セッション選択を取り消される。

所有の作法は statusLine と同じ（`internal/integration/claudecode/statusline.go`）:
無ければ書く / waired の id なら管理する / **それ以外は触らない**。操作者がモデルを選んで
いるなら、その人は実行先を決めている — そしてそれこそ本決定が意味を持たせたものである。
`waired claude enable` と `waired claude status` は、黙って従うのではなくそう告げる。

**代償**: `claude-waired-auto` は Claude Code の知らない id なので、セッションの先頭に
3 行の警告が出る（実測の逐語は `docs/knowledges` 側に置いた）。オーナーは了承済み。
回避策は 2 つとも採れない — `[1m]` を付ければ 1M を名乗ることになり不正直、
`CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1` は自動コンパクトを殺す。

### 5. 永続 route は同期しない。表示のほうを実効経路にする

`/model` の選択で `desired-claude-routing` を書き換えることは**しない**
（`20260820/0200` §2 を維持）。オーナー逐語:「`/model` が触るのはあくまでも Claude Code の
セッションにおける動作。それによって Claude Code 外とか、ほかの並列動作している
Claude Code セッションの動作が変わってはいけない」。

実測がこれを補強している。ワイヤの id は「そのリクエストが何を要求したか」であって
「ユーザーが何を選んだか」ではない — **Claude Code 自身がモデルを変える**（実測 8）し、
**降格後の分類器はセッションのモデル id を運ぶ**（実測 10）。id から永続設定を書けば、
ユーザーが何もしていないのにマシン全体の設定が飛ぶ。

代わりに、ステータス行が**そのセッションのモデル id**（Claude Code が stdin で渡す）を読み、
その id が示す経路を表示する。状態は持たない — id は描画ごとに、それが記述する当の
セッションから届く。並行するセッションは互いに影響しない。

サブエージェントの尾も同じ訂正を受ける。`/model` の選択は主会話を動かし、サブエージェントは
動かせない（managed settings が別の id に固定している）ので、選択が両者を引き離したなら
それは本物の分割であり、そう表示する。

### 6. `Waired cloud (Anthropic API)` の行は退役

決定 1 の後は実モデル名を選べば同じことができ、しかも**どのモデルが答えるか**まで言える。
ピッカーは Waired の行が実質 4 行しか見えないところで畳まれる（同 fact 6）ので、この行は
買えるものより高くついていた。

**route の受理は継続する。** 互換の約束としてではなく機構として — picker キャッシュに TTL が
無く、`ModelWairedAutoLegacy` と同じ形で、クライアントが古い id をセッション丸ごと運びうる。

### 7. 拒否された replacement は捨てる

fallback 再送で waired が差し込んだ id に上流が 404 を返したら、その記憶を破棄する。
観測された id は正直に陳腐化しうる（Anthropic が版を退役させる）し、書き換えは
**ユーザーが打っていないモデル id を waired がワイヤに乗せる唯一の場所**である。
どちらも、その失敗はこちらが回復すべきものであって、繰り返すべきものではない。

## Consequences

- **`/model` の意味が変わる。** 今まで無視されていた選択が、本当にそのモデルに届く（＝課金される）。
- **`waired` ルートは「Anthropic に一切繋がない」ではなくなった。** 文言は全面的に書き換えた。
  プライバシーの中間形（main はクラウド・subagent はローカル）は `waired claude route --sub` に
  残っており、`/model` はサブエージェントを動かさない。
- **既定モデルを持つホストでは `/waired-route` が主会話を動かさない。** セッションのモデル id が
  実行先を名指しているため。「大きい窓が欲しければ `/waired-route anthropic`」という案内は
  「`/model` でそのモデルを選ぶ」に直した（skill 本文と docs の両方、同じ PR で）。
- **auto 権限の分類器**は本決定の対象ではない。実測 10 の降格経路があるため id では捕まえ
  きれず、L82 が**形で**（`tools` 不在 + `stop_sequences` あり）どの経路でも Anthropic へ
  送る形で先に解決した（waired-agent#1041、`internal/proxy/intercept` の
  `bodyIsAutoModeClassifier`、directive 判定より**手前**）。順序はそれで正しい — 降格後の
  分類器はセッションの directive id を運ぶので、id を先に見ると許可の判定がこの端末へ
  戻ってしまう。本決定が加えるのは「ユーザーが名指ししたモデル」の側だけで、
  誰も名指ししていないリクエストの扱いは L82 のもの。
- `RequiredWindowFor` は `[1m]` を含む id への答えを保つ（別クライアント、再生した捕捉）。
  ワイヤから来る裸形には `RequiredWindowForRequest` が答える。

## Refs

- waired-ai/waired-agent#1036（欠陥）/ #1037（変更要求）/ #1067（BOM、同じ PR で修正）
- waired-ai/waired#1280（オーナー手動検証）/ waired-ai/waired#1283 レーン L81
- `docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md` §4（前提を部分的に置き換え）
- `docs/decisions/20260820/0200-model-picker-can-name-a-node.md` §1 §2 §4（維持）
- `docs/knowledges/20260820/0300-model-picker-measured-on-device.md`（fact 5 / 6 / 7）
- waired の private 側 `docs/knowledges/20260822/1507-claude-code-window-for-directive-model-ids.md`（窓の決まり方）
