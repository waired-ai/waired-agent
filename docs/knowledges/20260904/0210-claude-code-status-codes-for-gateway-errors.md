# Claude Code がゲートウェイのエラーをどう見せるか (20260904 02:10)

## Issue

waired-agent#1184 で「Waired が答えられないターンは即座に理由付きで失敗する」形を作るとき、
どの HTTP ステータスなら**こちらの本文がそのまま、リトライ無しで**ユーザーに出るのかを
決める必要があった。公式 docs
(https://code.claude.com/docs/en/errors#automatic-retries)は「5xx / 529 / 429 は最大 10 回
リトライしてから表示、400 / 401 / 403 は即表示」と書いているが、**表示のされ方**までは
書いていない。

## Learnings

ローカルにスタブ(`ANTHROPIC_BASE_URL` を向けた小さな HTTP サーバ)を立て、Anthropic 形の
error envelope でステータスだけ変えて `claude -p` を 1 回ずつ走らせた。
測定: Claude Code 2.1.259 / `CLAUDE_CODE_MAX_RETRIES=2`(リトライの有無だけ見るため短縮)。
本文は全ステータス共通で `STUBMARKER this computer cannot answer right now`。

| status | POST 回数 | 画面に出たもの |
|---|---|---|
| **400** | **1** | `API Error: 400 <本文そのまま>` |
| 401 | 3 | `Failed to authenticate. API Error: 401 <本文>` |
| 403 | 1 | `Failed to authenticate. API Error: 403 <本文>` |
| 404 | 2 | **本文は捨てられる** → `There's an issue with the selected model (<id>). It may not exist or you may not have access to it. Run --model to pick a different model.` |
| 409 | 3 | `API Error: 409 <本文>` |
| 422 | 1 | `API Error: 422 <本文>` |
| 429 | 3 | `API Error: Server is temporarily limiting requests (not your usage limit) · <本文>` |
| 503 | 3 | `API Error: 503 <本文>. This is a server-side issue, usually temporary — try again in a moment. If it persists, check your inference gateway (<host>).` |

読み取れること:

- **本文をそのまま、1 リクエストで、余計な前置き無しに出せるのは 400 だけ。** 422 も
  1 リクエストで逐語だが、docs が挙げているのは 400 の方。
- **404 はこちらの説明を捨てる。** ゲートウェイが「そのモデルを出せる機械が無い」と
  書いても、ユーザーが読むのは「モデルが存在しないかも」になる。0.0.3-rc5 の実機検証で
  `claude-waired-public` を選んだときに出た文言(waired-ai/waired#1309 の O14)は
  これで説明がつく。**404 は使わない。**
- **401 / 403 は「認証に失敗しました」を前置きされる。** 401 は docs の記述と違って
  リトライもされた(`apiKeyHelper` 経路の再試行と思われる)。
- 503 は 3 回リトライしてから、しかも「サーバ側の一時的な問題」という**こちらの意図と
  違う診断**を足して出る。「待っても直らない」種類の失敗に 503 を使うと、1 分近い匿名の
  `API error · Retrying 1/10` の後で誤った助言が出ることになる(waired-agent#1180)。

再現の手順とスタブは残していない(スクラッチ)。同じ形は 30 行程度の Python で作れる:
`POST` に対して選んだステータスと `{"type":"error","error":{"type":..,"message":..}}` を
返し、`GET` には `{"data":[]}` を返すだけでよい。

## Refs

- https://code.claude.com/docs/en/errors#automatic-retries
- https://github.com/waired-ai/waired-agent/pull/1198
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md` 決定 4
