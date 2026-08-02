---
status: accepted
---

# gateway 修正後にカタログを再測定し、#322 の退役根拠を取り下げる (20260802 15:05)

## Status
Accepted

## Context

`internal/catalog/agentgrade.json` の verdict は全て #409 より前に測ったものだった。
当時の gateway は、エンジンが取りこぼした tool call を地の文のままクライアントに
渡していた。`fixture_revision` は gateway を含まないので、ファイル上は何も
古びて見えないまま、2 世代の測定が無言で混ざっていた（#426）。

再測定の条件（#434 / #440 で入れた計器を使用）:

- 17 variant（提示対象 16 + internal の granite4-350m）
- 各モデルを **unary と stream の両方**で 12 試行ずつ、**合算 24 試行**
- engine ollama 0.31.1（pin 版）、fixture `2004661dc908`、
  agent `9200fbeb2d90`、24 GB discrete NVIDIA

測定結果（失敗の少ない順）:

| tag | verdict | 失敗合計 /72 | ケース別 | 失敗の種類 |
|---|---|---|---|---|
| `qwen2.5-coder:7b-instruct-q4_K_M` | pass | **0** | greeti 0/24 read 0/24 search 0/24 | — |
| `qwen3-coder:30b-a3b-q4_K_M` | pass | **0** | greeti 0/24 read 0/24 search 0/24 | warn_unprompted |
| `qwen3.5:27b-q4_K_M` | pass | **0** | greeti 0/24 read 0/24 search 0/24 | warn_unprompted |
| `qwen3.6:27b-mtp-q4_K_M` | pass | **0** | greeti 0/24 read 0/24 search 0/24 | — |
| `qwen3.6:27b-q4_K_M` | pass | **0** | greeti 0/24 read 0/24 search 0/24 | — |
| `qwen3.6:35b-a3b-mtp-q4_K_M` | pass | **0** | greeti 0/24 read 0/24 search 0/24 | — |
| `qwen3.6:35b-a3b-q4_K_M` | pass | **0** | greeti 0/24 read 0/24 search 0/24 | — |
| `gpt-oss:20b` | fail | **1** | greeti 1/24 read 0/24 search 0/24 | malformed |
| `qwen3.5:0.8b-q8_0` | fail | **1** | greeti 0/24 read 0/24 search 1/24 | unknown_tool |
| `qwen2.5-coder:14b-instruct-q4_K_M` | fail | **2** | greeti 0/24 read 2/24 search 0/24 | no |
| `qwen3.5:2b-q4_K_M` | fail | **5** | greeti 0/24 read 2/24 search 3/24 | malformed, no, warn_unprompted |
| `qwen3.5:35b-a3b-q4_K_M` | fail | **5** | greeti 0/24 read 2/24 search 3/24 | malformed, no |
| `qwen3.5:4b-q4_K_M` | fail | **6** | greeti 0/24 read 3/24 search 3/24 | malformed, no |
| `qwen2.5-coder:3b-instruct-q4_K_M` | fail | **7** | greeti 0/24 read 0/24 search 7/24 | unstructured |
| `granite4:350m` | fail | **8** | greeti 0/24 read 3/24 search 5/24 | no |
| `qwen3.5:9b-q4_K_M` | fail | **11** | greeti 0/24 read 0/24 search 11/24 | malformed |
| `qwen2.5-coder:0.5b-instruct-q4_K_M` | fail | **48** | greeti 0/24 read 24/24 search 24/24 | unstructured |

## Decision

### 1. n=24 を記録する。n=3 は測定になっていなかった

pass→fail に動いた行（gpt-oss:20b, qwen3.5:4b, qwen3.5:9b）は劣化ではない。
grade は worst-across-trials なので**試行数の関数**であり、n=3 の pass は
「12 回引かなかった」以上の意味を持たない。したがって判断は grade ではなく
`cases[].failed/trials` のレートで下す。

### 2. #322 の「qwen2.5-coder は tool 呼び出しに失敗するから退役」は取り下げる

修正前は 4 サイズ全てが tool ケース全滅だった。修正後:

- **qwen2.5-coder:7b**（コンパイル時 `BundledModelID`）は **24 試行で全ケース 0 失敗**。
  新規インストールが既定で使うモデルであり、退役していたら
  「parser 修正で使えるようになったモデル」を消していた。
- 14b は 2/24、3b は 7/24。残る失敗は `fail_no_tool_call`（呼ばない）で、
  #409 が対象とした「正しく選んで書き方を間違えた」とは別の欠陥。
- **0.5b だけは性質が違う**。24/24 失敗し、`fail_unstructured` の証拠として
  記録された JSON は `{"name": "systemd", …}` や
  `{"name": "internal/router/model_picker.go", …}` —— **tool 名ですらない**
  （デーモン名とファイルパス）。gateway は提示済みの名前でなければ回収しない
  ガードにより、正しく text のまま通した。0.5b は「書き方を間違えた」のではなく
  **そもそも tool を呼んでいない**。どのパーサでも救えない。

したがって退役の判断材料は #200 の quality tier 論に戻る。agentgrade は
14b の退役を妨げず、**0.5b → qwen3.5-0.8b の差し替えを強く支持し**
（0.8b は 1/24）、**3b・7b の据え置きを支持する**。

### 3. 残る主要因は上流のパーサバグであり、我々のコードではない

qwen3.5 系の失敗は全て `XML syntax error on line 8: element <function>
closed by </parameter>` —— ollama の厳格な `encoding/xml` が、モデルが
学習したとおりの Qwen3-Coder XML を拒否する（ollama/ollama#16383）。
9b は 2 段階の tool 要求を **11/24** 落とす。修正 PR（ollama#16398）は
まさにこのドリフトを許容する内容だが **未マージ**（2026-07-25 時点）。

これは #200 の「qwen3.5 の系列はまだ同メモリ級で qwen2.5-coder を
超えていない」という結論を **quality tier に加えて agent-harness の軸でも**
裏付ける。

### 4. `--require-pass` は引き続き無効のまま

n=24 では推奨級のモデルも `fail` と記録される。verdict に製品側の消費者は
なく（`catalog.AgentGrades()` を読むのは `cmd/catalog-tool` のみ）、
CI は coverage しか見ないので実害はない。有効化は grade rule を
レートベースに変える #322 側の作業。

## Consequences

- ファイル内の `fail` は増えるが、**製品挙動は何も変わらない**。
  読み手が「カタログが壊れている」と誤読しないよう、各エントリの `notes` と
  この記録で明示する。
- streaming ではエンジンの失敗が「正常終了した空のターン」になり診断が消える
  ことが測定で判明した → **#442**。Claude Code は常に streaming するので、
  実ユーザが受け取るのはこちら。
- クラス B（エンジンが 500 を返す）は #409 では救えない。上流待ち、
  patched ollama、あるいはエンジン側の tool 解析をバイパスして
  gateway の回収に一本化する、のいずれか。

Refs #426, #409, #322, #200, #442, ollama/ollama#16383
