---
status: accepted
---

# `/model` の Waired 行は `modelPicker` で出し、id から `claude-` を外す (20260906 03:30)

## Status

Accepted。オーナー裁定（2026-09-06、waired-ai/waired-agent#1185 / #1177）。
`docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md`
裁定 5 の実装で、書き先については
`docs/decisions/20260820/0400-picker-cache-refreshes-on-session-start.md` を
部分的に置き換える。

## Context

Waired の `/model` 行は Claude Code の private cache
`~/.claude/cache/gateway-models.json` に直接書いて出していた。discovery は
資格情報が要る（`ANTHROPIC_AUTH_TOKEN` / `apiKeyHelper` / API キー）のに waired は
設計として渡さない（#488）ので、discovery は一度も走らず、picker が読むのは
ディスク上のキャッシュだけ、というのが唯一の経路だった（#332 / #407 / #830）。
代償として、他人の private なファイル形式を毎リリース測り直す canary を抱えていた。

Claude Code v2.1.242 以降、同じことを言う文書化された手段がある: `modelPicker`
（https://code.claude.com/docs/en/settings-reference#modelpicker）。

## Decision

1. **行は user scope の `~/.claude/settings.json` の `modelPicker` で出す。**
   キャッシュの書込・読取・その canary は撤去する。managed ではなく user なのは
   2 つの理由がある: 昇格が要らないのでセッションごとにメッシュに追随できる。そして
   `modelPicker` は「最上位の 1 つのソースが lineup 全体を供給し、複数ソースは
   決して合成されない」ので、組織が managed に置いた lineup が waired の user 行を
   丸ごと上回る — 望ましい順序になる。
   同じ規則の裏返しとして、**waired 以外の lineup が user settings に在ったら
   触らない**（上書きは追加ではなく削除になる）。所有権は
   `modelsetting.go` の none / ours / foreign と同じ形で判定する。

2. **`replaceBuiltInOptions` は書かない。** 未設定なら Anthropic の組込 lineup の
   後ろに足される。true にすると組込を隠してしまい、「Anthropic のモデルを選べば
   Anthropic に行く」という #1037 の前提が消える。

3. **id から `claude-` / `anthropic-` の頭を外し、`waired` / `waired/local` /
   `waired/peer` / `waired/peer-<node>` / `waired/public` にする。**

   頭が在ったのは discovery のフィルタ（id に `claude` か `anthropic` を含む行だけ
   残す）を通すためで、`modelPicker` には何のフィルタも無いので存在理由が消えた。

   ただし頭にはもう 1 つ、意図された副作用があった。Claude Code は
   `CLAUDE_CODE_MAX_CONTEXT_TOKENS` を **`claude-` で始まらない id にだけ**適用する
   （バンドルの述語は `!id.toLowerCase().startsWith("claude-")`、2.1.261 で実測）。
   managed settings が書くその値は**この機械の窓**なので、local 行には正しく peer 行には
   誤り。だから local だけ `anthropic-` で受け取り、他は `claude-` で拒否していた。

   その代償は 2.1.26x が「カタログ外 id には仮定した窓を強制する」挙動を入れるまで
   見えていなかった: `claude-` 頭の行は黙って 200k のセッションで動き、画面に
   `"claude-waired-peer" isn't described by this version's model catalog; …` が出る。
   頭を全部外すと変数はすべての行に効き、注意書きは消える。peer 行に当たる数字は
   この機械のもので近似でしかないが、**その数字は compaction の目安でしかなく、
   長すぎるプロンプトを実際に拒むのはこのゲートウェイ自身の 400** である
   （`docs/decisions/20260714/0241-drop-static-auto-compact-window-pin.md`）。

   `behavesAs`（行ごとに既知モデルの扱いを当てるフィールド）は使わない。
   注意書きは消えるが窓もその既知モデルのものになり local の実測値が失われるうえ、
   公開 docs のどこにも無い（settings-reference / managed-settings / sub-agents /
   llm-gateway-protocol / model-config を取得して 0 件）。文書化された経路に寄せる
   というこのレーンの目的に反する。

4. **1M を宣言している行には `[1m]` の双子を出す。** 1M を宣言するノードが 1 台も
   無ければ双子は出さない（階層は**提供するノード**への約束なので、後ろに何も無い
   双子は選ぶと失敗するメニュー項目になる）。public 行に双子は無い — 他人の機械の
   窓は答えてもらうまで分からない。
   `RequiredWindowForRequest` は今までノードを名指す行に階層を認めなかったが
   （「ノードを名指すことがそのノードへの要求になってはいけない」）、双子は素の行とは
   別の行で、宣言しているホストでしか出ない。**双子を選ぶこと自体が要求**なので、
   ノードを名指す行にも認める。素の行は今までどおり 0 のまま。

5. **旧綴りは受理し続ける。** `claude-waired-auto` / `…auto[1m]` /
   `anthropic-waired-auto` / `anthropic-waired-local` / `claude-waired-peer` /
   `claude-waired-peer-<node>` / `claude-waired-public` / `claude-waired-cloud[1m]`。
   セッションと `~/.claude/settings.json` が持っているため。per-peer は同じ slug なので
   現行 id に写して解決する。

6. **`CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` は書かない。** 資格情報が無い
   ホストでは何もしていなかった。waired が書いた値だけ scrub し、運用者が自分で
   立てた値は残す。`/v1/models` の提供自体は続ける（OpenCode 等の面）。

7. **推論オフのホストでは local 行を出さない**（#1177、rc5 の実機所見）。
   `SubsystemStateDisabled` は運用者の意思がワイヤに出たもので、デーモンはエンジンの
   健康より先にこれを立てる。「エンジンが無い」と同じ扱いにする。エンジンが
   止まっている/再起動中なのは別 — それはまだこの機械のエンジンで、行が明滅する。

## Consequence — 行が反映されるのは次のセッションから

キャッシュは SessionStart フックの**後**に読まれていたので、フックが書けばその
セッションに間に合った。settings は起動時に読まれ、**その後**に監視が張られる。
2.1.261 での実測（2026-09-06）:

| 書き手 / 時刻 | 同じセッションの `/model` に出るか |
|---|---|
| SessionStart フック（同期） | いいえ |
| フック（1 s / 2 s / 3 s 遅延） | いいえ |
| フック（6 s 遅延）/ 外部プロセス（15 s） | はい |

3〜6 s の間に監視が張られる。**競合であって契約ではないので、遅延書込には依存しない。**
フックはそのまま残し、「行が変わるのは次の `claude` から」を仕様として docs に書く。
変わるのは per-peer 行と public 行・`[1m]` 双子の有無だけで、汎用の 4 行は動かない。

代わりに得たものもある: settings の変更は**セッション途中でも拾われる**（キャッシュは
プロセスごとに 1 回しか読まれなかった）。将来メッシュ変化でデーモン側から書く経路を
足すなら、そちらは即時に効く。

## Alternatives considered

- **綴りを現行のまま維持**（local だけ `anthropic-`、他は `claude-`）。窓の振る舞いは
  一切変わらないが、注意書きが 4 行で出たままになり、#1313 が挙げた「偽 id」の指摘も
  開いたまま。
- **全部 `waired/*` にして `CLAUDE_CODE_MAX_CONTEXT_TOKENS` を撤去**。全行が仮定の
  200k になり、**全行で**注意書きが出る。
- **`behavesAs`** — 上記 3 のとおり、未文書。

## Refs

- 実測: `docs/knowledges/20260906/0340-the-model-picker-measured-again.md`
- waired-ai/waired-agent#1185, #1177, #1036, #830, #407, #332
- waired-ai/waired#1313（親）
