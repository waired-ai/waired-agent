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

## Rebasing: the conflict is the hash, and picking a side loses a translation

Two PRs that touch the same **English** page will conflict on the ja
page's `sourceHash`, and on nothing else. The hash is one line of
frontmatter; the prose sits in whatever sections each PR happened to
edit, so git merges the bodies cleanly and stops on the line that records
"this pair was looked at".

Which makes the failure misleading. `CONFLICT (content): Merge conflict
in src/content/docs/ja/<page>` reads like a prose collision, and you find
out otherwise only by opening the file. It is also not auto-mergeable, so
whoever lands second pays a full CI cycle for a one-line resolution.

**Resolve by re-deriving, never by choosing.** Take either side to get
the file to parse, then recompute against the MERGED English page:

```sh
npm run i18n:accept -- src/content/docs/ja/<page>
npm run i18n:check          # must report all pairs in sync
```

`--accept` records "this pair was looked at" — it does not translate
anything and does not verify that anyone did. So before you run it,
**read the ja page and confirm both PRs' prose survived the merge.**
Keeping the incoming hash over a body that lost the other side's
paragraph produces a page that builds, passes `i18n:check`, and is
missing a translation. That is the failure mode this step exists to
prevent.

Since #678 one shape of that loss IS caught. `i18n:check` compares the
heading count and the fenced-code-block count of every pair whose hash
already matches, and fails with `Drifted` when they disagree:

```
Drifted — the Japanese page claims to be current, but its shape
no longer matches the English page.
  src/content/docs/ja/getting-started/verify.mdx  (en: 4 headings, 4 code
  blocks; ja: 3 headings, 4 code blocks)
```

Translation changes how many sentences a page has; it does not change
how many headings or code blocks it has. A whole paragraph going missing
usually takes one of the two with it.

Two limits worth knowing:

- **It is a shape check, not a content check.** A paragraph lost from the
  middle of a section, carrying no heading and no code block, still slips
  through. The instruction above — read the page — has not been replaced.
- **It deliberately says nothing while a pair is `stale`.** An English
  page that has moved ahead of its translation is *expected* to differ in
  shape until the translation catches up; failing there would fire on
  every honest piece of work. The comparison starts only once the pair
  claims to be current.

`--accept` refuses a drifted pair rather than skipping it quietly: the
hash already matches, so accepting would write nothing while printing
like an acceptance. Restore the missing content instead — there is no
hash to refresh.

Observed repeatedly through one afternoon of concurrent work
(2026-08-08): #557 rebasing onto #559 on `getting-started/first-run.mdx`,
then #574 again each time #571, #572 and #570 landed under it, across
`first-run.mdx`, `reference/cli.md` and `troubleshooting.md`. It is a
property of two PRs sharing an English page, not bad luck — worth knowing
when sequencing work that touches docs.

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
| the AI engine | AI エンジン | ユーザー向け文面では常に「AI エンジン」と呼び、`ollama` / `vllm` という**内部名を出さない**。ユーザーが選ぶものではなく Waired が入れて `waired update` が更新するもので、名前を知っても操作に結びつかない。フィールド名・ワイヤの engine キー・`waired runtimes ls` の `NAME` 列は逐語（出力引用の規則どおり）。カタログ行の床は `needs AI engine 0.32.13 (this computer has 0.31.1)`、版が読めないときは `(this computer's version could not be read)` | オーナー承認文言(20260819; waired-agent#836 / #850) |
| not available on this computer | このパソコンの AI エンジン向けのビルドが存在しない（製品出力の引用は逐語） | このエンジンが serve できる build を**カタログが持たない**という判定 (`no_variant_for_engine`) の文面。メモリ不足とは別物で、メモリを増やしても解決しない — メモリの数字も内訳も出さない。ルータ側のラベル `no variant supports ollama` は `variant` と内部名の二重違反なので、tray・`models ls --detail`・ピッカー・CLI 警告はすべてこの語で上書きする。CLI の警告は `<名前> is not available on this computer: the AI engine here has no build of it.`、判定が未知のときは `<名前> will not run on this computer: <不足の記述>.` で原因を主張しない | オーナー承認文言(20260819; waired-agent#862 / #850) |
| package index (apt) | パッケージインデックス | apt が事前にダウンロードして持っている、公開版一覧のローカル控え。Linux の `waired update --check` はこれを読むので、答えはインデックスの鮮度までしか新しくない。「パッケージ一覧」「リポジトリ情報」としない — 更新コマンド (`apt-get update`) が更新する対象そのものを指す語で、逐語のほうが読者の操作に直結する。製品出力の `Package index:` ラベルは逐語（出力引用の規則どおり） | オーナー承認文言(20260812; waired-agent#726) |
| below recommended spec (旧 under-spec) | 推奨要件未満 | 最小要件/推奨要件 が PC スペックの定着語。「未満」は境界を含まない=「満たさない」。旧 under-spec は造語で使用禁止 | #465 裁定(20260804) |
| Running AI locally is not recommended here. | このパソコンでのローカル AI 実行は推奨しません。 | 上の「推奨要件未満」から導かれる平易な結論として製品が出力する文。**「非推奨」を使わない** — 同表の `manual_only` 行と同じ理由で deprecated と衝突する。動詞形「推奨しません」なら衝突しない | #579 承認文言(20260809) |
| N s or more (per coding question) | N 秒以上 | 実測ではなく下界（プレフィル律速の下限）であることを示す。「約」「少なくとも」は付けない — 英語側で `about` / `at least` を落とした理由がそのまま当てはまる（要件の下限と読まれる） | #579 承認文言(20260809) |
| comfortable (speed criterion) | 快適 | 判定基準そのものの見出し。数値の横に並べる行なので短語。「推奨」としない（推奨要件と紛れる） | #579 承認文言(20260809) |
| Not recommended (model demotion) | 推奨されません（主語なし受け身。本文が「このコンピュータ」に言及済みなら「推奨されません。」、未言及なら「このコンピュータには推奨されません。」） | 旧 Waired would not choose / Waired は推奨しません の主語落とし。「非推奨」禁止（上の Running AI locally 行）は維持 — deprecated と衝突する名詞形は不可 | waired#1146 裁定(20260812) |
| Continue (setup wizard CTA) | 続行 | 押下時点でエンジン導入と計測は完了済みで、開始するのは選んだモデルの取得のみ。旧 Yes, set it up / セットアップを開始 | waired#1146 §5 裁定(20260812) |
| Model about {W} + session cache about {K} | モデル本体 約 {W} + セッションキャッシュ 約 {K} | ピッカー行の事実行（スペック表記・全行同一）。`weights_resident_mb` と `required_window_resident_mb − weights_resident_mb` の内訳。model_size バケットは NAVI ピッカーから撤去（カタログ面は #537 のまま） | waired#1174 裁定(20260812) |
| RAM and graphics memory combined | RAM と VRAM の合計 | 容量の数え方の定型。推奨文と実行不可文が同一式を使う。統合メモリ機では VRAM=0 扱いの合計（`carve_out_vram_mb` が読めた場合のみ非ゼロ）で二重計上はない — 裁定時に明示のうえ承認 | waired#1146 裁定(20260812) |
| graphics memory | docs-site は「グラフィックスメモリ」（en 直訳・#681 出荷済み）/ NAVI UI は「GPUメモリ」（既存面の統一語）。合算式は上の行 | en のユーザー向け文中の GPU メモリの語（VRAM は en 文中で使わない — フィールド名・KV ラベルは逐語）。ja は面ごとに定着語が異なり、この行がその使い分けを固定する | オーナー承認文言(20260811 CLI/tray; 20260812 NAVI, waired#1146) |
| system RAM (spill destination) | 通常のメモリ | GPU からあふれた分が読まれる先。graphics memory と対で使い、統合メモリ機では両者を足さない（同一バイト）。en は全面 system RAM に統一（旧 ordinary memory — tray/CLI #681 と NAVI で2語併存していたのを解消）。CLI 出力は英語のまま | オーナー承認文言(20260811 CLI/tray) + 統一裁定 waired#1146(20260812) |
| allocatable (fit verdict, predicate use) | 値は英語のまま（NAVI 未使用 — 使う際に改めて裁定） | 「このパソコンがモデルに割り当てられる量」。判定が実際に比較した `have_mb` を指し、搭載量ではない。tray のメニュー行など幅のない面で `needs 11 GB — 6 GB allocatable` の形（述語）でのみ使用。名詞句 allocatable memory / allocatable VRAM は不採用。射程は CLI と tray の2面 | オーナー裁定(20260811、セッション内; 出荷 waired-agent#681) |
| completions (LLM output) | 応答 / 生成結果 | 補完 = 入力補完の意で誤読される | #473 §1 |
| mesh | 初出「Waired メッシュ」→ 以降「メッシュ」 | 一般語のため初出は修飾 | #473 §3 |
| overlay (network) | 初出「オーバーレイネットワーク」 | 〃 | #473 §3 |
| peer | 初出「ピアデバイス」→ 以降「ピア」 | 〃 | #473 §3 |
| Waired peer (`/model` の項目名) | **Waired peer**（逐語・訳さない） | 製品出力の引用なので上の `peer` 行の適用外。picker のラベルは `Waired peer (another device, no local fallback)` で、説明欄は Claude Code が `From gateway` に固定するためラベル 1 行に収めている。散文で意味を補うときは引用の外に添える | オーナー裁定(20260820、セッション内; waired-agent#830) |
| Waired public share (`/model` の項目名) | **Waired public share**（逐語・訳さない） | 〃。ラベルは `Waired public share (someone else's computer)`。Public Share を有効にしたホストにだけ出る条件付きの項目なので、ja 側でも件数を書かない | オーナー裁定(20260820、セッション内; waired-agent#901) |
| worker | 初出「ワーカーマシン」→ 以降「ワーカー」 | 〃（`Worker:` ラベルは逐語） | #473 §3 |
| control plane | コントロールプレーン（= コーディネーションサービス） | glossary で相互リンク。`waired status` と `waired init` のサインイン行が出す **`Control Plane:` ラベルは逐語**（製品出力）。`waired status` は #800 まで `Control:` と短縮していたが、サインイン行と語を揃えて正式名に統一した | #473 §3 → #800 |
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
| keep the model in memory / model residency | モデルをメモリに保持する（時間） | 最後のリクエストからモデルを (V)RAM に残す時間の設定。「常駐」を単独の名詞として立てない（新語になる）。「キャッシュ」も使わない — 消えても再計算で済むものではなく、失効すると重みの再ロードと prompt の読み直しを丸ごと払う。CLI 出力は `Model stays in memory: always.` / `Model stays in memory for 30m0s after the last request.`、アプリの行は `Keep model in memory: always`（引用は逐語） | オーナー承認文言(20260820; waired-agent#861) |
| always (as a residency value) | 保持し続ける（値としては `always` を逐語） | 内部値は `0s` だが、**数値のまま出さない**。`0s` は「即座に降ろす」と読めて意味が逆になる。ja の説明文でも `Always` はボタン名として英語のまま引用し、意味は「解放しません」と書く | オーナー承認文言(20260820; waired-agent#861) |
| unload the model | モデルを降ろす | エンジンを動かしたままモデルのメモリだけを返すこと。エンジンごと止める `engine stop` と**別の操作**なので、両方を「停止」と訳さない。「アンロード」とカタカナにしない。行は `Unload model (free memory)`、降ろすものが無いときは `Model not loaded`（引用は逐語） | オーナー承認文言(20260820; waired-agent#861) |
| (loaded) / (not loaded) | メモリに載っている / 載っていない | モデル行の接尾辞。「起動中」「停止中」としない — エンジンの生死ではなく重みが (V)RAM にあるかどうかで、エンジンは両方の状態で `ready` でありうる。**この語は「メモリ」軸が専有する**: ディスク軸（ネットワークからの取得）は `loading` を明け渡し `downloading` を使う（waired-agent#837） | オーナー承認文言(20260820; waired-agent#861) |
| downloading (engine/peer state) | ダウンロード中 | `subsystem_state` の `loading` が出力される語。ワイヤの値は `loading` のままだが、面には `downloading` と出す。これは**モデルのファイルがネットワークから届いている**ことで、メモリに載っているかどうか（上の行）とは別軸。1 つの語が両方を指していたため、Claude のステータス行に「ダウンロード中」の意味で `local loading` が出ていた。行は tray の `Engine: downloading`、`waired peers list` の `no (downloading)`（引用は逐語） | waired-agent#837 |
| serving now: N requests | いま処理中: N 件 | `waired status` の行。**0 でも必ず出す** — コーディングツールが黙ったまま `0 requests` なら「待ちの原因はこのパソコンではない」と読める、この行の存在理由そのものだから。アプリのモデル行では接尾辞 `(loaded, serving 2 requests)`。「同時実行数」「並列数」としない — 設定値ではなく今の観測値（引用は逐語） | waired-agent#837 |
| model not loaded (status line) | モデルがメモリに載っていません | Claude Code のフッター接尾辞 `· model not loaded`。`(not loaded)` と同じ事実で、面が違う。**色は緑のまま**（この行の黄色は「Waired が答えていない」の意で既に埋まっている）。「未ロード」「ロードされていません」としない | waired-agent#837 |
| kept until unloaded | 降ろすまで保持 | `waired status` の `model loaded:` 行で、無期限に保持されているモデルを表す。エンジンは無期限を**数世紀先の日付**で返すので、そのまま出すと `(until 2318-11-30T12:52:47Z)` になり、設定ではなく不具合に読める（実機で観測、waired-agent#916）。値の語 `always` を流用しないのは、同じ行に並ぶ `(until <時刻>)` と対比したとき `(always)` が読み取りにくいため。`unload the model` と対で読ませる。行は `model loaded:   ollama: <tag> (kept until unloaded)`、降ろされているときは `model loaded:   ollama: no (the next request reloads it)`（引用は逐語） | オーナー承認文言(20260820; waired-agent#911) |
| how a residency change reached the engine | 変更がエンジンに届いた経路 | 設定の書き込みは必ず**どう届いたか**を名乗り、「適用した」を一括で言わない。`(applied live)`＝常駐中のモデルを打ち直した / `(the engine restarted to pick it up)`＝常駐が無かったのでエンジンを再スポーンした / `(applies when the engine starts)`＝エンジンが停止中。エンジンは `OLLAMA_KEEP_ALIVE` を spawn 時にしか読まず、配信は OpenAI 互換の口を通るので per-request の `keep_alive` は捨てられる（実機で観測、waired-agent#908）——経路によって効き方が違うので、黙らせると面が嘘をつく。waired が起動していないエンジンには届かず、その場合だけ別行で `This engine was started outside waired, so it keeps the old setting until it is restarted.` を出す（引用は逐語） | オーナー承認文言(20260820; waired-agent#911) |
| first token (as a `waired status` row) | 最初の1語までの時間 | `model loaded:` の直下に出る行で、直前の回答が始まるまでの実測値と、同じホスト・同じモデルで観測した最短値を並べる。**`cold` / `warm` という語は使わない** — 語には閾値が要り、固定閾値は実機3台のうち少なくとも1台で嘘になる（4b の *warm* 1,960ms は 35b-a3b の *warm* 259ms の 7.5 倍で、どちらも正しい）。さらに TTFT は症状であって機構ではなく、同じ数字は分岐距離でも出る（waired-agent#883）ので、「キャッシュが冷えていた」は測っていないことの主張になる。ja でも「冷えている」「温まっている」を使わず、数字と最短値だけを訳す。時刻の併記は必須 — 1時間前の数字が `model loaded:` の下に出ると次のリクエストへの約束に読める。行は `first token:    35.4s, 12 minutes ago (fastest seen here: 2.6s)`、比較対象が無いときは `first token:    35.4s, 12 minutes ago`（引用は逐語） | decision 20260821（waired-agent#912） |
