# qwen3.8 を 24GB カードで動かす — renderer の system 検証と /api/ps の死角 (20260827 13:30)

## Issue

waired-agent#1035 (Claude Code の会話途中 system ターンで 500) と
waired-agent#1038 (FIT は「収まる」と言うのに実機は CUDA out of memory) の
調査で実測した事実。次に ollama serve のチューニングか Anthropic ゲートウェイ
変換を触る人向け。

計測環境 (2026-08-27、すべて同一ホスト・同一日):
sv-mag — NVIDIA RTX PRO 4000 Blackwell (VRAM 24467 MiB) / RAM 121 GB /
ollama 0.32.13 / `qwen3.8:27b-mtp-q4_K_M`。

## Learnings

### 1. 非先頭の system ターンは qwen3.8 の renderer が拒否する

生きているエンジンの `POST /v1/chat/completions` に対する形状マトリクス:

| messages の形 | 結果 |
|---|---|
| `[system, user]` | 200 |
| `[user]` | 200 |
| `[user, system]` | 500 `system message must be at the beginning` |
| `[system, system, user]` | 500 同上 |
| `[system, user, assistant(tool_calls), tool, system, user]` | 500 同上 |
| `[system, user, developer, user]` | 200 |

この検証をするのは **qwen3.8 の renderer だけ**。同じエンジン上で
`qwen3.6-35b-a3b` と `qwen3.5-9b` は全形状に 200 を返す。upstream は
ollama/ollama#17754 で報告済みで、0.32.14 が許容 (#17757)、0.32.15 が
先頭の 1 ターンにマージする (#17855)。vLLM の Qwen 用 Jinja テンプレートも
同じエラーを raise する (vllm-project/vllm#41114) ので、エンジンを替えても
逃げられない。

引き金は実在する: Claude Code 2.1.229/241 は
`anthropic-beta: mid-conversation-system-2026-04-07` の下で、トップレベルの
`system` を 3 ブロックの配列で送り、**さらに**最初の user ターンの後に
`role:"system"` の `messages[]` エントリ (約 8 KB、"Available agent types
for the Agent tool:…") を送る。この 2 つ目が上の 5 行目の形そのもの。

### 2. `/api/ps` の `size` / `size_vram` は重みだけを数えている

KV バッファと生成用 compute バッファは追加の VRAM 消費で、`/api/ps` には
**一切現れない**。同一モデル・同一量子化での 4 構成:

| 構成 | /api/ps size | size_vram | 重みの spill | ロード後の free VRAM | 約 26.7k トークンのプロンプト |
|---|---|---|---|---|---|
| `-wb2048`, ctx=200704 | 21.73 GB | 15.54 GB | 28.5 % | 491 MiB | 500 CUDA out of memory |
| 素のタグ (エンジン既定のバッチ), ctx=200704 | 20.07 GB | 15.68 GB | 21.9 % | 945 MiB | OK, prefill 744 tok/s |
| `-wb2048`, ctx=65536 | 17.95 GB | 17.95 GB | 0 % | 3141 MiB | OK, 999 tok/s |
| 素のタグ, ctx=65536 | 17.54 GB | 17.54 GB | 0 % | 3969 MiB | OK, 978 tok/s |

1 行目の構成で runner が実際に持っていたコマンドライン (`ps` から逐語):

```
llama-server --model <blob> --port 44693 --host 127.0.0.1 --no-webui --offline -c 200704 -np 1 --log-verbosity 4 --no-log-prefix --no-log-timestamps --no-jinja --chat-template chatml --mmproj <blob> --spec-type draft-mtp --spec-draft-n-max 4 --spec-draft-backend-sampling --cache-type-k q8_0 --cache-type-v q8_0 --flash-attn on -b 2048 -ub 2048 --context-shift --keep 4
```

### 3. spill の「率」は生死を分けない。free VRAM の実読値は分ける

21.9 % spill の構成は 152k トークンのプロンプトを 966 tok/s で処理した。
28.5 % spill の構成は 2,000 トークンすら処理できなかった。verify パスが
使っていた許容値はこの 2 点の**間に偶然**座っていただけで、割合での判定は
動く構成と死んだ構成を区別していない。区別するのはロード後の free VRAM
(945 MiB は生き、491 MiB は死んだ)。

### 4. OOM の閾値はコーディングプロンプトよりはるかに低く、失敗はループする

914 トークンのプロンプトは通り、約 2,000 トークンで落ちた。落ちるたびに
runner が死んでモデルが降ろされる (`/api/ps` は空、GPU は 30 MiB まで戻る)
ので、次のリクエストは約 8 秒の冷ロードを払った上で**同じ失敗**をする。
一度きりの失敗ではなく、上限のないループ。

### 5. エンジンは強制バッチを過少計上する

`num_batch=2048` を強制すると `size_vram` は約 140 MiB しか動かないのに、
compute バッファは約 1.9 GB 増える。予測ヘッドルームからの規則がここで
成立しない理由がこれで、判定はロード後の実測から下すしかない。

### 6. 修正構成での深さラダー (再検証する人向け)

素のタグ・ctx=200704 で:

| プロンプト深さ | prefill |
|---|---|
| 20.8k tok | 766 tok/s |
| 52.0k tok | 960 tok/s |
| 104.0k tok | 822 tok/s |
| 152.0k tok | 966 tok/s |

全区間で free VRAM は 911 MiB のまま一定。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1035
- https://github.com/waired-ai/waired-agent/issues/1038
- https://github.com/ollama/ollama/issues/17754
- https://github.com/ollama/ollama/pull/17757
- https://github.com/ollama/ollama/pull/17855
- https://github.com/vllm-project/vllm/issues/41114
