---
status: accepted
---

# サインアウトはローカル推論の口を閉じる (20260912 12:14)

## Status

Accepted

## Context

waired-agent#1274 は「サインアウトでローカル推論エンジンを止めて、メモリを解放
すべきか」を問うたまま open だった。issue 自身が答えの前に 2 つの確認を求めて
いる — 今日どうなっているかを型ではなく実機で測ること、止めるのが正しいか。

0.0.3-rc6 の実機検証(record waired-ai/waired#1353、macOS M4)がその実測を出した。
`sudo waired logout` の直後、`ollama serve` は**消える**。セッションの context が
畳まれると `cmd/waired-agent/inference.go` の後始末が `ollama.Stop` / `vllm.Stop`
を呼び、`teardown` がその完了を待つ。つまり**エンジンは既に止まっている**。

ところが同じホストで、その 26 秒後に :9472 へ `{"model":"waired"}` を投げると
**200 が返り、エンジンが起き直して答えた**(`X-Waired-Local-Model: qwen3.5-4b`)。
同時に `sudo waired doctor` は「inference engine — not ready — local inference is
offline」と言い、:9473 は connection refused だった。3 つの面が 3 つの違うことを
言っていた(waired-agent#1310)。

機構は listener の寿命にある。:9472 は**ブート時に立つ** — 登録前にも答える必要が
あるため — のに対し、ローカル推論のハンドラは**活性化時に一度だけ** publish される
(`cmd/waired-agent/main.go` の `proxyH.SetLocalInference`)。外す口が無かった:
`SetLocalInference(nil)` は早期 return し、他に `atomic.Pointer` を書くものは無い。
畳まれたセッションのハンドラがそのまま残り、ゲートウェイのリクエストごとの遅延起動
(`internal/gateway/anthropic.go` の `EnsureRunning`)がエンジンを起こし直していた。

つまり #1274 の問いは半分ずれていた。止めるかどうかではなく、**止めたものを
起こし直せる口が開いたままだった**。

## Decision

**サインアウトはローカル推論の口を閉じる。**(オーナー裁定、2026-09-12)

1. `deactivateSession` は、セッションを畳む**前**に `proxyH.ClearLocalInference` で
   ハンドラを外す。エンジンの停止は従来どおり teardown が行う(実測済み) —
   足すのは「起こし直せなくすること」だけ。
2. サインアウト後に Waired のモデル id が来たら、既存の `waired_cannot_serve` の
   形で**サインアウトを理由として**断り、`waired init` を名指しする。状態は 400 の
   まま — Claude Code は 5xx を 10 回まで黙って再試行するので、ここでの 503 は
   1 分間の匿名の「API error」になる(`docs/decisions/20260903/0333-...` 決定 4)。
3. `/v1/models` は Waired の id を出さない。素通しにして、実行先の無い行を
   `/model` に並べない。
4. `waired logout` は自分が書いた `/model` の行を外す。行はユーザー自身の
   `~/.claude/settings.json` にあるので昇格は要らない。SessionStart フックは
   起動のたびにデーモンへ登録状態を尋ね、サインアウト済みなら書かずに消す。
5. **Claude 連携(managed-settings)は残す。** 変更には昇格が要り、サインアウトは
   昇格を求めない設計だから(`docs/decisions/20260907/0230-sign-out-is-the-daemons-job.md`)。
   Anthropic のモデル id は :9472 を素通りして本物の API に行くので、残っていても
   通常の Claude Code 利用は壊れない。`waired logout` はこの切り分けを出力で言う。

## Consequences

- waired-agent#1274 は「今日どうなっているか」の実測とともに閉じる。答えは
  「既に止まっている。足りなかったのは再起動を止めることだった」。
- サインアウト後の 3 つの面が同じことを言う: :9472 は理由付きで断り、:9473 は
  落ちており、doctor は「このパソコンはサインアウトしている」と言う。doctor の
  「turns go to another of your computers」はメッシュを離れたホストには出なくなった。
- 登録前のホストの :9472 も変わる。以前は 502 の平文「local handler not ready」で、
  Claude Code が 10 回再試行する形だった。いまは登録前も 400 + 理由。
- デーモンが答えないときはフック側で何もしない。ブート直後や、デーモンを動かして
  いない per-user インストールを「サインアウト」と読むと、サインイン済みの行を
  競合で消すことになる。
- 範囲外: サインアウトしたあと `waired init` で**同じデバイス**に戻ること。
  machine 鍵を消す以上、次の登録は別の行になり、残った行が名前を占有する
  (`<hostname>-1`)。挙動は現状維持というオーナー裁定で、設計は
  waired-agent#1323。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1274
- https://github.com/waired-ai/waired-agent/issues/1310
- https://github.com/waired-ai/waired-agent/issues/1272
- https://github.com/waired-ai/waired-agent/issues/1323
- docs/decisions/20260907/0230-sign-out-is-the-daemons-job.md
- docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
