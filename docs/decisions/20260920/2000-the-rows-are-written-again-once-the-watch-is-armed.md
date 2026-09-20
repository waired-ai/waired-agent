---
status: accepted
supersedes:
  - docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md
---

# `/model` の行は、監視が張られてからもう一度書く (20260920 20:00)

## Status

Accepted。オーナー裁定（2026-09-20、waired-ai/waired-agent#1454）。

`docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md`
の **`## Consequence — 行が反映されるのは次のセッションから`** の節だけを
置き換える。とくに「3〜6 s の間に監視が張られる。**競合であって契約ではないので、
遅延書込には依存しない。**」を改める。0330 の裁定 1〜7（書き先を `modelPicker` に
すること、id の綴り、`replaceBuiltInOptions` を書かないこと、他人の lineup に
触らないこと）は有効のまま。

## Context

書き先を `modelPicker` に移したとき、Claude Code は settings を起動時に読み、
その数秒後に監視を張ることが分かった。SessionStart hook の書き込みは起動 +1.2 秒に
着地するので監視より前で、**そのセッションには永久に届かない**。0330 はこれを
「行が変わるのは次の `claude` から」という仕様として受け入れた。

その代償が実機で見えた。2026-09-20、Linux・Claude Code 2.1.267・waired 0.0.3-rc6 の
ホストで、`waired claude enable` から 9 日後に `/model` を開いたところ、4 つの
ピア行のうち 2 行が誤ったモデル名、1 行はラベルが古く、1 行はもう serving して
いないピアの行だった。片方は「0.8B のモデル」と書かれていて実際は 125B で、
**選ぶ側の判断材料が逆向きに間違っていた**（waired-ai/waired-agent#1454）。

「メッシュが変わったら 1 回起動し直せば直る」ではない。**1 回目の起動は必ず
古い行を見せ、2 回目でようやく正しくなる。** たまにしか `claude` を使わない人は、
毎回古い行を見ることになる。

さらに 0400 裁定 3 の「`CLAUDE_CONFIG_DIR` を継承できるのはフックだけ」は、
その継承をコードが使っていなかった（waired-ai/waired-agent#1457、本決定とは別の
欠陥として同じ PR で直した）。

## 実測（2026-09-20、Claude Code 2.1.278）

使い捨て HOME・ダミーの `ANTHROPIC_AUTH_TOKEN`・ローカルのスタブ・pty。書き込みは
temp+rename（`secrets.WriteFile` と同じ形）。

内容を変える書き込みを 1 回だけ打ち、十分あとで `/model` を開く:

| 書き込みの時刻 | 同じセッションの `/model` |
|---|---|
| 1.1 s / 3.1 s / 4.1 s | 古いまま（48 s 後でも） |
| 5.1 s / 6.1 s / 8.1 s | 新しい値 |

監視が張られるのは **4〜5 秒の間**。2.1.261 の 3〜6 s（`docs/knowledges/20260906/0340`
§5）と一致する。

**ディスクは既に正しいまま、同じバイト列をもう一度書く**とどうなるか:

| 形 | 結果 |
|---|---|
| 1.3 s に書く（hook 相当・届かない）→ 8.1 s に同一バイトを書き直す | 新しい値が出る（3/3） |
| 同上で、15 s に `/model` を開いて古い行を見た後、20.1 s に同一バイトを書き直す | 新しい値が出る |

**同じ内容でも監視は発火する。** 旧 private cache のようなプロセス内メモ化も無く、
一度描画したあとでも入れ替わる。詳細は
`docs/knowledges/20260920/2010-the-model-picker-on-2-1-278.md`。

## Decision

1. **SessionStart hook が実際に行を書き換えたときだけ、もう一度書く。**
   `cmd/waired/claude_picker_write.go` は書き込みの結果を
   `pickerWriteOutcome{LineupChanged, CacheRemoved}` の 2 ビットで返し、
   `LineupChanged` かつ `--from-managed` のときだけ再公開を予約する。
   退役キャッシュの削除は起動時にしか読まれないので予約しない。昇格した
   `waired claude enable` の経路でも予約しない（見ているセッションが無く、
   root で走っていることがある）。

2. **再公開は切り離した子が行う。** `waired claude _picker republish` を
   `internal/platform/detach` 経由で起動し、`Wait` しない。子は 8 秒後と 20 秒後に
   書き直す。**hook のコマンド文字列と `refreshHookTimeout = 5` は変えない** —
   これが決め手で、今日 hook を持っているホストはバイナリ更新だけで直る（昇格した
   再 enable も、退役マーカーの追加も、`packaging/install/testdata/claude-leftovers/`
   のバイト固定コーパス再生成も要らない）。

3. **再公開はディスクにあるものを書き戻す。** `RepublishPickerLineup(path)` は行を
   引数に取らない。8 秒前に計算した行を書くと、間に `waired logout` /
   `waired claude disable` が消した行を復活させ（waired-agent#1310）、間に別の起動が
   書いた行を巻き戻す。ファイルを読み直すことで、どちらも「起きにくい」ではなく
   「起きない」になる。

4. **best effort であって契約ではない。** 8 秒と 20 秒は今日測った値からの余裕で
   あって保証ではない。外したときは次の起動で行が出る＝この決定の前の挙動。
   Windows で Job Object が子の breakaway を許さない場合も同じところに落ちる。

5. **docs にはこの形で書く。** 「行が変わるのは次の `claude` から」は改める。
   `docs-site/src/content/docs/guides/claude-code/how-turns-are-routed.mdx` は
   "appears in the **next** `claude` you start" と書いていたが、これは修正前の
   挙動としても 1 起動ぶん楽観的だった（実際に出るのはその次）。

## 退けた案

- **hook 自身が待つ。** 行が変わった起動で毎回 7 秒セッション開始が止まる。5 秒の
  timeout に引っかかるので hook 文字列と timeout の両方を変えることになり、
  昇格した再 enable を踏まないと直らない。失敗したときの見え方も「hook の
  タイムアウト」で騒がしい。
- **`WritePickerLineup` に `force` を足す。** 裁定 3 のとおり、覚えた行を書くのが
  そもそも誤り。
- **デーモンに書かせる。** `docs/decisions/20260820/0400` 裁定 1 の所有権の理由は
  今も成立する。Linux のデーモンは `User=waired` で他ユーザーのホームに書けない。
- **tray に書かせる。** ユーザー権限で常駐しており 5 秒ごとにメッシュを読んでいるので
  「開いているセッションにメッシュ変化をその場で反映する」唯一の案だが、ヘッドレス機に
  居ない（#1454 を観測したホストがまさにそれ）。今回は書き手を増やさない。必要になれば
  別の裁定で。

## Refs

- waired-ai/waired-agent#1454、waired-ai/waired-agent#1457
- `docs/decisions/20260906/0330-the-model-rows-are-published-through-modelpicker.md`
- `docs/decisions/20260820/0400-picker-cache-refreshes-on-session-start.md`
- `docs/knowledges/20260906/0340-the-model-picker-measured-again.md` §5
- `docs/knowledges/20260920/2010-the-model-picker-on-2-1-278.md`
