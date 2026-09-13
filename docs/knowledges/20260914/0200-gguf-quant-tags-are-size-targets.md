# 量子化タグは精度ではなくサイズの目標 (20260914 02:00)

## Issue

カタログは同一モデルの量子化 variant を `quantization`（`Q4_K_M` / `UD-Q3_K_XL` /
`UD-Q2_K_XL` …）と `quantization_tier`（1〜8 の整数）で表している。後者は
`proto/catalog/manifest.go` の Phase 7 スコアで `ParamCount × QuantizationTier`
の積としてピアの順序付けに直接効く。

この整数が GGUF の中身を表しているのかを確かめるため、カタログに在る dense 27B の
ラダー 4 本のテンソル型を読んだ。**表している、とは言えなかった。**

## Learnings

### GGUF のテンソル表はダウンロードせずに読める

GGUF はヘッダ → メタデータ KV → テンソル情報ブロックの順でファイル先頭に並ぶ。
`Range` リクエストで先頭数 MB を取れば全テンソルの `ggml_type` を分類できる。
79 GB のモデルでも 17 MB で足りた。レジストリの blob URL に直接当てられるので、
pull しなくても建ての構成が分かる。

### 「Unsloth Dynamic」のタグ名はサイズの目標であって、精度の水準ではない

dense 27B の 4 本（いずれも 866 テンソル・同一アーキテクチャ）:

| 建て | bits/param | FFN の最低 | attention の最低 | 4bit 未満のテンソル |
|---|---|---|---|---|
| 公式 `Q4_K_M`（MTP ビルド） | 4.98 | `Q4_K` | `Q4_K` | **0 本** |
| `UD-Q4_K_M` | 4.88 | `Q3_K` ×7 | `Q4_K` | 11 本（すべて 3bit） |
| `UD-Q3_K_XL` | 3.90 | `IQ2_S` / `IQ2_XS` / `IQ2_XXS` ×20 | `Q2_K` ×3 | うち **2bit が 23 本** |
| `UD-Q2_K_XL` | 2.91 | `IQ1_S` ×20 | `Q2_K` ×7 | うち **1bit が 20 本** |

`UD-Q3_K_XL` は「3bit の建て」ではなく、**1bit から 8bit までを層ごとに混ぜて
3.90 bits/param に収めた建て**で、FFN テンソルの約 1 割が既に 2bit に落ちている。
`UD-Q2_K_XL` も同様に 1bit を 20 本含む。

したがって:

- **`quantization_tier` の整数は、サイズの段としては正しいが、精度の測度としては
  正しくない。** 混在する建てに 1 個の整数を貼っている。
- **同じ「3bit」というラベルで、別の発行元の建てと横に並べることはできない。**
- 公開ベンチの「Q3 で N ポイント落ちた」を自分たちの `UD-Q3_K_XL` に当てはめる
  ことも、厳密にはできない。

公式の `Q4_K_M` だけが 4bit 未満のテンソルを 1 本も持たない（`Q4_K` と `Q6_K` のみ、
i-quant ゼロ）。`docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md` の
「原則は公式の Q4_K_M」は、出所だけでなく中身の点でも裏付けられる。

ただし 4bit どうしの比較は一方的ではない。**公式の建ては一様に `Q4_K` で 4bit 未満が
無い**代わりに attention も `Q4_K` 止まりで、**`UD-Q4_K_M` は attention を `Q5_K`〜`Q8_0`
で厚く守る**代わりに FFN に `Q3_K` を 7 本含む。**どちらを既定にするかは、どこを守る
建てを選ぶかという判断**であって、片方が一様に優れているわけではない。

### MoE の建ては router と attention を守っているが、dense の建ては守らない

同じ `UD-Q2_K_XL` というタグでも:

- **MoE**（`qwen35moe` / `qwen4exp`）: router（`ffn_gate_inp`）は **F32**、attention は
  `Q5_K`〜`Q8_0`。量子化されるのは expert だけ。
- **dense**（`qwen35`）: attention も `Q2_K` に落ち、FFN の一部は `IQ1_S`。

これは文献の食い違いを解く。MoQE（arXiv 2310.02410）は「expert 層は量子化に強い」と
言い、route flip の研究（arXiv 2608.11212）は「ルーティングは脆く、劣化の約 1/3 は
route 由来」と言う。**出荷されている MoE の建ては MoQE 側の構成をそのまま採って
いる**ので、後者の警告は当たらない。dense の 2bit 建てにはその保護が無い。

帰結として、**「MoE と dense の 2bit を同じ扱いで並べる」ことは成立しない。**
推奨条件を完全常駐にしたときに 12 GB 帯が dense 27B の `UD-Q2` から 9B の `Q4` へ
降りるのは、この観点からも妥当。

### KV 量子化が効くかどうかは、同じメタデータから分かる

`Range` で読めるメタデータには `attention.head_count_kv` / `key_length` /
`value_length` / `sliding_window` も入っている。ここから KV 量子化の効き目が読める:

- **head 次元 256 の系列**（`qwen35` / `qwen35moe` / `qwen4exp`）: KV 量子化がそのまま
  効く。`qwen35` では 200k の KV が半分になり、CPU へ落ちる層が 13 → 3 に減った。
- **`head_count_kv` が層ごとの配列で 0 を含むハイブリッド**: 0 の層は attention の KV を
  持たない（状態が再帰側にある）ので、`kv_bytes_per_token_fp16` から計算した削減量は
  **過大になる**。
- **head 次元 64 + `sliding_window` を持つ系列**: 窓の長さぶんしか KV を要さないので、
  200k を宣言しても KV 量子化で得るものはほとんど無い。**全長で値付けしていると、
  過大な数字を半分にしているだけになる。**
- **`key_length` / `value_length` を持たないハイブリッド SSM**: KV 量子化の対象がほぼ
  存在しない。

つまり「KV の既定を q4_0 にする」の適用範囲は、**「使えない」ではなく「効かない」で
分けたほうが実態に合う。**

## Refs
- `docs/decisions/20260913/2355-catalog-variant-kv-and-residency-rulings.md`
- `proto/catalog/manifest.go`（`Quantization` / `QuantizationTier` と Phase 7 スコア）
- MoQE: https://arxiv.org/abs/2310.02410
- Unsloth Dynamic GGUF: https://docs.unsloth.ai/basics/unsloth-dynamic-2.0-ggufs
