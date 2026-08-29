---
status: accepted
---

# 番人は経路の判断を観測する — トランスポートではなく (20260829 16:55)

## Status

Accepted。PR で実装。

## Context

`TestIntegration/claude-anthropic-model-id` は `claude-fable-5[1m]` を投げて
**ローカル配信**を主張していた。#1091(`af5f9898`、2026-08-27 19:31 UTC)の
オーナー裁定で「実 Anthropic API が配信するモデル id を名指すことは、
どこで走るかを名指すこと」となり、この id は上流にピンされる。
主張は覆ったが、レグは書き換えられなかった — #1091 は 44 ファイルを触りながら
`internal/e2e/integration/legs.go` を触っていない。

macOS / Windows の nightly は 8/28 から赤い(実 API から 401、
`X-Waired-Fallback` も event ring 行も無く、ターンはローカルに一度も触れていない)。
**しかし linux の足は緑のままだった。**

理由は #665 の降格である。`routeAnthropic` は `passthroughWithLocalDegrade` を通り、
CI が `api.anthropic.com` を `0.0.0.0` にしているため dial が失敗し、
`X-Waired-Fallback: local; reason=anthropic_unreachable` を付けてローカルで配信し直す。
gateway はそれを `decision=local status=200` と記録する — 番人の合格条件そのものである。

穴は起票時の理解より広い。`classifyDrive` のブラックホール分岐は
**route=auto のフェイルオープン**(502 `waired_upstream_unreachable`)を正しく捕まえる。
捕まえられないのは **route=anthropic クラス**で、そこは #665 が
「成功と区別できない 200」に変換してしまう。したがってこれは 1 レグの問題ではなく、
**どのレグが route=anthropic に退行しても、毎 PR 回る唯一の足では緑になる**。

## Decision

**1. レグは自分の期待する結果を宣言し、主張は daemon が記録した判断に対して行う。**

`Leg.Expect` は `local` か `upstream`。`upstream` のレグは
`GET /waired/v1/integration/claude/route` の `last_request_route` が `anthropic` で
あることを主張する。この記録は `dispatchRoute` の**冒頭**で書かれるので、
#665 で降格しても `anthropic` のまま残る。**3 OS でも、ブラックホールの有無でも、
同一に成立する**主張はこれだけである。

チャネルは #1091 自身が作った。その doc コメントが理由を述べている —
「実 Anthropic API へ送られたターンは `RecordServed` に一切届かないので、
最後のターンが何を要求したかをホストに訊けることが、予期しない経路に乗った
セッションを可視にする唯一の方法」(#1036)。**番人はトランスポートから経路を
推測していたが、daemon は経路そのものを publish していた。**

**2. `local` を主張するレグは、降格ヘッダ付きの 2xx を拒否する。**

これが 1 レグでなくクラス全体に効く一般修正である。`Outcome` の零値は
`local` として扱う — 宣言を忘れたレグは「検査なし」ではなく厳しい側に倒れる。

**3. ブラックホールは主張の前提ではなくなる。**

経路の記録が主張になったので、macOS / Windows に `/etc/hosts` の細工を広げる必要はない。
`scripts/dev/macos-installtest-run.sh` は逆に `/etc/hosts` が綺麗であることを
主張しており、無用な衝突を作らない。ブラックホールは belt-and-braces に戻る。

**4. #600 のカバレッジは別レグへ移す。**

`waired/tiny` はカタログ別名なので `claude` レグは `ResolveUnknownModel` に到達しない。
到達していた唯一のレグが今回 upstream になるため、放置すると
**#600 の e2e カバレッジはゼロ**になる。新レグ `claude-unresolvable-id` は
`claudemanaged.SubagentModelID`(`waired/subagent`、#646 の実ラベル)を駆動する —
カタログ解決不能で、Anthropic 所有でないためクラス方針に従ってローカルへ行く。

このレグは class=sub なので `RecordRequest` が意図的に記録を落とす
(記録するとユーザーが選んでいない文字列でメインの記録を上書きしてしまう)。
`SubagentClass` で明示し、経路記録は読まない — 読めば**前のレグ**の値を見てしまう。
降格ヘッダの検査(決定 2)は効くので、このレグが盲目になることはない。

## Consequences

- macOS / Windows の nightly が 8/28 以来の赤から戻る。**赤かったのは正しかった**ので、
  緑に戻すのはレグの書き換えであって製品の変更ではない。
- 毎 PR 回る linux の足が、初めて route=anthropic クラスの退行を観測できるようになる。
- 実行結果の語彙に `upstream` が加わる。#1147 の
  [1500-a-harness-reports-what-it-observed](1500-a-harness-reports-what-it-observed.md) が
  「#1141 がレグ 1 本を恒常的に上流にした瞬間に生きる」と予告していたとおり。
  wrapper 3 本は `local` 決め打ちをやめ、**終端に達していないレグ**(`ran` のまま)が
  在れば落ちる形にした。
- wrapper に「最低 1 本はローカル配信されたこと」は**入れなかった**。Go 側が
  レグごとの期待を持つようになったので冗長であり、`WAIRED_INTEGRATION_LEGS` で
  絞った正当な実行を誤検知しうる。期待は Go が持ち、wrapper は観測を報告する
  (#1147 の分担のまま)。
- `internal/e2e/integration` が `internal/management` に依存する。型を借りるのは
  `intercept.HeaderFallback` と同じ理由 — 欄名の変更で黙って食い違わせないため。
  import はタグ付きの `harness.go` 側に置き、タグ無しの `budget.go` は
  stdlib + `intercept` のままに保った(毎 PR の unit レーンで回るのはそちら)。
- 経路は `tcpReadRoutes` に無く socket 専用だが、ハーネスは `models/pull` で
  既に `ipcclient` を使っており、3 OS の CI ログで読めることを確認済み。
  **allow-list は変更していない** — 新しい経路を TCP に開ける理由が無い。

## Refs

- waired-agent#1141(本件)/ #1091(裁定)/ #600(未知 id の写像)/ #646(サブエージェント標識)
- #665(上流不達時のローカル降格)/ #1118・#1147(ハーネスは観測したことを報告する)
- `docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md`
