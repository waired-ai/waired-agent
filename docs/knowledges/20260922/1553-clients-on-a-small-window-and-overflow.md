# 小さいコンテキストウィンドウと超過の 400 に、コーディングクライアントがどう振る舞うか (20260922 15:53)

## Issue

カスタムモデル(waired-ai/waired#1473)は、コンテキストウィンドウが 200,704 トークン未満でもコーディング
エージェントの行から届く(#1473 裁定 10、waired-ai/waired#1481)。そのモデルを動かすコンピュータには、
ゲートウェイがモデル自身のコンテキストウィンドウでターンを抑え、超えたターンは超過の 400 で返す。
このとき各クライアントが何をするかを、実機を使わずスタブで測った(waired-ai/waired#1482)。

- Claude Code 2.1.278(`claude -p` と `--continue`)
- OpenCode 1.18.30(`npx opencode-ai@1.18.30 run`)
- OpenClaw 2026.7.1-2 は、送信を確かめられない(`openclaw agent --local` が waired プロバイダの API キーを
  求めて送らない。既知の制約)。行ごとのコンテキストウィンドウの解決までを見た。

スタブは、要求の本文が N バイトを超えたらゲートウェイと同じ 400 を返す。Anthropic 形は
`capability_rejected: prompt_too_long`、OpenAI 形は `context_length_exceeded`。捕捉は大きさと件数だけで、
本文は保存していない(`scripts/dev/measure-agent-request.py` の冒頭と同じ理由)。

## Learnings

### Claude Code は超過の 400 から自分で回復する(-p で測定)

1. **2 ターン目が超えた場合**: 400 を受けると、次の要求で会話の要約(compact)を頼み、要約で置き換えた会話で
   同じターンを送り直す。送り直しが収まれば、利用者には普通に答えが返る(終了コード 0)。
2. **要約の要求そのものも超えた場合**: 古い部分を落とした小さな要約の要求を送り直し、その要約でターンを
   送り直す。これも回復した。
3. **要約した後もターンが収まらない場合**: 送り直しが 400 になった時点で `Prompt is too long` を出して止まる
   (終了コード 1、要求は計 4 件でループしない)。
4. Claude Code の固定の重さ: `claude -p` の最初の要求は 61,136 バイト(ツール 20 個、system 約 6 KB)。
   ゲートウェイの見積もり(本文のバイト数 ÷ 4)で約 15,000 トークン。これより大幅に小さいコンテキスト
   ウィンドウのモデルは、最初のターンも受けられない。

対話の TUI では測っていない。分類器(400 の文言の照合)は
`docs/knowledges/20260906/0216-capability-rejected-is-matched-by-substring.md`。

### OpenCode は、出力の取り置きを引いた残りで要約を決める

- 上流の式(v1.18.30): `usable = context − maxOutputTokens(model)`、
  `maxOutputTokens = min(limit.output, 32000) || 32000`
  (`packages/opencode/src/session/overflow.ts`、`packages/opencode/src/provider/transform.ts`)。
  報告された使用量がこれ以上になると要約する。
- **`limit.output` が 0 のまま小さいコンテキストウィンドウを与えると、要約が終わらない。** `limit.context`
  16,384 では usable が負になり、1 ターンの `opencode run` が 200 秒で 1,834 件の要求(40 KB の本体と 3.6 KB の
  要約が交互)を出し、timeout で止めるまで続いた。40,960 でも usable は 8,960 で、OpenCode 自身の 1 要求
  (約 40 KB、ツール 10 個)に届かない。
- `limit.output` を小さいコンテキストウィンドウの行にだけ `min(8192, context / 4)` で与えると、16,384 は
  2 要求(`max_tokens` 4,096)で終わり、40,960 では 6 ターンの途中、使用量が 32,768 を超えた要求の後に
  1 回要約して続いた。ゲートウェイの 400 には届かなかった。
- **ゲートウェイの 400(`context_length_exceeded`)が OpenCode 自身のしきい値より先に来ると、ループする。**
  OpenCode は要約を作るが、送り直す要求は元と同じ(メッセージ数もバイト数も同じ)で、また 400 になる。
  画面には `Error: prompt is too long: …` が繰り返し出る。OpenCode は自分のしきい値に頼っているので、
  行のコンテキストウィンドウは実際の値でなければならない。
- ゲートウェイの数え方(`CountOpenAIPromptTokensApprox`)は内容のバイト数 ÷ 4 で、`max_tokens` を含めない。
  OpenCode のしきい値は、エンジンが報告する実際のトークン数と出力の取り置きで決まる。どちらが先に届くかは
  実際のトークン数とバイト数 ÷ 4 の比による。この計測のスタブは報告する使用量もバイト数 ÷ 4 で返したので、
  その比は確かめていない。取り置き(最大 8,192)がその差の余裕になる。

### 反映したこと(waired-ai/waired#1481)

- 名前つきの行(`waired/local`、`waired/peer-<名前>`)は、200,704 未満のカスタムモデルを動かすコンピュータの
  コンテキストウィンドウを `max_input_tokens` で示す(`internal/integration/modelrows`)。名指ししない行は
  200,704 のまま。どのコンピュータが答えるかがターンごとに決まるため。
- OpenCode のプラグインは、一覧の値が 200,704 未満のときだけそれを `limit.context` に採り、
  `limit.output` を `min(8192, context / 4)` にする(`PLUGIN_REV` 2)。
- OpenClaw のプラグインは、同じ行にそのコンテキストウィンドウを書く(`PLUGIN_REV` 4)。送信は未確認。

## Refs

- https://github.com/waired-ai/waired/issues/1482
- https://github.com/waired-ai/waired/issues/1481
- https://github.com/sst/opencode/blob/v1.18.30/packages/opencode/src/session/overflow.ts
- `docs/knowledges/20260906/0216-capability-rejected-is-matched-by-substring.md`
