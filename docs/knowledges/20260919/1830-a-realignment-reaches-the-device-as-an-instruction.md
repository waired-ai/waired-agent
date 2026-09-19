# 追随はデバイスには指示として届く (20260919 18:30)

## Issue

#1445: ローカルでモデルを切り替える (`waired models use`、降格の
受け入れ) と、数秒後に daemon が元の desired モデルへ戻す。最初の報告は
Linux ホストで、受け入れた降格先は 18 GB のダウンロードが要った。同じ
配線から waired-ai/waired#1454 (コントロールプレーンの追随が、人が選んだ
モデルではなく離れようとしたモデルに寄る) が見つかり、#1418 (CI で
テストが 10 分の deadline までハング) は隣で踏んだ。

修正前後の実測は 2026-09-19、Apple M5 Pro の macOS ホスト (ollama)。
「追随」は、コントロールプレーンが `desired_model_id` をデバイスの
報告に合わせて動かすこと (#647) を指す。

## Learnings

### 1. 追随はデバイスには指示として届く

コントロールプレーンは、デバイスの `LocalModelChoiceAt` が自分の指示
より新しいとき、`desired_model_id` をそのデバイスが `ActiveModel` で
報告しているモデルに動かす (#647。`proto/signer/inference_state.go` の
`LocalModelChoiceAt` のコメント)。

Network Map のフレームは `desired_model_id` などを運ぶが、出自も
書き込み時刻も運ばない。デバイス側の setup reconciler
(`cmd/waired-agent/setup_desired.go`) には、追随とブラウザからの人の
書き込みを区別する材料が無い。見えるのは「見ている間に指示が変わった」
だけで、それは #308 の freshness の判定 (60 分間) にウィザードの
書き込みとまったく同じに掛かる。

### 2. 届いた時点で収束済みだった指示も、適用した指示と同じく使い切る

`stepDesiredModel` は収束経路 (desired がもう serve されている) で
`modelApplied` / `modelAdmitted` を記録せずに return していた。指示は
60 分間 live のままで、push ループの 2 秒刻みの reconcile pass
(#779、`reconcileDesiredModel`) が、次のローカル切替の上にその指示を
再適用した。修正は、収束の return でも両方を記録すること。

`modelAdmitted` も要る。`Apply` の #779 ブロックは最後の admission
でないキーを毎フレーム再武装するので、収束した指示を admission として
記録しないと、その前に admit していたモデルへの戻りが古い admission と
等しく比較され、戻りではなく繰り返しとして黙って落とされる。

### 3. 戻りが間欠的に見えた理由 (修正前の agent、macOS ホスト)

- ディスクに在るモデルへの切替は `ActiveModel` を即座に変える。次の
  push が新しい選択時刻を運び、コントロールプレーンは 0.5〜1.2 秒で
  追随した (preference の書き込みから追随のタイムスタンプまで)。
  reconcile pass の 2 秒より速いので、デバイスは新しい指示に収束し、
  何も戻されない。
- このプロセスが以前に適用した desired 値 (ブラウザから設定して適用
  したもの) は使い切られたままなので、そこへ戻っても戻されない。
- 戻りが決定的になるのは、追随後の値がこのプロセスが一度も適用して
  いないもので、かつローカルの選択がまだ効いていない (ダウンロードが
  要る) とき。`ActiveModel` が変わらないので追随が来ず、reconcile pass
  が選択の 1.1 秒後に古い desired 値を再適用した (08:54:34.14Z →
  08:54:35.27Z、preference が source `desired` で書き直された)。
  Linux ホストで最初に報告された降格の受け入れと同じ形。
- 修正後は、同じ手順で 17 分のダウンロードの間ずっと選択 (source
  `operator`) が残った。

### 4. 選択時刻は、選んだ値と一緒にしか乗せない (waired-ai/waired#1454)

コントロールプレーンが指示を動かす先は push が**報告する**値
(`ActiveModel`) であって、人が選んだ値ではない (push は選んだモデル id
を運ばない)。選んだモデルがまだダウンロード中の間、`ActiveModel` は
離れようとしているモデルを名指すので、そのとき時刻を公開すると指示は
離れる側のモデルに動き、§1 によりデバイスがそれを選択の上に適用した。

修正: probe (`cmd/waired-agent/inference_probe.go`、
`localModelChoiceInForce`) は、正規化した選択のモデル id が同じ push の
`ActiveModel` と等しい間だけ `LocalModelChoiceAt` を公開する。両方が
空 (「ローカルモデル無しで動かす」を選び、何も serve していない) も
効いている扱い。probe の中で、同じ push の値と突き合わせるのが要点:
`ActiveModel` と preference は別々に読まれていた。

実測: desired ≠ served の状態で 2.5 分のダウンロードが要るモデルを
ローカルで選ぶと、コントロールプレーンはダウンロードの間ずっと指示を
保ち、選んだモデルが active になった約 1.8 秒後にそこへ動いた。修正前の
agent では、選択を運ぶ最初の push で追随が起きる (§3 の 0.5〜1.2 秒。
waired-ai/waired#1454 の記録では選択の 11 秒後に、離れる側のモデルへ
動いた)。消費者は既に `""` を「主張なし」として
扱うので変更は無い。proto のフィールドコメントが遅れている (#1447)。

### 5. residency も同じ形 (コードから。実機では再現していない)

`LocalResidencyChoiceAt` は `ResidencyIdleTimeout` が報告する値が何で
あれ乗っていた。vLLM では報告は常に `0s` (#943)、再起動後は
`IDLE_TIMEOUT` / `--inference-idle-timeout` の上書きが保存した選択と
異なり、変更の前半と後半の間に push が読まれることもある。いずれも
コントロールプレーンは residency の指示を誰も選んでいない値に動かし、
デバイスは新しい値をそれぞれ 1 回適用する。今は報告された residency が
記録された選択と等しい間だけ時刻が乗る (`residencyChoiceInForce`、
duration として比較)。記録の `Value` フィールドは以前は診断専用で、
これが比較を可能にした。

### 6. 隣の CLI の罠 (#1445 の「also seen」)

降格の待ちは `Models.Ready` だけで
「`ready. The background service is now serving it.`」を刷り、再計測は
serve されているものを何でも計った。今は成功に `st.Active` が対象を
名指すことが要り、daemon が別のモデルについて報告した数値は捨てる。

### 7. #1418 はテストスタブの競合で、製品側ではない

`benchStub` は最初の POST がハンドラに届く前に `/benchmark/status` に
`running` と答えていた。クライアントはスタブが数えていない要求を放棄
でき、再計測の POST がそのまま永久に保持された。クライアントの POST を
50 ms 遅らせると決定的に再現する (コミットしていない)。

### 8. 次に追随に依存する挙動を実機で検証するとき

- デバイスがコントロールプレーンに接続していることを先に確認する。
  refresh token が失効したデバイスはローカルでは動き続けるが追随を
  受け取らず、テストは空虚に通る。
- コントロールプレーンがそのデバイスの desired モデルを持っていること。
  持っていないデバイスは追随しない。
- 追随はコントロールプレーン側から読む。デバイス側だけでは足りない。

## Refs

- https://github.com/waired-ai/waired-agent/issues/1445
- https://github.com/waired-ai/waired-agent/issues/1418
- https://github.com/waired-ai/waired-agent/issues/1446 (desired の変更が
  ローカル推論を再度 on にする — 追随を含む)
- https://github.com/waired-ai/waired-agent/issues/1447 (proto の
  フィールドコメント)
- #647 / #779 / #308 / #943
- waired-ai/waired#1454 / waired-ai/waired#1232
- `cmd/waired-agent/setup_desired.go` (`stepDesiredModel`、
  `reconcileDesiredModel`)、`cmd/waired-agent/inference_probe.go`
  (`localModelChoiceInForce`、`residencyChoiceInForce`)、
  `proto/signer/inference_state.go` (`LocalModelChoiceAt`)
