---
status: accepted
---

# .deb のプレリリースは `~` で綴る。既に公開した 0.0.2 系は捨て、次を 0.0.3 系にする (20260815 02:45)

## Status

Accepted

## Context

`.github/workflows/reusable-build-artifacts.yml` のヘッダコメントは
「SemVer の `-` を Debian の `~` に書き換えるのはここだ」と宣言していたが、実装は
edge 分岐にしかなく、tag 分岐は `debver="${v}"` の素通しだった (waired-agent#780)。
その結果、`v0.0.1-rc2` から `v0.0.2-rc9` までの **25 版がすべてハイフン形**で
公開 APT リポジトリ `waired-dev-apt` に載っている。

dpkg は版を**最後のハイフン**で upstream と revision に分割する。`0.0.2-rc9` は
upstream=`0.0.2` / revision=`rc9`、`0.0.2-rc8-dev` は upstream=`0.0.2-rc8` /
revision=`dev` になり、順序が壊れる。実測 (`dpkg --compare-versions`):

```
0.0.2-rc9  >  0.0.2          rc が正式版を上回る (rc1 から構造的に存在)
0.0.2-rc9  <  0.0.2-rc8-dev  2 ハイフンのタグが次の rc を追い越す
0.0.2~rc9  <  0.0.2          `~` なら正しく並ぶ
```

害は「更新できない」ではなく「黙って下がる」だった。rc9 が入った実機
(Ubuntu 26.04) での `apt-get install -s --only-upgrade` の出力、逐語:

```
2 upgraded, 0 newly installed, 0 to remove and 2 not upgraded.
Inst waired [0.0.2-rc9] (0.0.2-rc8-dev waired-dev-apt:waired-dev-apt [amd64])
```

apt の順序では昇順なので、素の `sudo apt upgrade` が警告も `--allow-downgrades`
も無しに rc9 を rc8-dev へ落とす。ダウングレードですらなく正規の upgrade である。

問題は、`~` の書き換えを入れるだけでは**状況が悪化する**ことにある。`~` は
空文字より前に並ぶので `0.0.2~rc10` は既存の `0.0.2-rcN` すべてより下に沈み、
candidate は `0.0.2-rc8-dev` のまま動かない。まっさらな新規ホストの `install.sh`
すら rc8-dev を掴む。既に公開してしまった 25 版をどう扱うかの判断が要る。

## Decision

1. **tag 経路でもタグの全ハイフンを `~` に書き換える。** 最初の 1 個だけでなく
   全部にするのは、dpkg の「最後のハイフンで分割」というルールに一切依存させない
   ため。多ハイフンのタグでも壊れない。
2. **既に公開した 0.0.2 系 25 版は AR に残置する。** 削除も epoch も導入しない。
3. **次のプレリリースを `v0.0.3-rc1` とし、0.0.2 系は捨てる** (0.0.2 GA は出さない)。

3 が 2 を成立させる。実測:

```
0.0.3~rc1  >  0.0.2-rc8-dev   既存の最上位を上回る = candidate が正しく更新される
0.0.3~rc1  >  0.0.2-rc9       既存ホストが素の apt upgrade で上がれる
0.0.3      >  0.0.3~rc1       GA も正しく並ぶ
```

### 採らなかった案

- **epoch (`1:0.0.2~rc10`)** — Debian が版付けの誤りのために用意した正式な機構で、
  削除も版番号の変更も要らない。採らなかったのは epoch を**二度と外せない**ため
  (外すと再び逆転する)。以後 apt 上の版表示は永久に `1:` 付きになり、GA も
  `1:1.0.0` になる。加えて `WAIRED_VERSION` の pin も `1:0.0.2~rc10` の翻訳が要る。
  1.0 の手前で恒久的な代償を負う案である。
- **AR から 0.0.2 系を削除** — 公開リポジトリからの不可逆な削除というコストを
  払ってもなお、既存 rc9 ホストは `--allow-downgrades` が要り、素の `apt upgrade`
  しかしないホストは rc9 のまま取り残される穴が残る。
- **`~` を入れない (現状維持)** — rc が永久に stable より上に立ち、`v0.0.2` を
  切っても apt ホストは上がれない。上記の「黙って下がる」も残る。

## Consequences

- **タグを切る前にこの変更が main に入っている必要がある。** 逆で入れると
  `0.0.3-rc1` のまま publish され、次の版でまた同じ判断をすることになる。
- 0.0.2 系のハイフン形 25 版は apt に残るが、0.0.3 系が全部その上に立つので
  無害化される。rc の連番は rc9 で切れ、0.0.3-rc1 から数え直す。
- 版の綴りが 2 通りになる。Go のビルドとリリースタグは SemVer の `-`
  (`0.0.3-rc1`)、`.deb` の Version は Debian の `~` (`0.0.3~rc1`)。Linux の
  更新経路はこの 2 つを突き合わせるので、比較器は両方を同じ値に正規化する
  (`internal/version`、`install.sh` の `version_lt`、`install.ps1` の
  `Compare-WairedVersion`)。順序規則も dpkg のものに揃えた — 詳細と、SemVer §11
  の字句順を採らなかった理由は `internal/version/dotted.go` の `comparePre`。
- `WAIRED_VERSION` の pin は apt の綴りへ翻訳される。利用者はリリース名の
  ままで書ける。ただし**翻訳は無条件に適用できない** — 0.0.2 系を残置した
  結果、1 つの suite に 2 通りの綴りが同居する。無条件に翻訳したところ
  公開済み 25 版が pin 不能になった (waired-agent#811)。pin はインデックスの
  実際の中身に対して解決する (`apt_version_pin` / `apt_has_version`)。
- 版解決は `scripts/ci/resolve-build-version.sh` に切り出し、自己テストを付けた。
  ワークフローの inline shell はテストで実行できず、それが「ヘッダコメントで
  宣言だけして tag 経路に実装が無い」を 9 リリース許した機構そのものである。

## この決定はまだ実機で確認されていない

**`v0.0.3-rc1` を切る人へ。** 上の順序はすべて `dpkg --compare-versions` に
対して固定してあるが (`internal/version/dpkg_compat_test.go`、
`scripts/ci/resolve-build-version-test.sh`)、**固定したのは順序であって、
apt が実際にその順序で動くところまでは確認していない。** その確認は公開済みの
`0.0.3~rc1` を前提とするので、タグを切るまで実行できない。

タグ後、stable suite (`waired-dev-apt`) を向いた Linux で:

```sh
sudo apt-get update
apt policy waired                            # Candidate が 0.0.3~rc1 になるか
apt-get install -s --only-upgrade waired     # 前へ進む計画を出すか (simulate)
waired update --check                        # Latest が一致し、先頭の v が無いか
```

修正前の同じ形の出力が waired-ai/waired-agent#780 のコメントにあり、
そこでは `Inst waired [0.0.2-rc9] (0.0.2-rc8-dev ...)` と**後退する計画**を
出していた。前へ進めばこの決定は閉じる。

## Refs

- waired-ai/waired-agent#780, waired-ai/waired-agent#781,
  waired-ai/waired-agent#811 (pin の翻訳を無条件にしたことによる退行)
- waired-ai/waired#1213 (tracking), waired-ai/waired#1217 (実機検証の記録)
- `.github/workflows/reusable-build-artifacts.yml`, `scripts/ci/resolve-build-version.sh`
- `internal/version/dotted.go`
