# ja translation terms (pinned)

The ja mirror is hand-translated — there is no machine-translation
pipeline. When `npm run i18n:check` reports a stale pair, update the
Japanese page **around this table**: the term choices below are settled
rulings, never re-derive or "improve" them while retranslating a page.
Changing a row requires an owner ruling; update the row in the same PR
that changes the usage.

Provenance: the 2026-08-03 terminology audit
(waired-ai/waired#1056 → #473) and the CLAUDE.md §Vocabulary and
provenance rules (#468).

## Register

- 断定形・体言止めのコンソール調で書く。soft-assistant 的な語尾は使わない
  （「何もしなくて大丈夫です」→「対応不要」、「〜しましょう」禁止、
  見出しを質問形にしない）。
- 「お使いの」「ご自身の」→「自分の」「このパソコン」。
- CLI / アプリの出力を引用するときは**逐語**で写す（`Mesh` ステータス行、
  `Worker:` メニューラベル、`waired phase` の出力など）。訳したい場合は
  引用の外に補足を添える。出力の引用を「修正」しない。
- フラグ名・JSON キー・識別子は逐語（`--min-model-size`、`model_size` など）。

## Terms

| EN | ja | Rationale | Ruling |
|---|---|---|---|
| below recommended spec (旧 under-spec) | 推奨要件未満 | 最小要件/推奨要件 が PC スペックの定着語。「未満」は境界を含まない=「満たさない」。旧 under-spec は造語で使用禁止 | #465 裁定(20260804) |
| completions (LLM output) | 応答 / 生成結果 | 補完 = 入力補完の意で誤読される | #473 §1 |
| mesh | 初出「Waired メッシュ」→ 以降「メッシュ」 | 一般語のため初出は修飾 | #473 §3 |
| overlay (network) | 初出「オーバーレイネットワーク」 | 〃 | #473 §3 |
| peer | 初出「ピアデバイス」→ 以降「ピア」 | 〃 | #473 §3 |
| worker | 初出「ワーカーマシン」→ 以降「ワーカー」 | 〃（`Worker:` ラベルは逐語） | #473 §3 |
| control plane | コントロールプレーン（= コーディネーションサービス） | glossary で相互リンク | #473 §3 |
| coordination service | コーディネーションサービス | 調整サービスとしない | #473 §3 |
| Network Map | Network Map（初出に 1 行説明） | 固有名詞 | #473 §3 |
| model size (`small`/`medium`/`large`) | サイズ（`small`/`medium`/`large` は逐語） | 「どのクラスのグラフィックボードで動くか」であって品質の主張ではない。訳語を当てず値は英語のまま | #537 |
| ~~quality score (`quality_tier`)~~ | ~~品質スコア（1–100）~~ | **ユーザー向け文面から撤去（#537）**。数値はカタログの内部順位で測定値ではない。以後この語をユーザー面に書かない | #473 §2 → #537 |
| context window | コンテキストウィンドウ | 「窓」単独は禁止 | #473 §4 |
| enrollment / enroll | 登録 | エンロールとしない | #473 §4 |
| sign in | サインイン | | glossary |
| direct connection | 直接接続 | ダイレクト接続としない | #473 §4 |
| relay | リレー（中継） | | glossary |
| serving (local inference) | 推論 | サービングとしない | #473 §4 |
| serve / handle (requests) | 実行する / 処理する | 「配信」はコンテンツ送出のみ | #473 §4 |
| discover | 探索 | 「ディスカバー。」としない | #473 §4 |
| fail-open (privacy page) | 「止めずに続け、使ったことは必ず知らせる」 | セキュリティ用語の fail-open と衝突 | #473 §1 |
| one click each way | 行きも戻りもワンクリック | 「片道」は真逆の意味になる | #473 §1 |
| secrets | 秘密情報 | 「秘密」単独としない | #473 §4 |
| user space | ユーザー空間 | ユーザースペースとしない | #473 §4 |
| main conversation / subagents | メイン会話 / サブエージェント | 「クラス」と書かない | #473 §2 |
| escape hatch | 緊急手段 | 避難口としない | #473 §4 |
| unified memory | ユニファイドメモリ | 「メモリリッチな〜機」等の直訳をしない | #473 §4 |
| full capacity | 同時に受けられる上限 | フルキャパシティとしない | #473 §4 |
| compute-bound | 計算性能がボトルネック | 計算律速はユーザー向けでは使わない | #473 §4 |
| surviving / staying up | 途切れずに継続 | 「生き延びている」としない | #473 §4 |
| surgical (removal) | 「`link` が追加したものだけを取り消す」等、具体に開く | 外科的としない | #473 §4 |
| private by design | 既定でプライベート | 設計からプライベートとしない | #473 §4 |
| declares (its limit) | 明示する / 宣言する | 「名乗る」としない | #473 §4 |
| sized / estimated | 見積もる | 「サイジングされています」としない | #473 §4 |
| introduces (machines) to each other | 互いを見つけられるようにする | 「引き合わせる」としない | #473 §4 |
| gets out of the way | 以後は通信に関与しない | 「脇に退く」としない | #473 §4 |
| beyond their own keyboard | そのパソコン以外からも使えるようにする | 「キーボードの外へ」と直訳しない | #473 §4 |
| each step outward | 外へ広げる段階ごとに | 「外側への一歩一歩」としない | #473 §4 |
| the same switch (install-time) | 同じ設定を指定する | 「スイッチを倒す」としない | #473 §4 |
| becomes a helper machine | 補助マシンとして使える | 「補助機に仕立てる」としない | #473 §4 |
| active params (MoE) | アクティブパラメータ | 「アクティブ少」等、「アクティブ」を単独の名詞にしない | #473 §4 |
| retired (a catalog model) | 退役 | 「廃止」はサポート終了、「削除」はユーザーのデータが消える印象になる。名前は解決し続ける | #200 |
| successor / replacement (model) | 後継モデル | 「代替」としない（品質が劣る含意） | #200 |
| never the automatic choice (`manual_only`) | 自動では選ばれない | 一覧にも残り選択もできるので「除外」「非対応」「非推奨」としない（「非推奨」は deprecated と衝突）。「退役」も使わない（#200 でモデルの撤去に確定済み） | #521 |
| offered (a model) | 提示される | 「提供」は製品としての提供と読めるため、一覧に出て選べる意味では使わない | #521 |
