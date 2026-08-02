# agent-harness の pass/fail は試行数の関数になっている (20260802 07:53)

## Issue

`internal/agentgrade` の verdict を、記録済みの 3 試行ではなく 12〜96 試行で
測り直したところ、**3 試行で pass だったモデルのうち 2 つが fail に転んだ**
（qwen3.5:4b, qwen3.5:9b）。さらに n を上げると、ローカルで測れたカタログ
モデルのうち pass のまま残るのは gpt-oss:20b だけになった。

きっかけは qwen3-coder:30b-a3b の 1/3 という中途半端な記録値の再測定
（waired-ai/waired-agent#322 のフォローアップ候補）。

## Learnings

### 1. grade は worst-across-trials なので、n を増やすと必ず fail に落ちる

`Probe.Run` は case ごとに**試行を通じた最悪の verdict** を採り、`Report.Grade`
は 1 ケースでも失敗すれば `fail` になる。したがって 1 回あたりの失敗率が
0 でないモデルは、試行数を増やせば確率 1 で fail に収束する。

**3 試行での「pass」は「そこまで探さなかった」以上の意味を持たない。**
二値の grade はモデルの性質ではなく、測定回数の関数になっている。

実測（ollama 0.31.1、24GB discrete NVIDIA、fixture revision 2004661dc908）:

| モデル | tool ケースの失敗率 | 3 試行での grade |
|---|---|---|
| gpt-oss:20b | 0/48 | pass |
| qwen3.5:27b | 1/48 (2%) | pass |
| qwen3.5:4b | 5/96 (5%) | pass |
| qwen3.5:9b | 17/60 (28%) | pass |
| qwen3-coder:30b-a3b | 8/24 (33%) | fail |
| qwen2.5-coder 0.5b/3b/7b/14b | 24/24 (100%) | fail |

失敗**率**で並べると綺麗に分離するが、二値の grade では 2% と 28% が同じ
「fail」になり、0% と 28% が（3 試行では）同じ「pass」になる。

### 2. 3 試行では case 単位の診断が再現しない

独立した 2 回の 3 試行スイープで、13 モデル 39 ケースのうち **11 ケースの
verdict が変化**した。しかも `fail_unstructured_tool_call → pass` のように、
分類器を厳しくしては起こり得ない向きにも動く。grade は 13/13 一致したが、
それは両方とも同じように取りこぼしていたためで、一致は正しさの証拠では
なかった。

### 3. ばらつきはエンジンの状態持ち越しではない

qwen3.5:9b で確認した:

- **A**: 1 プロセスに 12 リクエスト × 3 回 → 3/12, 2/12, 3/12
- **B**: 1 リクエストごとに ollama もモデルロードもやり直し × 12 回 → 4/12

同率。KV キャッシュやスロットの劣化ではなく、モデル素のサンプリング
ばらつき。真の失敗率が 28% なら 6 連続成功は約 14% で、3 試行 ×2 回が
たまたま全部通ることは珍しくない。

### 4. qwen3.5 の失敗は ollama を上げても直らない

失敗の実体は ollama が 500 を返すもので、中身は

```
XML syntax error on line 8: element <function> closed by </parameter>
```

モデルが `<parameter=glob>` を吐き、Go の `encoding/xml` が要素名を
`parameter=glob` と解釈するため閉じタグと食い違う。ollama 0.32.5
（waired の pin は 0.31.1）で測り直しても残る:

| ケース | 0.31.1 | 0.32.5 |
|---|---|---|
| qwen3.5:9b search-then-edit | 17/60 (28%) | 13/24 (54%) |
| qwen3.5:4b tool 2 ケース計 | 5/96 (5%) | 3/48 (6%) |
| qwen3-coder:30b tool 2 ケース計 | 8/24 (33%) | 5/24 (21%) |

エンジンのバージョン更新は対策にならない。

### 5. カタログの並び順と tool 追従性が逆転している

**qwen3.5:9b (28%) は 4b (5%) より 5 倍悪い。** 上位帯のモデルに乗り換えた
ユーザーが tool 呼び出しの信頼性を落とす。サイズや世代から追従性を推測
できないという #322 の前提が、同一シリーズの中でも成り立っている。

### 6. 退役の根拠は無傷

qwen2.5-coder 0.5b/3b/7b/14b は n=12 で全 tool 呼び出し・全試行が失敗する
（100%）。試行数を増やしても結論は変わらない。

## Refs
- https://github.com/waired-ai/waired-agent/issues/322
- `internal/agentgrade/probe.go` — `DefaultTrials`, `Probe.Run`
- `internal/catalog/agentgrade.json`
