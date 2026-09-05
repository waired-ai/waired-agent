---
status: accepted
---

# ja ミラーの鮮度は、ページに保存する値ではなく PR の規則にする (20260906 02:23)

## Status

Accepted（オーナー裁定 2026-09-06、waired-agent#1215）

## Context

`docs-site/src/content/docs/ja/**` の各ページは frontmatter に `sourceHash`
——英語ページ全体の sha256 先頭 16 桁——を持ち、`npm run i18n:check` がそれを
再計算して「英語が動いたのに ja が追いついていない」を検出していた（#147 で導入）。

これは**派生値を版管理下のファイルに 1 行として保存する**形なので、同じ英語ページを
触る 2 本の PR は必ずその行を別々の値に書き換え、必ず衝突する。prose は綺麗に
マージされ、衝突するのは「この対を見た」を記録する行だけ。#578 がこの性質と解決手順を
`docs-site/TRANSLATION.md` に書き留めたが、機構自体は直さなかった。

実測（main、60 日）:

- ja ページを触る commit は 219 / 622 = **35.2%**。そのうち 90.4% が `sourceHash` を書き換える。
- 30 日で ja ページを触った PR は 120 本、平均オープン 1.16 時間、**最大同時 9 本**。
- 重なった PR 対 144 のうち **35 対**が同じ ja ファイルを共有した。
- 最も競合するのは `reference/cli.md`（16 対）と `troubleshooting.md`（12 対）。

#578 が価格付けたのは「衝突 1 回あたりのコスト」だった。今日のレーン数では
**着地条件**になる。2026-09-03、#1198 は 32 枚中 12 枚の ja を 2 時間 19 分保持し、
4 回 CONFLICTING した。4 回の force-push はいずれも競合 PR の merge から
4〜9 分後で（#1195→6m35s、#1207→9m23s、#1203→6m00s、#1211→4m06s）、
その日の答えは「#1198 が着地するまで全員の merge を止める」だった。

検討して落とした選択肢:

- **git merge driver / `.gitattributes`**: カスタム driver はローカルの
  `git config merge.<name>.driver` に登録されて初めて動く。GitHub がサーバ側で行う
  mergeability 計算には届かないので、PR が `CONFLICTING / DIRTY` になるのを止められない
  （競合中の PR は CI が 1 本も回らない）。手元の rebase が楽になるだけ。
- **マニフェストへ集約**: 同じページを 2 本の PR が触れば同じ行で衝突するので、
  衝突の回数は変わらない。prose ファイルの中で衝突しなくなる分だけ安全にはなる。
- **merge 後に CI がハッシュを push**: その瞬間ハッシュは「誰かが見た」ではなく
  「merge された」の記録に退化する。実質は下の決定と同じところに着く。

## Decision

**`sourceHash` を廃止し、鮮度を PR 差分の規則にする。**

1. ja ページの frontmatter から `sourceHash` を削除（32 枚）。スキーマ
   （`docs-site/src/content.config.ts`）と `--accept` モード、`i18n:accept` も削除。
2. 新しい規則: **英語ページを変えた PR は、その ja ページも同じ PR で変える。**
   `scripts/ci/i18n-pair-guard.sh` が差分から判定し、`docs-guard.yml` の
   required job `user-visible surface documented` の中で走る。
3. 例外は PR 本文の `translation-not-needed: <reason>` 1 行。本文の編集で
   チェックが再実行されるので、例外に CI サイクルは要らない。
4. `i18n:check` は木についての 2 問だけを残す——ja ページが在るか（`missing`）、
   対の形が一致するか（`drifted`、#678 / #1011）。形の比較は**全ペアで無条件**に
   なる（旧来はハッシュ一致が前提条件だった）。
5. `PageTitle.astro` の「英語版より古い可能性があります」バナーと、そこにあった
   digest の**重複実装**を削除。

## Consequences

- 同じ英語ページを触る 2 本の PR は、同じ段落を編集しない限り衝突しなくなる。
  実測の 35 対／30 日のうち大半が消える。1 回あたり force-push 1 回とフル CI 1 周。
- **過去の全履歴でこの規則を再生した結果**: 機構導入（2026-07-24, #147）以降、
  英語ページを触った 198 commit・405 ページ対のうち、**ja を全く触らなかったものは 0 件**、
  **ja をハッシュ行だけ書き換えたものが 6 件**（#478 / #1000 / #1012、いずれも
  用語統一で英語の言い回しだけが変わり ja は既に正しかったケース）。この 6 件が
  `translation-not-needed:` を必要とした唯一の集合で、他に落ちるものは無い。
- **失うもの 1**: ja ページ上のバナー。ただし `deploy-docs.yml` は同じ job 内で
  `i18n:check` を build の**前**に走らせるため、古いページは本番に到達し得ず、
  このバナーは #147 で追加されて以来 docs.waired.ai で一度も表示されていない
  （出るのは手元の `npm run dev` のみ）。
- **失うもの 2**: 木を走査して「古い対」を後から見つける能力。入口（PR）で塞ぐ設計に
  変わったので、squash 専用・PR 必須のこのリポジトリでは main に古い対が溜まらない。
  ただしガードが required でなくなれば穴が開く——`docs-guard.yml` の既存 job に
  相乗りしているのはそのため。チェック名（`user-visible surface documented`）は
  2 つの規則のうち 1 つしか名乗っていない。required context を分けるのは
  ruleset の編集（オーナー権限）が要るので、そこは後日の整理として残す。
- **得るもの**: 形の比較が無条件になる。旧来は `stale` の間は黙り、しかも
  `--accept` は `classify` が `stale` を返す経路で先にハッシュを書いてしまうため、
  「drifted なら拒否する」保護は*既に同期していた対*にしか効いていなかった
  （実機で確認: 見出しを 1 つ落とした ja + 動いた en に対し `--accept` は
  `accepted: ja/faq.md` と書き、`Drifted` はその次の `--check` で初めて出た）。
- `--accept` が無くなるので、「英語だけ見て ja を読まずにハッシュを更新する」経路が
  消える。新しい規則は ja ファイルの実質的な変更を要求するので、記録としては強い。

## Refs
- waired-agent#1215（この決定）、#578（旧手順の記録）、#678 / #741（形の比較）、
  #1010 / #1011 / #1012（部品の数え上げ）、#147（機構の導入）
- `docs-site/TRANSLATION.md` §Writing the pair
- `scripts/ci/i18n-pair-guard.sh`、`docs-site/scripts/i18n-sync.mjs`
