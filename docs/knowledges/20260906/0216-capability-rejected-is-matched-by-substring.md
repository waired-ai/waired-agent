# `capability_rejected:` は部分一致で照合される (20260906 02:16)

## Issue

waired-agent#1187 で、窓超過の 400 を Anthropic の文言の写しから
`capability_rejected: prompt_too_long` に変える。公式 docs
(https://code.claude.com/docs/en/llm-gateway-protocol#automatic-retry-and-error-forwarding)
は「gateway が自分の envelope で包むと回復経路が壊れる。ただし message が安定した
`capability_rejected:` トークンを運ぶ場合は別で、例は `capability_rejected: prompt_too_long`」
とだけ書いていて、**トークンの前後に何を置いてよいか**は書いていない。
`docs/decisions/20260903/0333` の裁定 8 が求めるのは「トークンを運び、Anthropic の文言は
写さない」ところまでで、数値をどう残すかは開いていた。

## Learnings

Claude Code **2.1.261** のバンドルから照合器を読み、スタブ(`ANTHROPIC_BASE_URL` を
向けた小さな HTTP サーバ)に 400 を返させて 5 変種を実測した。

### 照合器

```js
var own = "capability_rejected: ";
function V_(e, t) {                       // e = error.message, t = クラス名
  let r = own + t, o = 0;
  for (;;) {
    let d = e.indexOf(r, o);
    if (d === -1) return false;
    let f = e[d + r.length];              // トークンの直後 1 文字
    if (f === void 0 || !/[A-Za-z0-9_:.-]/.test(f)) return true;
    o = d + 1;
  }
}
function gq(e) { return EPe(e.message) || V_(e.message, "prompt_too_long") }
```

`EPe` は旧来の文言判定(`"prompt is too long"` / `"input is too long for requested model"`
を小文字化して部分一致)。**両者は OR** なので、移行中はどちらでも回復する。

### 実測(2 往復の会話で 400 を返し、ユーザーに見えた文言)

| `error.message` | 画面 |
|---|---|
| `capability_rejected: prompt_too_long` | `Prompt is too long` |
| `capability_rejected: prompt_too_long (214000 tokens > 200704 maximum)` | `Prompt is too long` |
| `capability_rejected: prompt_too_long: 214000 tokens > 200704 maximum` | **`API Error: 400 …`(生の文言)** |
| `prompt is too long: 214000 tokens > 200704 maximum` | `Prompt is too long`。**数値も描画される** |
| 無関係な文言 | `API Error: 400 …` |

読み取れること:

- **トークンの後ろに人間向けの語を続けてよい。ただし区切りが `[A-Za-z0-9_:.-]` だと
  一致しない。**「数値を足す」ときに最も自然な `:` が、まさにその集合に入っている。
  空白や `(` なら通る。壊れても**静かに**壊れる —— ステータスも envelope も正しいまま、
  回復だけが消える。
- **数値を message に残す意味はない。**Claude Code が数値を描画するのは旧文言の正規表現
  `/prompt is too long[^0-9]*(\d+)\s*tokens?\s*>\s*(\d+)/i` に当たったときだけで、
  トークン経路では丸括弧で足しても画面には出ない(`errorDetails` に残るだけ)。
- 1 往復だけの会話では `Prompt is too long · this conversation is a single exchange and
  cannot be compacted — the request size comes mostly from system prompt, tool
  definitions, or attachments.` になる。旧文言のときだけ
  `the request is ~214000 tokens (limit 200704)` と数値入りになる。
- バイナリに同梱されている gateway 向けの契約文は
  「The token is the whole `error.message` — nothing else in it」と書いている。
  照合実装はそれより緩いが、**契約文の側に合わせるのが安全**。

## Applied

- `internal/gateway/anthropic.go` の `contextOverflowToken` を `error.message` の**全体**にした。
  数値は `X-Waired-Prompt-Tokens` / `X-Waired-Context-Window` に移した ——
  ヘッダなら区切り文字で壊れようがなく、ピア中継も「解析」ではなく「複写」になる
  (以前はピアの散文に `HasPrefix` で依存していた)。
- `TestAnthropicMessages_OverflowMessageCarriesTheDocumentedToken` に照合器を写し、
  **`:` 区切りの変種が一致しないこと**を対で撃つ。片側だけだと、将来「数値を足す」編集が
  一番自然な区切りで回復を殺しても全緑のまま通る。
- `scripts/ci/claude-code-canary.sh` は `capability_rejected: ` と `prompt_too_long` を
  追加で監視する。旧文言 `prompt is too long` の監視も残す —— 同じ分類器のもう一方の腕なので、
  消えたら分類器ごと作り替えられたということで、トークンの側を測り直す合図になる。
- OpenAI 面(`internal/gateway/openai.go`)の文言は**変えない**。あの面は OpenAI 形の
  クライアントに直接答えるところで、`docs-site/guides/chat-clients` が本文を逐語引用している。

## Refs

- 実測の証跡: 本セッションのスタブと 5 変種(`m1-5` / `m1-5b`)
- https://code.claude.com/docs/en/llm-gateway-protocol#automatic-retry-and-error-forwarding
- `docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md` 裁定 8
- `docs/knowledges/20260904/0210-claude-code-status-codes-for-gateway-errors.md`(同じスタブ手口の先行測定)
- `docs/knowledges/20260714/0241-claude-code-context-window-internals.md`(旧文言の正規表現)
