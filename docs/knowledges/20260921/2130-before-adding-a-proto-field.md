# proto にフィールドを足す前に確かめること (20260921 21:30)

## Issue

`proto/` にフィールドを足す、または `proto/` の中でコードを移すときに、
着手の前に確かめることの一覧。規則の正本は CLAUDE.md §Modules と
`docs/decisions/20260719/0000-concurrent-proto-development.md` に在る。
ここに置くのは、その規則の下で実際に踏んだ罠と、2 本のガードの照合の仕方。

出自: セッションのメモリに在った知見を、オーナーの指示
(2026-09-21、waired-ai/waired-ai#12)でリポジトリへ移した。

## Learnings

### 1. そのフィールドは署名済み NetworkMap の PEER エントリに乗るか

いつ読むか: agent が報告する構造体にフィールドを足すとき。

capability 定数が要るかどうかは、フィールドの**位置**で決まる。意味の細かさでは
決まらない。waired-agent#69 のフィールド表に「既存フィールドの近くのスカラー
だから capability は不要」と書いて誤ったことがある。

- `signer.HardwareSummary` のように、agent が報告して全 PEER エントリに乗る
  構造体に足すなら、`proto/signer` の capability 定数が要る(CLAUDE.md §Modules、
  上の決定記録の §5)。フィールドを知らない agent は canonical re-marshal で
  キーを落とし、署名検証に失敗する。理由は `proto/signer/capability.go` の
  各定数のコメントに在る。
- CP 側の追随は proto の版を上げるだけでは終わらない。capability を宣言して
  いない poller に配るマップの**全体**から、そのフィールドを落とす処理が要る。
  Self のエントリだけでなく、ほかの PEER のエントリも対象になる。CP 側の PR
  本文にこの点を明記する。
- 落とす処理を書くときの注意(一般形。CP の実装の場所は内部の記録を参照):
  - 保存している値を in-place で書き換えない。Go では struct をコピーしても
    スライスは backing array を共有する。スライス要素のフィールド
    (`signer.HardwareGPUSummary.VRAMFreeMB` の形)を落とすならスライスもコピーし、
    配った値と保存した値で要素のアドレスが違うこと
    (`&served.GPUs[0] != &stored.GPUs[0]`)をテストする。
  - 「落とすものが無ければ何もしない」の早期 return を持つなら、値をゼロにする
    側と、その述語の**両方**に新しいフィールドを入れる。述語に入れ忘れると、
    上書きの無いホスト(大半)でフィールドが素通りする(waired-ai/waired#1250)。
    述語の項を外すと落ちるテストを置く。
  - フィールドを明示的に列挙して比べる等値関数は、足し忘れてもコンパイルが通る。
- 管理コンソールがその値を書く面にも、対象のデバイスが capability を宣言して
  いるかのゲートが要る。設計の注意と、ゲートが撤去された例
  (waired-ai/waired#1302)は内部の記録を参照。

### 2. 出す順序を決める制約

いつ読むか: proto・CP・agent の PR をどの順で出すかを決めるとき。

- CP は、capability を宣言していない poller にそのフィールドを配らない。agent は、
  構造体がフィールドを持って初めて re-marshal でそれを保てる。この順を崩すと、
  旧い agent の署名検証が落ちる。
- CP がそのフィールドを知る版に上がるまでは、agent が送った値は CP を往復する
  間に消える。producer を CP の追随より先に出しても、値は届かない(理由の詳細は
  内部の記録を参照)。
- proto の PR は挙動を変えない形で作れる。producer が居ない間は
  「0 = 不明 → 従来の値」と読まれるようにしておく。producer の負債は
  `scripts/ci/protoconsumer/exemptions.go` の `producerPending` に登録し、
  producer の PR がその行を削除する。

したがって順序は proto(単独の小さな PR)→ タグ → CP の追随 → agent の producer。

### 3. `protoconsumer` はフィールド名で producer を突き合わせる

いつ読むか: 新しい公開フィールドの名前を決めるとき、`producerPending` に
登録するとき。

`scripts/ci/protoconsumer` は「この名前のフィールドに書く非テストのコードが
`cmd/` か `internal/` に在るか」を構文だけで見る。型は見ない。これは `main.go` の
doc コメントに書かれた設計判断で、欠陥として起票するものではない。

- 既存の同名フィールドに書き手が居ると、新しいフィールドは「producer 有り」と
  判定され、負債が表に現れない。足す前に `grep -rn "<Name>" cmd internal` を打ち、
  書き手が居たら名前を変える。`IdleTimeout` ではなく `ResidencyIdleTimeout`、
  `EngineVersion` ではなく `ServingEngineVersion` としたのはこの理由。経緯は
  `exemptions.go` の `producerPending` の上のコメントに在る。
- 登録する**前に** `go run ./scripts/ci/protoconsumer` を走らせ、そのフィールドで
  落ちることを確かめる。落ちなければ、名前がどこかの書き手と衝突している。
- 入れ子の struct の `ModelID` のようにありふれた綴りは、中身が空でも緑になる。
  そこはテストで担保し、PR 本文にそう書く。

### 4. `proto-additive-guard` は書かれたとおりの綴りを比べる

いつ読むか: `proto/` の中で const や func を移す、書き換えるとき。

`scripts/ci/proto-additive-guard.sh` は直前の `proto/v*` タグの `proto/` を取り出し、
`scripts/ci/protoguard` で作業ツリーと比べる。比較は `types.ExprString` の文字列。

- **const は値ではなく式の綴りで比べる。** `KVFactorF16 = 1.0` を
  `= hostfit.VLLMKVFactorF16` に書き換えると、数値が同じでも
  「const value changed」になる。移設先にもリテラルを置き、両方を残して
  テストで結ぶ(`docs/decisions/20260828/1730-vllm-sizing-moves-into-hostfit.md` §2、
  `TestVLLMConstsMatchHostfit`)。
- **func は型の並びで比べる。** 現在の `protoguard` は `unnamedParams` で
  パラメータ名と戻り値の名前を外してから比べるので、`_` を名前に変えるだけなら
  通る(`TestParameterRename_Passes`)。型・順序・可変長の変更は落ちる。
  型を変えたいときは、新しいエントリポイントを足して旧いほうから呼ぶ。
  #1383 より前は名前も署名の一部として比べていて、使っていなかった引数を
  読み始めるだけで落ちた。
- 未公開のシンボルは追跡されない。
- 公開済みの struct に足すフィールドは `omitempty` か `json:"-"`。
  タグ無しは弾かれる。

### 5. 本番の書き手がいないフィールドを公開しない

いつ読むか: 「念のため」の設定フィールドを proto に足したくなったとき。

公開した proto は後から削れない(additive-only、上の決定記録の §3)。
本番の書き手がゼロのフィールドは載せない。`PickInput.NoSpeedFloor` を足さなかった
判断が
`docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md` §4
に在る。

逆向きの注意: 既に在るそういうフィールドを消すと、それを道具にしていたテストが
壊れる。不変条件を言い換えるなら、切り替える前のコードでその不変条件が成立する
ことを実測してから書く。

### 6. 1 本のブランチから proto だけの PR を切り出す

いつ読むか: proto の変更と agent の変更を 1 本のブランチで作り込んだあとで、
proto を先に出すことになったとき。

手順(waired-agent#1337 / #1347 の proto を #1378 として切り出したときのもの):

```sh
git worktree add <abs-repo>/.claude/worktrees/<topic>-proto -b <branch> origin/main
git checkout <full-branch> -- proto scripts/ci/protoconsumer/exemptions.go \
    docs/reference/models.md <root の依存ファイル>
go build ./... && go test ./...
```

切り出した直後に落ちたもの:

- **`protoconsumer`**: 名前の一致する書き手が agent 側の PR にしか無いフィールドは、
  proto の PR で `producedInProto` に一時的に宣言し、agent 側の PR で削除する
  (#1378 では 4 件)。
- **main の agent コードのテスト**: 較正値(×3.0)を前提にした pin が、新しい
  proto で落ちた。agent の判定ロジックには触らず、フィクスチャ(重み 23.7 → 28 GB、
  `size_vram`)だけを動かした。
- **付随ファイルの取りこぼし**: `internal/catalog` の alias、scoring のテスト、
  hardware の `GPUVendor`、router / setup の pin、`docs/reference/models.md` の
  再生成(`make catalog-docs`)。`go build ./... && go test ./...` を proto の PR の
  ツリーで通してから push する。
- **紛れ込んだファイル**: `scp` の宛先の書き損じで、リポジトリ直下にスクリプトが
  1 本落ちて WIP に混ざっていた。`git diff --stat origin/main..HEAD` で
  ファイルの一覧を見てから push する。

最初から proto の PR のツリーで作らなかった場合は、切り出したあとにガード 3 本
(`go run ./scripts/ci/protoconsumer`、`bash scripts/ci/proto-additive-guard.sh`、
`bash scripts/ci/testnet-gate-guard.sh`)と全テストを回す。

## Refs

- CLAUDE.md §Modules
- docs/decisions/20260719/0000-concurrent-proto-development.md
- docs/decisions/20260828/1730-vllm-sizing-moves-into-hostfit.md
- docs/decisions/20260804/1937-capacity-computation-and-window-recommendation.md
- docs/knowledges/20260828/2117-exemptions-key-at-the-site.md
- proto/signer/capability.go
- scripts/ci/protoconsumer/main.go, scripts/ci/protoconsumer/exemptions.go
- scripts/ci/protoguard/main.go, scripts/ci/proto-additive-guard.sh
- https://github.com/waired-ai/waired-agent/issues/69
- https://github.com/waired-ai/waired-agent/pull/1378
- https://github.com/waired-ai/waired-agent/pull/1383
- waired-ai/waired#1250, waired-ai/waired#1302
