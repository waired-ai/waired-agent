---
status: accepted
---

# 永続 preference とプロセス内 preference は同じ関数が両方公開する (20260904 01:50)

## Status
Accepted（waired-agent#1170。waired-ai/waired#1312 レーン L91、rc5 実機検証 record
waired-ai/waired#1309 の F3）

## Context

rc5 の実機検証（Linux / NVIDIA、RTX PRO 4000 24 GB）で、ブラウザウィザードの
推論エンジン選択に vLLM を選び、既定でチェックされている gpt-oss-20b を選ぶと、
エンジンのカードが ERR のまま止まった。`systemctl restart waired-agent` すると
そのまま回復する。つまり「デーモンを再起動する」と知っている人にだけ動く導線だった。

デーモンのログは同じミリ秒に 4 行を並べていた。

```
setup: engine became installed; re-admitting the desired model  engine=vllm model=gpt-oss-20b
adopting a different serving engine  was=ollama now=vllm
vllm bootstrap: no vLLM-capable model selected — …  bundled=qwen3.6-35b-a3b   [ERROR]
setup: model switch needs a restart; downloading now and activating on the next boot  model=gpt-oss-20b
```

`SwapPreferredModel` は、この製品が持つ 2 つの preference を**同じ関数で**公開して
いる。ひとつはディスク上の `preferred-model.json`（呼び出し側が直前に書く）、もう
ひとつは `preferredOverride`。後者は `cfg.PreferredModelID` が起動時スナップショット
であるために存在し、`effectivePreferredModelID()` を通る全読み手が見る値である
（#812）。

ところがエンジンのガードが、その公開行の**手前**で return していた。

```go
if p.servingEngine() != catalog.RuntimeOllama {
    return false, errSwapNeedsRestart      // vLLM ホストはここで抜ける
}
```

コメントは "cross-engine" と書いてあるのに、条件は「ollama でないなら」だった。
vLLM → vLLM の同一エンジン切り替えまで restart 経路に落ちる。ウィザードのホストは
直前に vLLM を採用したばかりなので、選ばれたモデルへの切り替えがこの return を通り、
`preferredOverride` は nil のままになった。以降 `vllmTarget()` は候補として bundled の
ollama 自動選択（qwen3.6-35b-a3b、vLLM variant を持たない）しか見つけられず、
bootstrap は `no vLLM-capable model selected` で拒否した — **例として挙げているのが、
ウィザードがまさに選んだモデル**だという皮肉ごと。

再試行の辺は 1 本も残らなかった。エンジン出現の検知は false→true の辺だけ、desired
model の投入は値ごとに 1 回だけ、そして vLLM では**ダウンロード完了がエンジンに
戻る辺を持っていなかった** — `reconcileEngineServe` は ollama 以外で即 return する。
再起動が効くのは `preferred-model.json` が起動時にしか読み直されないからである。

助言する 2 面もどちらも空振りしていた。`waired doctor --fix` は `waired link all`
だけで、修復計画にエンジンの腕が存在しなかった。`waired inference engine start` は
bootstrap を回し直してはいたが、同じ理由で同じ拒否に落ち、200 を返して CLI が
`engine start ok.` と印字していた。

## Decision

1. **公開の条件は「今動いているエンジンがその model を配れるか」**。エンジン名で
   分岐しない。配れないときだけ `errSwapNeedsRestart`（起動時に `chooseEngine` が
   エンジン種別ごと選び直す）。配れるなら `preferredOverride` を必ず公開する。
   永続側を書いた呼び出し元と、プロセス内側が食い違ったままにならない。

2. **vLLM のモデル切り替えは、エンジンが上がっていないときだけプロセス内で適用する。**
   配信中の vLLM を止めてモデルを差し替える in-process swap は作らない（#347 の
   restart-to-swap を維持）。KV プールは起動時に確保して終了まで保持するので、
   切り替えは「何も配信していない数分」になる。#1170 のホストはそもそも何も
   配信していないので、そこには swap は無く start しかない。

3. **重みの着地はエンジンに戻る辺である**（`noteWeightsLanded`）。切り替え待ちの
   pull が完了したとき、または vLLM の bootstrap が重み不在で拒否を記録していると
   きに、エンジンの起動を要求する。`endPull` はこの意図をエンジンごとに振り分ける
   （`requestEngineSwap`）。どちらの goroutine が先に走っても収束する。

4. **起動できない理由は 1 か所で解決し、明示的な start はそれを先に問う**
   （`vllmStartPlan` / `vllmStartRefusal`）。venv・配れる model・進行中の pull の
   3 つ。`engineController.StartEngine` が同期でこれを問い、駄目なら
   `management.ErrEngineStartRefused` で包んで返す → 409 → CLI が既存の経路で
   その文を印字する。ollama 側もバイナリ未導入で同じ形を返す。
   2026-08-28 の決定（`docs/decisions/20260828/0100-engine-power-and-the-vllm-port.md`）
   が soft トグルについて出した「拒否は理由を印字する、`ok.` ではなく」と同じ形。

5. **`waired doctor --fix` にエンジン修復を足す**。ウィザードの ERR カードは
   `sudo waired doctor --fix` を案内しており、その案内を真にする。デーモンが理由を
   述べているときだけ提示する — オペレータ自身の `waired inference engine stop` と
   起動途中はどちらも「動いていないが何も壊れていない」に見えるので、そこで
   起動を投げるのは誰も頼んでいないことをやり返すことになる。

## Consequences

- ウィザードの vLLM 導線が再起動なしで完了する。ログの列は「公開 → bootstrap 成功」
  か「拒否 → 重み着地 → 再 bootstrap」のどちらかになり、どちらでも収束する。
- `bootstrap` は進行中の pull があるとき重みを取りに行かなくなった。1 の結果、
  bootstrap の解決する target と setup が投げた pull が同じ model になり得るように
  なり、bootstrap 側の取得は in-flight 登録簿を通らないので、同じディレクトリに
  `hf download` が 2 本走り得たため。
- `waired inference engine start` の出力が変わる。起動できないホストでは `ok.` の
  代わりに理由が出る。docs-site のトラブルシュートは既に
  「`waired inference engine start` は原因に対処してから再試行する」と書いており、
  その説明はそのまま真である。
- `probeObservability` / `collectDoctorFindings` が 2 値を返すようになった
  （findings と修復可否）。行を印字した瞬間と修復が動く瞬間が別の観測にならない
  ように、同じ probe から返す。
- **`AgentState` に新しいフィールドは足していない**。「壊れている」と「止められて
  いる」は `EngineFailureReason` の有無で分かれる — 前者は述べる理由を持ち、後者は
  持たない。ワイヤを増やさずに済んだ。
- 副産物として、docs-site が説明していた `waired doctor` の項目
  **inference engine app signature** が製品に存在しないことが分かった（#492 が
  /Applications への Ollama.app 導入をやめた時点で、その修復ごと消えている）。
  項目の記述は実在する **inference engine** の項目に差し替えた。同じ節に残る
  「そのファイルを 1 つ消せば直る」という説明は #492 起点の別の陳腐化なので、
  この決定では触らず別 issue にした。

## Refs
- https://github.com/waired-ai/waired-agent/issues/1170
- https://github.com/waired-ai/waired/issues/1312 （tracking, レーン L91）
- https://github.com/waired-ai/waired/issues/1309 （rc5 実機検証 record, F3）
- docs/decisions/20260828/0100-engine-power-and-the-vllm-port.md （拒否は理由を印字する）
- docs/decisions/20260828/2130-the-boot-path-records-that-it-stopped.md （3 種の give-up と needsRepair が latch 限定である理由）
- docs/decisions/20260828/1830-the-give-up-message-carries-the-diagnosis.md （bootstrap 拒否を状態にした #1075）
- docs/knowledges/20260830/0105-fail-open-guards-need-a-recoverability-line.md （「まだ試している」と「もう試さない」の軸）
