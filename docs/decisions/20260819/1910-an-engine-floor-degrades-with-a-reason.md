---
status: accepted
superseded_by:
  - docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md
---

# エンジン版の床は「理由のある行」に落とす — unfloored な兄弟では退避できない (20260819 19:10)

## Status

Accepted。ただし §3(ユーザー向け文面はエンジンの内部名を出さない)のみ
`docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md` が反転した —
行ラベルは `needs Ollama 0.32.13 (this computer has 0.31.1)`。機械可読な理由と不変条件は有効。


## Context

v0.0.3-rc2 で 27B 帯を `qwen3.8-27b` に交代させた
(docs/decisions/20260816/2024-qwen3-8-takes-the-27b-band.md)。同じ変更で
ollama の pin が 0.31.1 → 0.32.13 に上がり、前任 `qwen3.6-27b` が `manual_only` になった。
インストール済みエンジンを pin に追随させる converge は次のタグ v0.0.3-rc3 でしか
出荷されなかった (docs/decisions/20260816/2243-update-converges-the-bundled-engine.md、#826)
ので、その窓の間 `qwen3.8-27b` はカタログに載ったまま全ホストで使えなかった。

オーナーの実機レビュー (waired-ai/waired#1223) がこれを踏み、waired-agent#836 が
残件として 3 つを挙げた。(1) 床付き variant には unfloored な兄弟を同梱してカタログ側で
退避せよ、(2) `Fit` が zero value で理由が機械可読でない、(3) 「pin を上げるカタログ追加」を
converge 同梱でゲートせよ。

調査で 3 つとも前提が変わった。

### 1. 床はタグ単位ではなくアーティファクト単位でありうる

ollama レジストリを直接読んだ:

```
GET /v2/library/qwen3.8/manifests/27b-q4_K_M      → config sha256:492b2922…
GET /v2/library/qwen3.8/manifests/27b-mtp-q4_K_M  → config sha256:492b2922…  (同一)
GET /v2/library/qwen3.8/blobs/sha256:492b2922…
  {"model_format":"gguf","model_family":"qwen35","model_type":"27.3B",
   "file_type":"Q4_K_M","renderer":"qwen3.8","parser":"qwen3.5",
   "requires":"0.32.12","architecture":"amd64","os":"linux"}
```

2 つのタグは **config blob が同一 digest** で、そこに `requires` が入っている。
対照に `qwen3.6` の config (`sha256:728c795c…`) には `requires` フィールドが無い。
`ollama pull` の 412 はタグではなくこの config が決めているので、**`qwen3.8-27b` に plain
`q4-gguf` を並べても床は消えない**。消えるのはカタログ上の表示だけで、実際の失敗は
「needs AI engine 0.32.13」という明示行から pull 時の生の 412 に移動する。それは
`min_engine_version` を入れた裁定 (private `waired` 2026-06-10
`docs/decisions/20260610/1810-min-engine-version.md`) がまさに消した失敗モードで、
実装すれば退行になる。

`qwen3.6-27b` で unfloored な兄弟が成立していたのは、あの世代の床が **waired 側の判断**
(mtp タグは ollama < 0.30 が拒否する) であってアーティファクトの宣言ではなかったから。
2026-06-10 の裁定の Consequences 節が「plain q4 に落ちる」と書けたのはその条件下の話で、
一般則ではなかった。

### 2. 帯は消えていない — 消えるのは 1 行と、その行が言う理由

実カタログを `FamilyBestFit` / `RecommendedFamily` に通した (24GB dGPU / 121GB RAM, ollama 0.31.1):

```
qwen3.5-27b  fits=true  tier 67
qwen3.6-27b  fits=true  tier 69
qwen3.8-27b  fits=false  Fit{Runnable:false Reason:"" QualityTier:0 ModelSize:""}
recommended = "qwen3.6-35b-a3b"    ← 0.32.13 でも同じ
```

暗転するのは `qwen3.8-27b` の 1 行だけで、推奨は 24GB/121GB・24GB/32GB・32GB/64GB の
いずれでもエンジン版に関係なく `qwen3.6-35b-a3b` (tier 73)。**推論は失われない。**
実害は行の表示と、そこから伸びる文言だった。

その文言が原因を偽っていた。`Fit` が zero value なので `DeficitLabel` だけが答えになり、
それがメモリ不足用の文に流し込まれる:

```
! Qwen3.8 27B does not fit in this computer's memory: needs ollama ≥ 0.32.13 (running 0.31.1).
  This computer has 121 GB RAM + 24 GB graphics memory; 11 GB is already in use ...
  Run `waired models ls --detail` to see what does fit.
```

121GB のマシンに「メモリに入りません」と言い、メモリ内訳まで並べ、対処として
「入るモデルを探せ」と案内する。tray の同型は waired-agent#850 が実機で見つけ、#851 が
「原因を主張せず行を反復する」形に直した。CLI 側 (`waired models pull` /
`waired init`) は本決定で直す。

### 3. pin を超える床は converge でも救えない

`min_engine_version ≤ OllamaPinnedVersion` を強制するテストは 1 本も無かった。
converge は **pin にしか追随しない**ので、pin を超える床は「どのホストにも存在しない
エンジン」を要求することになり、出荷した日からフリート全体で暗転する。rc2 は
たまたま同じコミットで pin を上げたので成立していただけだった。

## Decision

1. **カタログでは退避しない。** `qwen3.8-27b` に unfloored な兄弟を足さず、
   `qwen3.6-27b` の `manual_only` も解除しない (#836 item 1 は却下)。
   窓を閉じるのは converge (#826) と、下の不変条件テストであって、カタログの回避策ではない。
   将来 `requires` を持たないアーティファクトで床が waired 側の判断である場合は、
   従来どおり unfloored な兄弟を置いてよい — 成立条件が違うだけで禁止ではない。
2. **エンジン床は機械可読な理由を持つ。** `FamilyBestFit` はこの枝で
   `hostfit.ReasonEngineTooOld` と `NeedEngineVersion` / `HaveEngineVersion` を載せる。
   `proto/hostfit` の package doc が「engine-version floor はここに置かない」と書いていた
   のを改める: `ReasonNoVariantForEngine` が既に「機械についてではなくカタログについての
   事実」を運んでいる前例で、エンジン床は同じ系列。`NeedMB` / `HaveMB` は載せない
   (メモリの話ではない)。`QualityTier` と `ModelSize` は載せる —
   `NoVariantForEngineModel` が同じ理由で載せている。
3. **ユーザー向け文面はエンジンの内部名を出さない。** 行ラベルは
   `needs AI engine 0.32.13 (this computer has 0.31.1)`、版が読めないときは
   `(this computer's version could not be read)`。製品の他の面が一貫して
   "the AI engine" と呼んでおり、`ollama` は人が選ぶものではなく waired が入れて
   `waired update` が更新するもの。`waired runtimes ls` の `NAME` 列やワイヤの
   engine キーは逐語のまま。
4. **不変条件を 2 本置く。** (a) bundled の全 ollama variant について
   `min_engine_version ≤ OllamaPinnedVersion`、(b)
   `TestManualOnly_NoHostLosesItsPick` を engine version `{pin, ""}` の 2 値で回す。
   `""` は fail-closed の最悪ケースで、収束前の窓の効果そのもの — 「1 つ前の pin」という
   維持し続けなければならない定数を作らずに窓を試験できる。
5. **fail-closed は agent 側だけの規則。** コントロールプレーンは engine version の
   入手元が host-speed 計測しかなく、未計測デバイスでは常に不明なので、同じ規則を写すと
   全モデルが消える。CP は fail-open で「engine 更新待ち」行を出す
   (private `waired` の 2026-08-19 決定、waired-ai/waired#1225)。同じ `Reason` 語彙を
   共有し、未知バージョンの扱いだけが非対称。

## Consequences

* `qwen3.8-27b` は未収束ホストで引き続き 1 行だけ暗転する。ただし行は理由を持ち、
  対処 (`waired update`) を案内する。
* カタログに床付き variant を足す変更は、pin を同じ変更で上げないと lint が落ちる。
  上の (a) がその場で理由を言う。
* `DeficitLabel` の文言が変わった。日付つき記録
  (docs/decisions/20260816/2243-…) は旧文字列を引用しているが、記録は凍結なので触らない。
  用語裁定は docs-site/TRANSLATION.md の Terms 表に載せた。
* `proto/hostfit` は engine version を「モデル化しない」から「refusal の理由として名前を
  持つ」に変わった。値の判定は依然として `internal/router` 側にある —
  hostfit が持つのは語彙だけで、比較器 (`proto/version`) と床の解釈は呼び出し側。
* 残る穴: エンジンが導入済みでも版が読めないホストが実在する
  (実機 pc-dell-premium: `waired runtimes ls` が `ollama not_started yes spawned - -`)。
  そこでは fail-closed が効いて converge を当てても行は暗転したままになる。
  別 issue で追う — 本決定は「その状態を行が正直に言う」ところまで。

## Refs
- https://github.com/waired-ai/waired-agent/issues/836
- https://github.com/waired-ai/waired-agent/issues/850
- https://github.com/waired-ai/waired-agent/issues/826
- https://github.com/waired-ai/waired-agent/issues/823
- https://github.com/waired-ai/waired/issues/1223
- https://github.com/waired-ai/waired/issues/1225
- docs/decisions/20260816/2024-qwen3-8-takes-the-27b-band.md
- docs/decisions/20260816/2243-update-converges-the-bundled-engine.md
