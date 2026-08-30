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
heading count, the fenced-code-block count and — since #1011 — the count
of each MDX component (`<LinkCard>`, `<Aside>`, `<Expected>`, …) of every
pair whose hash already matches, and fails with `Drifted` when they
disagree:

```
Drifted — the Japanese page claims to be current, but its shape
no longer matches the English page.
  src/content/docs/ja/getting-started/verify.mdx  (en: 4 headings, 4 code
  blocks; ja: 3 headings, 4 code blocks)
  src/content/docs/ja/guides/claude-code.mdx  (en: 6 headings, 12 code
  blocks; ja: 6 headings, 12 code blocks; components en/ja: LinkCard 2/3)
```

Translation changes how many sentences a page has; it does not change
how many headings, code blocks or components it has. A whole paragraph
going missing usually takes one of the three with it. Components are the
case where the page can keep every heading and every code sample and
still have lost something — that is how the OpenCode restore dropped the
OpenClaw card from two English pages unnoticed (#1010). Only capitalised
tags are counted: lowercase ones are HTML (`<a id>` anchors, `<kbd>`),
which the two sides may legitimately use differently.

Two limits worth knowing:

- **It is a shape check, not a content check.** A paragraph lost from the
  middle of a section, carrying no heading and no code block, still slips
  through. The instruction above — read the page — has not been replaced.
- **It deliberately says nothing while a pair is `stale`.** An English
  page that has moved ahead of its translation is *expected* to differ in
  shape until the translation catches up; failing there would fire on
  every honest piece of work. The comparison starts only once the pair
  claims to be current.
- **It compares the two sides to each other, not to the truth.** A
  mistake made the same way in both languages — the glossary's coding
  agent entry lost OpenClaw in en and ja together (#1010) — and anything
  outside `docs-site/` (`SECURITY.md`) are invisible to it by
  construction. A green check means the pair agrees, not that the pair
  is right.

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
| the inference engine (旧 the AI engine) | 推論エンジン | ユーザー向け文面は確立した用語で書く(対象読者は LLM のローカル実行にある程度の知識がある人)。**事実がエンジン固有の行は Ollama / vLLM を名指しする** — 版の床 `needs Ollama 0.32.13 (this computer has 0.31.1)`、版が読めないときは `needs Ollama 0.32.13 (this computer's version could not be read)`、エンジン不在は `needs Ollama 0.32.13 (no inference engine on this computer)`。総称としては inference engine。フィールド名・ワイヤの engine キー・`waired runtimes ls` の `NAME` 列は逐語。旧「AI エンジン」「AI ソフトウェア」は使わない(20260819 の「内部名を出さない」裁定を反転) | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| no Ollama variant (旧 not available on this computer) | この推論エンジン向けの variant(量子化ビルド)が無い(製品出力の引用は逐語) | カタログがこのエンジン向けのビルドを持たない判定(`no_variant_for_engine`)。ルータのラベル `no Ollama variant` を tray・`models ls --detail`・ピッカーが**そのまま繰り返す**(上書きしない)。CLI の警告は `<名前> has no Ollama variant, so the inference engine on this computer cannot run it.`、tray のダイアログは `<名前> has no Ollama variant.` メモリの話ではないので数字を出さない | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| package index (apt) | パッケージインデックス | apt が事前にダウンロードして持っている、公開版一覧のローカル控え。Linux の `waired update --check` はこれを読むので、答えはインデックスの鮮度までしか新しくない。「パッケージ一覧」「リポジトリ情報」としない — 更新コマンド (`apt-get update`) が更新する対象そのものを指す語で、逐語のほうが読者の操作に直結する。製品出力の `Package index:` ラベルは逐語（出力引用の規則どおり） | オーナー承認文言(20260812; waired-agent#726) |
| below recommended spec (旧 under-spec) | 推奨要件未満 | 最小要件/推奨要件 が PC スペックの定着語。「未満」は境界を含まない=「満たさない」。旧 under-spec は造語で使用禁止 | #465 裁定(20260804) |
| Local inference is not recommended on this computer. (旧 Running AI locally is not recommended here.) | このコンピュータでのローカル推論は推奨しません。 | 「推奨要件未満」から導かれる結論として製品が出力する文。**「非推奨」を使わない**(`manual_only` 行と同じ理由)。「ローカル AI」は使わず「ローカル推論(local inference)」。直前の行は `This computer is below the recommended spec for local inference.` | #579 承認文言(20260809)→ オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| N s or more (per request) (旧 per coding question) | N 秒以上(1 リクエストあたり) | 実測ではなく下界(プレフィル律速の下限)であることを示す。「約」「少なくとも」は付けない。行は `210.4 s or more per request (target: 45 s or less)`、表の行は `per request           210.4 s or more` | #579 承認文言(20260809)→ オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| target (speed criterion) (旧 comfortable) | 目標 | 判定基準の見出し語。数値の横に並べる行なので短語。表の行は `target                45 s or less`、括弧書きは `(target: 45 s or less)`。「推奨」としない(推奨要件と紛れる) | #579 承認文言(20260809)→ オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| Not recommended (model demotion) | 推奨されません（主語なし受け身。本文が「このコンピュータ」に言及済みなら「推奨されません。」、未言及なら「このコンピュータには推奨されません。」） | 旧 Waired would not choose / Waired は推奨しません の主語落とし。「非推奨」禁止（上の Running AI locally 行）は維持 — deprecated と衝突する名詞形は不可 | waired#1146 裁定(20260812) |
| Continue (setup wizard CTA) | 続行 | 押下時点でエンジン導入と計測は完了済みで、開始するのは選んだモデルの取得のみ。旧 Yes, set it up / セットアップを開始 | waired#1146 §5 裁定(20260812) |
| Weights about {W} + KV cache about {K} (旧 Model about {W} + session cache about {K}) | 重み 約 {W} + KV キャッシュ 約 {K} | ピッカー行の事実行(スペック表記・全行同一)。`weights_resident_mb` と `required_window_resident_mb − weights_resident_mb` の内訳。「セッションキャッシュ」「コンテキストキャッシュ」「作業用メモリ」は使わない — 標準語は KV cache。CLI / tray の接尾辞は `· 2.5 GB of KV cache in system RAM` | waired#1174 裁定(20260812)→ オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| RAM + VRAM combined (旧 RAM and graphics memory combined) | RAM と VRAM の合計 | 容量の数え方の定型。推奨文 `Chosen from this computer’s RAM + VRAM combined.` と実行不可文が同一式を使う。統合メモリ機では VRAM=0 扱いの合計で二重計上はない | waired#1146 裁定(20260812)→ オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| VRAM (旧 graphics memory) | VRAM | en・ja とも **VRAM**(docs-site の「グラフィックスメモリ」、NAVI の「GPUメモリ」を統一)。GPU 本体は **GPU**(旧 graphics card / グラフィックボード)。行は `needs 24 GB of VRAM (have 8 GB)`、size 凡例は `small — fits an 8 GB GPU` | オーナー承認文言(20260811/20260812)→ オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| system RAM (spill destination) | システム RAM(旧 通常のメモリ) | GPU からあふれた分が読まれる先。VRAM と対で使い、統合メモリ機では両者を足さない(同一バイト)。en は system RAM に統一(`system memory` も使わない)。行は `…is read from system RAM, which is slower.` | オーナー承認文言(20260811)+ waired#1146 → オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| allocatable (fit verdict, predicate use) | 値は英語のまま（NAVI 未使用 — 使う際に改めて裁定） | 「このパソコンがモデルに割り当てられる量」。判定が実際に比較した `have_mb` を指し、搭載量ではない。tray のメニュー行など幅のない面で `needs 11 GB — 6 GB allocatable` の形（述語）でのみ使用。名詞句 allocatable memory / allocatable VRAM は不採用。射程は CLI と tray の2面 | オーナー裁定(20260811、セッション内; 出荷 waired-agent#681) |
| completions (LLM output) | 応答 / 生成結果 | 補完 = 入力補完の意で誤読される | #473 §1 |
| mesh | 初出「Waired メッシュ」→ 以降「メッシュ」 | 一般語のため初出は修飾 | #473 §3 |
| overlay (network) | 初出「オーバーレイネットワーク」 | 〃 | #473 §3 |
| peer | 初出「ピアデバイス」→ 以降「ピア」 | 〃 | #473 §3 |
| Waired peer (`/model` の項目名) | **Waired peer**（逐語・訳さない） | 製品出力の引用なので上の `peer` 行の適用外。picker のラベルは `Waired peer (another device, no local fallback)` で、説明欄は Claude Code が `From gateway` に固定するためラベル 1 行に収めている。散文で意味を補うときは引用の外に添える | オーナー裁定(20260820、セッション内; waired-agent#830) |
| Waired public share (`/model` の項目名) | **Waired public share**（逐語・訳さない） | 〃。ラベルは `Waired public share (someone else's computer)`。Public Share を有効にしたホストにだけ出る条件付きの項目なので、ja 側でも件数を書かない | オーナー裁定(20260820、セッション内; waired-agent#901) |
| Share this computer / Stop sharing this computer / Sharing: enabled\|disabled\|paused (アプリのメニュー行) | **逐語・訳さない** | Waired アプリのメニュー項目と状態行。ja ページでも英語のまま引く — 上の `Waired peer` 行と同じ理由で、製品出力の引用だから(CLAUDE.md「Docs quote product output verbatim」)。地の文で意味を補うときは引用の外に添える。文言を変えるときは製品文字列と docs を同じ PR で動かす | waired-ai/waired#1297 オーナー裁定(20260830) → 出荷 waired-ai/waired-agent#1164 |
| Your other computers / People outside your account (共有の配分名) | **逐語・訳さない** | `waired share status` の行名と Waired コンソールのトグル名。どちらも同じ 2 つの配分を指す — 自分のアカウントの他のパソコン、アカウント外の人。「メッシュ共有」「公開共有」は説明の地の文でのみ使い、行名としては使わない | waired-ai/waired#1297 オーナー裁定(20260830) → 出荷 waired-ai/waired-agent#1164 |
| worker | 初出「ワーカーマシン」→ 以降「ワーカー」 | 〃（`Worker:` ラベルは逐語） | #473 §3 |
| control plane | コントロールプレーン（= コーディネーションサービス） | glossary で相互リンク。`waired status` と `waired init` のサインイン行が出す **`Control Plane:` ラベルは逐語**（製品出力）。`waired status` は #800 まで `Control:` と短縮していたが、サインイン行と語を揃えて正式名に統一した | #473 §3 → #800 |
| control plane (旧 coordination service) | コントロールプレーン | 上の `control plane` 行に一本化。glossary は `coordination service` を旧称として 1 項目残す。公式サイトの FAQ も control plane | #473 §3 → オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| Network Map | Network Map（初出に 1 行説明） | 固有名詞 | #473 §3 |
| prefer (`speed` / `size`) | どちらを優先するか（`speed`/`size` は逐語） | `waired worker set --prefer=` と Waired アプリの **When several computers can answer** 群。値は英語のまま。`size` を「品質」と訳さない — #537 が品質の数値を操作者の面から外したのと同じ理由で、大きい＝良いという読みを持ち込まない語として `size` が選ばれている | オーナー裁定(20260829; waired-ai/waired-agent#1128) |
| smallest model to route to | ルーティング先の最小のモデル | `waired worker get` の `smallest model:` 行（逐語）と Waired アプリの **Smallest model to route to** 群。`waired public use` の `Smallest model accepted` と同じ概念で、値は `any` / `small` / `medium` / `large`（逐語）。「下限」は説明文でのみ使い、ラベルには使わない | オーナー裁定(20260829; waired-ai/waired-agent#1128) |
| Waired node | Waired ノード（製品文字列は逐語） | `no Waired node runs a medium model or larger` などのフォールバック理由。CLI が既に `waired worker` to choose which Waired node serves it と言っているので、この面では **node** を使う。docs の地の文は従来どおり「パソコン」 | オーナー裁定(20260829; waired-ai/waired-agent#1128) |
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
| worker (旧 becomes a helper machine / worker machine) | ワーカーとして使える | 「補助マシン」「ワーカーマシン」は使わず、上の `worker` 行の語に統一 | #473 §4 → オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| active params (MoE) | アクティブパラメータ | 「アクティブ少」等、「アクティブ」を単独の名詞にしない | #473 §4 |
| retired (a catalog model) | 退役 | 「廃止」はサポート終了、「削除」はユーザーのデータが消える印象になる。名前は解決し続ける | #200 |
| successor / replacement (model) | 後継モデル | 「代替」としない（品質が劣る含意） | #200 |
| never the automatic choice (`manual_only`) | 自動では選ばれない | 一覧にも残り選択もできるので「除外」「非対応」「非推奨」としない（「非推奨」は deprecated と衝突）。「退役」も使わない（#200 でモデルの撤去に確定済み） | #521 |
| offered (a model) | 提示される | 「提供」は製品としての提供と読めるため、一覧に出て選べる意味では使わない | #521 |
| keep-alive (旧 keep the model in memory / model residency) | keep-alive(説明文では「最後のリクエストの後にモデルをメモリに残す時間」と開く) | Ollama の `keep_alive` と同じ語。「常駐」を単独の名詞にしない、「キャッシュ」は使わない(失効すると重みの再ロードと prompt の読み直しを丸ごと払う)は維持。CLI 出力は `Keep-alive: always (the model stays loaded).` / `Keep-alive: 30m0s after the last request.`、アプリの行は `Keep-alive: always`(引用は逐語)。`waired inference residency` のコマンド名は逐語 | オーナー承認文言(20260820; #861)→ オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| always (as a residency value) | 保持し続ける（値としては `always` を逐語） | 内部値は `0s` だが、**数値のまま出さない**。`0s` は「即座に降ろす」と読めて意味が逆になる。ja の説明文でも `Always` はボタン名として英語のまま引用し、意味は「解放しません」と書く | オーナー承認文言(20260820; waired-agent#861) |
| unload the model | モデルを降ろす | エンジンを動かしたままモデルのメモリだけを返すこと。エンジンごと止める `engine stop` と**別の操作**なので、両方を「停止」と訳さない。「アンロード」とカタカナにしない。行は `Unload model (free memory)`、降ろすものが無いときは `Model not loaded`（引用は逐語） | オーナー承認文言(20260820; waired-agent#861) |
| (loaded) / (not loaded) | メモリに載っている / 載っていない | モデル行の接尾辞。「起動中」「停止中」としない — エンジンの生死ではなく重みが (V)RAM にあるかどうかで、エンジンは両方の状態で `ready` でありうる。**この語は「メモリ」軸が専有する**: ディスク軸（ネットワークからの取得）は `loading` を明け渡し `downloading` を使う（waired-agent#837） | オーナー承認文言(20260820; waired-agent#861) |
| downloading (engine/peer state) | ダウンロード中 | `subsystem_state` の `loading` が出力される語。ワイヤの値は `loading` のままだが、面には `downloading` と出す。これは**モデルのファイルがネットワークから届いている**ことで、メモリに載っているかどうか（上の行）とは別軸。1 つの語が両方を指していたため、Claude のステータス行に「ダウンロード中」の意味で `local loading` が出ていた。行は tray の `Engine: downloading`、`waired peers list` の `no (downloading)`（引用は逐語） | waired-agent#837 |
| serving now: N requests | いま処理中: N 件 | `waired status` の行。**0 でも必ず出す** — コーディングツールが黙ったまま `0 requests` なら「待ちの原因はこのパソコンではない」と読める、この行の存在理由そのものだから。アプリのモデル行では接尾辞 `(loaded, serving 2 requests)`。「同時実行数」「並列数」としない — 設定値ではなく今の観測値（引用は逐語） | waired-agent#837 |
| model not loaded (status line) | モデルがメモリに載っていません | Claude Code のフッター接尾辞 `· model not loaded`。`(not loaded)` と同じ事実で、面が違う。**色は緑のまま**（この行の黄色は「Waired が答えていない」の意で既に埋まっている）。「未ロード」「ロードされていません」としない | waired-agent#837 |
| kept until unloaded | 降ろすまで保持 | `waired status` の `model loaded:` 行で、無期限に保持されているモデルを表す。エンジンは無期限を**数世紀先の日付**で返すので、そのまま出すと `(until 2318-11-30T12:52:47Z)` になり、設定ではなく不具合に読める（実機で観測、waired-agent#916）。値の語 `always` を流用しないのは、同じ行に並ぶ `(until <時刻>)` と対比したとき `(always)` が読み取りにくいため。`unload the model` と対で読ませる。行は `model loaded:   ollama: <tag> (kept until unloaded)`、降ろされているときは `model loaded:   ollama: no (the next request reloads it)`（引用は逐語） | オーナー承認文言(20260820; waired-agent#911) |
| how a residency change reached the engine | 変更がエンジンに届いた経路 | 設定の書き込みは必ず**どう届いたか**を名乗り、「適用した」を一括で言わない。`(applied live)`＝常駐中のモデルを打ち直した / `(the engine restarted to pick it up)`＝常駐が無かったのでエンジンを再スポーンした / `(applies when the engine starts)`＝エンジンが停止中。エンジンは `OLLAMA_KEEP_ALIVE` を spawn 時にしか読まず、配信は OpenAI 互換の口を通るので per-request の `keep_alive` は捨てられる（実機で観測、waired-agent#908）——経路によって効き方が違うので、黙らせると面が嘘をつく。waired が起動していないエンジンには届かず、その場合だけ別行で `This engine was started outside waired, so it keeps the old setting until it is restarted.` を出す（引用は逐語） | オーナー承認文言(20260820; waired-agent#911) |
| first token (as a `waired status` row) / time to first token (TTFT) in prose | 行は逐語 `first token:`、散文は「最初のトークンまでの時間 (TTFT)」 | `model loaded:` の直下に出る行で、直前の回答が始まるまでの実測値と最短値を並べる。**`cold` / `warm` という語は使わない**(閾値が要り、固定閾値は実機で嘘になる; waired-agent#883)。散文では標準語 TTFT で呼び、「最初の 1 語」とは書かない。行は `first token:    35.4s, 12 minutes ago (fastest seen here: 2.6s)`(引用は逐語) | decision 20260821(waired-agent#912)→ 散文の語は オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| ● / ○ / ◐ / ⚠ (アプリの状態記号) | 記号は逐語（訳さない・置き換えない） | アプリのトップの状態行とピア行が使う 4 つ。**● 動いている / ○ 動いていないが、誰のせいでもない / ◐ 進行中 / ⚠ 何かが壊れた**。○ と ⚠ の切り分けが要点で、エンジンを入れていないパソコン・止めてあるエンジン・ローカル推論を切ってある設定は**どれも本人が選んだ状態**なので ⚠ にしない。正常な機で警告を出し続けたのがこの区別を入れた理由（waired-agent#1032） | waired-agent#1032 |
| Engine: off on this computer / none on this computer | このパソコンでは切ってある / このパソコンには無い | アプリのトップの Engine 行。`subsystem_state` の `disabled` / `no_engine` に対応するが、**行の上に「どのパソコンの話か」を言う見出しが無い**ので、サブメニュー内の `Engine: disabled` と違い「どこで」を言い切る。off＝設定で切ってある、none＝そもそも入っていない。どちらも正常な状態として ○（引用は逐語） | waired-agent#1032 |
| Peers: N of M serving | ほかの M 台のうち N 台が答えられる | アプリのトップの Peers 行。「接続中」「オンライン」としない — 疎通ではなく**いまリクエストに答えられるか**で、判定はルータが実際に照合するものと同じ（waired#1064）。ピアが 0 台のときは行を出さない | waired-agent#1032 |
| Claude Code: routed through Waired / not routed through Waired / routed elsewhere | Waired 経由になっている / なっていない / ほかへ向いている | アプリのトップの Claude Code 行。**「答えられるか」ではなく「どこへ送っているか」**を言う行で、判定は managed settings の `ANTHROPIC_BASE_URL` だけ（`waired claude status` と init の完了ボックスと同じ述語）。この 2 つを混ぜたのが waired-agent#1032 で、ピアが処理している最中に「routing inactive」と出ていた。routed elsewhere はほかのプロキシが同じ変数を握っている状態で、Waired は上書きしない（引用は逐語） | waired-agent#1032 |
| No engine is answering | 答えられるエンジンがありません | アプリのトップの ⚠ 見出し。**供給の話**（このパソコンのエンジンもピアのエンジンも答えられない）であって、配線の話ではない。旧「Claude Code routing inactive」は配線を名指ししていたが、実際には managed settings も待ち受けも正常だった。「ルーティングが無効」「接続されていません」としない | waired-agent#1032 |
| Status… (アプリのメニュー行) | 逐語（訳さない） | `Open Admin Console…` の上に出る行。押すと下の状態ダイアログが開く。状態を伝える行はどれも同じものを開くので、この行は**それを知らない人のための入口**であって、唯一の入口ではない。「ステータス」「状況」としない | オーナー要求(20260828; waired-agent#1090) |
| Waired status / Copy details / Close (状態ダイアログ) | 逐語（訳さない） | ダイアログの表題と 2 つのボタン。**Copy details は「より詳しい版をクリップボードへ」**であって画面の内容のコピーではない（全ピア＋識別子・アドレス・接続の種類・時刻が付く）。Close はクリップボードに触れない。Windows は `MessageBoxW` がボタン名を変えられないので本文末に `[Yes = Copy details]   [No = Close]` の凡例が付く（引用は逐語） | オーナー要求(20260828; waired-agent#1090) |
| THIS COMPUTER / OTHER COMPUTERS / RECENT / MESH MAP (状態ダイアログの見出し) | 逐語（訳さない） | ダイアログ内の区切り。ダイアログは Windows/macOS とも等幅でもスクロールでもないので**表を組まない**——見出し＋字下げした行だけで、ピアは 10 台で切って残りは `+N more — on the clipboard` と言う。MESH MAP はクリップボード側にしか出ない | オーナー要求(20260828; waired-agent#1090) |
| グレーアウト（アプリのメニュー行） | 「いまはできない」の意味に限る | **状態を伝える行をグレーにしない**。グレーはどの OS でも unavailable の意味で（Windows UX Guide「refer to unavailable menu items as unavailable, not as dimmed, disabled, or grayed」/ GNOME HIG「make a menu item insensitive when its command is unavailable」）、正常な状態をグレーで出すと「壊れている」と読まれた。グレーのまま正しいのは**セクション見出し**と**本当に実行できない操作**（`Model not loaded` など）の 2 つだけ。有効な行はクリックでメニューが閉じる（3 OS 共通・回避不能）ので、グレーを外す行には必ず行き先を与える | オーナー報告(20260828; waired-agent#1090) |
| LLM | LLM(訳さない) | 大規模言語モデルの総称・クラス名。個別には「モデル」。「AI モデル」「LLM モデル」(重複語)は使わない | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| model (旧 AI model) | モデル | `Choose the model for this computer`、`Download the model`、`Run models on this computer?`(旧 Run AI models on this computer?)。glossary の定義は「大規模言語モデル(LLM)」 | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| local inference (旧 local AI) | ローカル推論 | `Pause local inference` と同じ語。`Skipping local inference`、`Non-interactive: skipping local inference (…)`、サインイン box の `local inference starts off on this computer`、installer の `Local inference is not running on this device.`(引用は逐語) | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| KV cache (旧 context cache / session cache / working memory) | KV キャッシュ | 推論中にトークンごとに保持するメモリ。glossary に定義。`models ls --detail` の凡例は `"KV cache in system RAM" is the part of a full coding session this computer's GPU cannot hold.` | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| variant / quantization | variant(量子化ビルド) | カタログがエンジンごとに持つビルド。「ビルド」単独や「build of it」は使わない。glossary に定義 | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`) |
| benchmark (旧 speed check / speed test / measure how fast this computer runs AI / 速度を測定 / 速度の確認) | ベンチマーク | `Benchmarking this computer with a small model — one-time, a few minutes…`(引用は逐語)。「速度テスト」「速度チェック」としない。NAVI ウィザードのステップ行とボタンは `Benchmark the inference speed` / 「推論速度をベンチマーク」— 「速度」単独は回線速度と読まれた(waired-ai/waired#1286) | オーナー裁定(20260822; waired-ai/waired#1272; `docs/decisions/20260822/2029-user-copy-uses-standard-llm-terms.md`)。ウィザードの行はオーナー裁定(20260824; waired-ai/waired#1286) |
| coding agent / coding tools | 概念語は**コーディングエージェント**(glossary の定着語)。製品出力 `🔌 Setting up your coding tools (claude-code, openclaw, opencode)…` を引用する文脈だけ「コーディングツール」 | 同じものを指す 2 語が公開面に併存している(製品出力と glossary 見出しがそれぞれ先に定着)。逐語引用の規則が優先するので glossary 語に一本化はしない。散文で新しく書くときは常にエージェント側 | オーナー裁定(20260823; waired-ai/waired-agent#1014) |
| command (OpenCode) / skill (Claude Code) | OpenCode 側は「コマンド」、Claude Code 側は「スキル」。**1 語に揃えない**。`/waired-status`・`/waired-doctor`、ファイル名 `waired-status.md`、doctor の `opencode command waired-status` / `claude-code skill waired-status` は逐語 | ツールが自分の面でそう呼んでいる語。統一すると製品出力の逐語引用と食い違い、ユーザーがそのツールの文書で探せなくなる | オーナー裁定(20260823; waired-ai/waired-agent#1014) |
| OpenCode / OpenClaw | 逐語。訳さず、初出でも修飾を付けない | 製品名(`Network Map` 行と同じ固有名詞扱い)。`astro.config.mjs` のナビと既存 ja ページ全面で確立 | 今日の挙動の記録(20260823) |
| `waired/default` (OpenCode / OpenClaw のモデル) | 逐語(識別子)。説明は「このパソコンで Waired が動かしているモデル」。OpenCode の表示名 **Waired Default** と区分名 **Waired** も逐語 | 旧 `waired/coding` / `waired/small` は #521 で退役済みなので書かない | 今日の挙動の記録(20260823; `internal/integration/opencode/templates/plugin_waired.js.tmpl`) |
| provider (プラグインが登録するもの) | 製品出力 `(registers the 'waired' provider)` は逐語。OpenCode のモデル選択の区分名としては「**Waired** プロバイダ」。散文で Waired 自体を「プロバイダ」と呼ばない(「接続先として登録する」) | `cmd/waired/link_helper.go` のヘルパ文と、OpenCode の画面上の語 | 今日の挙動の記録(20260823) |
| `<tool> installation` (doctor の行) — a leftover folder is not an installation | 「そのツールのコマンドが `PATH` に無ければインストール済みとしない。残ったフォルダはインストールではない」。doctor の行は逐語 | #652 の裁定。`internal/integration/adapter.go` の `InstallationFinding`。OpenCode 側は #1004 / #1007 で同じ挙動に | waired-ai/waired-agent#652 |
| `OpenCode integration: ● configured` / `Config: ✓ …` / `Reconfigure…` (tray の行) | 逐語。Waired アプリの導線は「**Waired アイコン** → **Settings** → **OpenCode**」 | `internal/gui/tray/state.go`。OpenClaw の行と同形 | 今日の挙動の記録(20260823) |
| Recent activity の理由: `engine not ready` / `sharing off` / `at capacity` / `legacy peer` / `auth error` / `transport error` / `paused` | 逐語(英語のまま)。散文で開くときは「エンジンが準備できていない」「共有がオフ」「同時受け入れの上限に達していた」 | tray の Recent activity 行が出す語。**ワイヤのタグ(`engine_not_ready` など)は書かない** — 行に出るのはこの英語で、タグは `X-Waired-Fallback-Reason` ヘッダと `waired` のイベントリングにだけ残る。`internal/gui/tray/state.go` の `fallbackReasonLabel`。エンジン状態側の同じ変換は `inferencemesh.ConditionLabel` | waired-ai/waired-agent#1100 |
| local gateway (旧 Local Gateway / data-plane gateway) | ローカルゲートウェイ | このパソコンで OpenAI / Anthropic 互換の API を答える待ち受け。**1 本しかない**(`127.0.0.1:9473`)。旧称の 2 語は同じ物を指していた — トークンを要求する `Local Gateway` と、要求しない `data-plane gateway` に分かれていたが、トークンごと廃止して統合した。`waired doctor` の行 `Local Gateway — HTTP 200 at http://127.0.0.1:9473/v1/models` は逐語 | オーナー裁定(20260823; waired-ai/waired#1277) |
| gateway token (廃止) | — | **書かない。** `<state>/secrets/gateway-token` と `Authorization: Bearer` の要求はもう存在しない。「API キー」を要求する外部ツールには「Waired は検査しないので何を入れてもよい」と書く(`No API key. Waired does not check one. If your tool requires an API key, any value works.` は逐語)。doctor の `gateway token` 行も撤去済み | オーナー裁定(20260823; waired-ai/waired#1277) |
| data-plane gateway / `:9479` (廃止) | — | **書かない。** 2 本目の待ち受けは無くなり、9479 は空き番号に戻った。コーディングツールのプラグインも `waired infer` も `127.0.0.1:9473` を指す | オーナー裁定(20260823; waired-ai/waired#1277) |
| ! running here with a warning (`models ls --detail` の FIT 列) | 逐語(英語のまま)。散文で開くときは「モデルは動いているが、行の残りが予測する構成をこのパソコンが保持できなかった」 | エンジンがいま実際に動かしているモデルの行にだけ出る値。**「モデルに警告が付いている」と書かない** — KV キャッシュのシステム RAM 退避のように、警告を伴っても予測どおり動くトレードとは別で、構成を保持できなかった場合の値。表の下の `<モデル> is running on this computer with a warning:` + エンジンの記録文、凡例 `FIT says whether a model fits this computer. …` も逐語。`waired status` が同じ文を繰り返し、`waired doctor` が対処を言う | オーナー承認文言(20260827; waired-agent#1035 / #1038) |
