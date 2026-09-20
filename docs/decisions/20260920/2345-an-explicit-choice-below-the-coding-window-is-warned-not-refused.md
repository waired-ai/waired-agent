---
status: accepted
supersedes:
  - docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md
---

# 200,704 を保てないビルドの明示的な選択は、断らずに警告する (20260920 23:45)

## Status

Accepted。オーナー裁定(2026-09-20、waired-ai/waired-agent#1435 の設計を確認する場)。

`docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md`(オーナー決定
2026-09-16、waired-ai/waired-agent#1396)を**部分的に置き換える**。記録の `## Status` に
鏡の一文を置いた。置き換えるのは次の 2 か所だけで、ほかはそのまま有効:

- 決定 1 の最後「明示的に選ばれたビルドを起動前に断るのは waired-ai/waired-agent#1435」。
- 「まだ入っていないもの」の 1 つ目「明示的に選んだビルドを、200,704 を保てないなら断り、
  選択画面で『実行できない』と示す」。

**私設側の記録を散文で復元する。** `waired-ai/waired` の決定
`docs/decisions/20260808/2325-capacity-warns-and-asks-not-refuses.md`(オーナー裁定
2026-08-08)は「拒否の全廃、容量不足は警告と明示確認(既定 No)」で、`0337` がそれを
コンテキストウィンドウについて部分的に覆していた。**その上書きを巻き戻す。** 2325 の
決定は、コンテキントウィンドウについても再び全面的に有効になる。リポジトリをまたぐ
置き換えは front-matter で書けないので散文で置く。

エンジンが 200,704 か 1,048,576 だけを配信・宣言することは**変わらない**(`0337` の
決定 1〜4 の本体)。変わるのは、保てないビルドを人が名指したときに**断るかどうか**だけ。

## Context

### オーナーの問い

> 気になるのは、waired ではたとえ OOM のリスクがあろうともユーザーの選択を拒否はせず、
> 尊重する、ただし警告は出すというスタイルである。

2026-08-08 の裁定(2325、原文)がその出典:

> これまで OOM Kill の可能性がある場合はハードリミットとしていましたが、
> その決定についても撤回してソフトリミットに変更します。
> (容量を)下回ったとしても推論エンジンの提供を拒否するのではなく、推奨しないだけ。

### どういうときに起きるのか

`min_vram_mb` は定義上「200,704 トークンの窓込みでそのビルドが載る最小の VRAM」なので、
カードがそれを下回ると該当する。出荷カタログの vLLM ビルドでは `qwen3.8-27b/fp8` が
52,224 MB、同 `nvfp4` が 39,936 MB、`qwen3.5-35b-a3b/gptq-int4` が 38,912 MB、
`qwen3.5-4b/bf16` でも 20,480 MB。**24 GB や 16 GB のカードの人が、自分のカードより
大きいビルドを明示的に選んだとき**で、珍しい状況ではない。フリートの検証機(24,467 MiB)
でも 27B の 2 variant が該当する。

自動選択では起きない。`proto/modelrank` の段 1 が vLLM で 200,704 を保てないビルドを候補
から外し、1 つも残らなければ `ErrHardwareInsufficient` を返す(`0337` 決定 4)。

### 今すでにどう動いているか

選択は受け入れられ、エンジンは起動し、窓を宣言せず(`WindowFits=false` → `declaredTier` が 0)、
`vllmBelowTierWarning` が `waired status` と `waired runtimes ls` に出る。つまり**現状が
すでに「拒否せず・警告して・尊重する」形**で、#1435 はそれを拒否に変える変更だった。

### 決定打になった非対称

同じ「どの Waired の行にも答えない」状態は **ollama でも起きる**。メモリが足りず段に
届かないとき、段の計画は `Fits=false` のまま最下段を配信し、窓を宣言しない
(`cmd/waired-agent/inference_ollama_tuning.go`)。そして ollama 側は拒否しない。

#1435 のとおり実装すると、**同じ結果になる 2 つの経路のうち vLLM だけが断られる**。
エンジンの選択は速さと同時実行の選択であって、製品がその人の選択を尊重するかどうかの
選択ではない。

加えて waired-ai/waired-agent#685 が書くとおり `anthropic-waired-local` は契約外の窓を
名乗れる唯一の id として意図的に残されており、200k 未満のローカル推論には用途がある
(自分の鍵盤のためだけに動かす)。断るとその用途ごと消える。

## Decision

1. **200,704 を保てないビルドの明示的な選択を断らない。** 管理 API、CLI、tray、制御プレーンの
   指示、どの経路でも受け入れる。
2. **代わりに警告して、既定 No の明示確認を取る。** 2325 の warn-and-ask にそのまま乗る。
   既存の面をそのまま使う(`cmd/waired/models_fit.go` の `warn*` 族、tray の確認、
   NAVI の `ConfirmDialog`)。
3. **ピッカーには「このコンピュータのどの Waired の行もこのモデルを使わない」と示す。**
   「実行できない」ではない — エンジンは起動するので、そう書くと嘘になる。
4. **保存済みの選択の落ち先は決めなくてよい**(#1435 scope 3)。落とさないので落ち先が無い。
5. **エンジンをまたいで同じ扱いにする。** ollama 側で段に届かないときの扱い(配信して宣言
   しない)を vLLM に合わせるのではなく、vLLM 側を ollama に合わせる。

## Consequences

- waired-ai/waired-agent#1435 の範囲が狭まる。scope 1 は取り消し、scope 2 は文言を変えて残り、
  scope 3 は消え、scope 4 はこの記録になる。実装は waired-ai/waired#1456(L122)が引き取る。
- `docs/decisions/20260828/1730-vllm-sizing-moves-into-hostfit.md` 決定 4「容量だけが断ってよい」
  は**触らずに済む**。断らないので、断ってよい主体の話が出てこない。
- `vllmBelowTierWarning` の文言はそのまま使える。すでに「so no Waired row will use this
  computer」と、断るのではなく起きることを述べている。
- **1M のグレーアウトはこの決定の対象外。** `waired-ai/waired` の決定
  `20260913/2350` 決定 9 が「エンジン別のタブで他方のエンジンの一覧をグレーアウトして
  選べなくするのは『選んだエンジンでは動かない』の表示であり、容量のソフト化の対象外」と
  切っており、「選んだ**窓**では動かない」は同じ形。2026-09-20 のオーナー裁定
  (1M を選んだとき 200k までのモデルを理由つきでグレーアウトして選べなくする、
  waired-ai/waired#1359)はそのまま有効。
- 残る非対称が 1 つある。ollama は**段に届かなくても最下段で配信する**が、vLLM は
  `--max-model-len` を推定値にする。どちらも窓を宣言しないので Waired の行から見れば同じ
  だが、エンジンに渡る数は違う。この記録はそこを揃えない。

## Refs

- waired-ai/waired-agent#1435 / #1396 / #1434 / #685
- waired-ai/waired#1456(L122、実装) / waired-ai/waired#1067
- `docs/decisions/20260917/0337-engines-serve-only-the-two-tiers.md`(部分的に置き換える)
- `docs/decisions/20260828/1730-vllm-sizing-moves-into-hostfit.md`(触らずに済む)
- 私設側: `waired-ai/waired` `docs/decisions/20260808/2325-capacity-warns-and-asks-not-refuses.md`(散文で復元)、`docs/decisions/20260913/2350-l107-catalog-rulings-variant-kv-residency.md`(決定 9)
