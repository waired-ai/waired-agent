---
status: accepted
---

# setup 報告は起きたことを言う (20260821 14:20)

## Status

Accepted。

## Context

rc9 の 3 OS 実機検証 (waired-ai/waired#1213) で、`waired init` の
setup 進捗報告に 3 つの穴が見つかった (#790 / #791 / #797)。
いずれも同じ型で、**報告が実態より良く見える**。

- **driver が消える (#790)**。`setup_progress.driver` には 2 つの腕があり、
  耐久性が違った。`browser` は CP の desired state から毎 push 導出される
  派生値で、desired 列は消えないので事実上恒久。`terminal` は executor
  リースに載る申告で、`waired init` の終了とともに落ちる。
  #667/#771 以前はそれが見えなかった — init 終了後デーモンは push 自体を
  止めていたので、CP に最後の文書が残り、値は恒久に見えていた。
  `observedSetup` が入って毎 tick 全投影を押すようになり、
  `Driver: ""` が保存済みの値を上書きした (setup_progress 列は毎回丸ごと
  置換される)。
- **失敗した integration 行が消える (#791)**。CLI は失敗を報告せず、
  デーモン側も「指示があるか、成功が永続化されているか」でしか行を開かない。
  失敗はそのどちらでもないので、ステップはどの状態にも存在しなかった。
  CP の完了規則は *報告された* ステップだけを見るので、
  オペレータが失敗を目で見た直後に `setup_complete=true` になっていた。
- **ブラウザの無い run にブラウザの話をする (#797)**。`--auth-key` は
  それ自体がサインインで、run のどこにもブラウザセッションは無い。
  それでも「Setup is continuing in your browser…」を出し、そのウィンドウを
  開けておくよう警告し、乗っ取りを持ちかけ、3 分の grace を待ち切っていた。

## Decision

### 1. `Driver` は「この計算機をセットアップした面」

優先順位は、生きたリースの申告 → desired state からの `browser` →
`observedSetup` からの `terminal`。2 つの派生は構造上排他で、
`observedSetup` は desired state が無いときにしか走らない。
つまり「指示が無いのに自分を説明できるホスト」は、
`waired init` が設定したホストである — init は自分の答えをこのデーモンにしか
書かないため。

**state dir にレコードを持たせる案は採らない。** 理由は 2 つで、
どちらも単独で決定的:

- レコードを書けるのは *これから* 走る `waired init` だけなので、
  既に導入済みのホストは更新しても直らない。派生なら更新だけで直る。
- レコードが読まれるためには「記憶した driver だけで push を正当化する」
  必要があり、それは端末導入後にエンジンを外したホストに
  *ステップ 0 個 + `driver: terminal`* の文書を押させる。
  それはウィザードを「このコンピュータを待っています」のカード
  (waired-ai/waired-agent#198) に固定する組み合わせで、
  報告することが残っていないマシンにはそこから出る道が無い。

派生した driver は単独では push を正当化しない (`snapshot` は nil を返し続ける)。

帰結として明示しておく: 一度でも desired state を持ったホストでは
`browser` が恒久的に `terminal` に優先する。後から `waired init` を回しても
`terminal` が出るのはリースが生きている間だけ。
またローカル AI オフ / エンジン無しのホストは `observedSetup` が偽なので
driver は空のまま。

### 2. 端末の integration 失敗は `failed` を出し、記録は書かない

CLI は失敗時に行を `failed` で報告する。デーモンは executor が phase を
報告した行を常に出す (指示や永続レコードが無くても)。
**永続化はしない** — 批准元は
`docs/decisions/20260802/1757-setup-integration-persisted-front-loaded.md`
(失敗を記録すると `waired link` で直した後も赤が残る)。これは supersede では
なく、その決定を守るための実装。

帰結: デーモン再起動で赤は消える。これは契約ではなく今日の挙動で、
テストにもそう書いてある。復旧経路 (決定 3) がその窓を埋める。

CLI が名乗るコードは `timeout` だけ。context のデッドラインは型であって
語ではなく、`classifyIntegrationFailure` に届く頃には文字列なので
`errors.Is` が使えるのはこちら側だけ。permission denied はデーモンの規則の
ままにする — 2 実装は必ず食い違う。

### 3. `waired link --force all` の成功はデーモンに報告する。リースは張らない

報告は `step_only` の POST 1 本 (`attached:false`)。
**リースを再利用しない**理由は 3 つあり、いずれも進行中の `waired init` を
壊す:

- `attachSetupExecutor` の最初の post は `Step:""` で、この protocol では
  `engine_install` を意味する。attach しただけでその行の `errText`/`errCode`
  が消え、他の executor が報告した本物の失敗が別の理由に化ける。
- `Attached:true` は `executorElevated` を無条件で上書きする。この値は
  リースより長生きするよう意図されていて (`engine_install` が
  `permission_denied` を報告できるように)、非昇格の `link` が触ると
  昇格 init が正しくエンジンを入れたホストでその行が権限エラーになる。
- `Release()` の `Attached:false` は `installClaimed` を落とす。それは
  二重の昇格インストールを止めているガードそのもの。

`attached:false` を運ぶのは、このフィールドを知らない旧デーモンに
「release」として読ませるため。ステップ記録と永続化のエッジはリース分岐の
外にあるので、旧デーモンにも有用な半分は届く。`attached:true` を運ぶ案は
旧デーモンで 45 秒の幽霊リース + `executorElevated=false` になり厳密に悪い。

報告するのは `link all` で **全アダプタが `Applied`** のときだけ。
レコードは丸ごと置換されるので単一ターゲットの報告は既存レコードを縮めるし、
`--force` 無しの `link all` は未検出のエージェントをエラー無しで `Skipped`
にするため「終了コード 0」は「両方書いた」ではない。
失敗は報告しない (効く場面では行は既に赤く、効かない場面では健全なホストを
赤くする経路を作るだけ)。`unlink` も報告しない。

失敗しても無言。修復コマンドの仕事はファイルであって、デーモンが止まって
いても旧版でも Windows のネットワークログオンからでも、修復が成った事実は
変わらない。

### 4. 同意プロンプトと apply は別予算

`runPostLoginIntegration` の 60 秒はエージェント検出と `[Y/n]` の質問だけを
覆う。apply は同意が返ってから張り直した context で走り、長さはウィザードと
同じ `setupIntegrationBudget` (3 分)。
1 つの予算が質問と子プロセスの両方を覆っていたので、オペレータが質問を読んで
いた秒数が `waired link all` の持ち時間から引かれ、runuser の NSS 参照が
冷えていると「context deadline exceeded」で終わっていた。

### 5. auth key の run は最初から端末駆動

`--auth-key` は `--non-interactive` / `--no-browser` と同じ枝に入る。
ただし resume 経路 (`waired init` を登録済みデバイスで再実行) は除く —
そこでは鍵は使われず、run はウィザード駆動になり得るので、
`terminal` を名乗ると生きたブラウザセッションの上に端末を報告してしまう。

文言を消すだけでなく窓も閉じる: grace の間はどの面も run を主張していないので、
古い指示を持つデバイスは `browser` と導出されていた (#645 の推測そのもの)。

## Consequences

- `proto/signer/setup_progress.go` の `Driver` の doc コメント
  (「the surface currently driving setup」) が実態と合わなくなる。
  **この PR では触らない** — `proto/**` の差分は
  `.github/workflows/proto-tag.yml` が main マージ時にタグを 1 本切るので、
  コメントだけの変更にタグを消費しない。次の proto PR に相乗りさせる。
- `management.SetupExecutorRequest` に `StepOnly` が増えた。これは
  agent ローカルの IPC であって proto の wire ではないので、
  proto の追加規則もタグも関係しない。`omitempty` なので既存の post は
  バイト同一。
- 反転した既存テスト 3 本 (PR 本文に記載): 失敗 apply が「何も報告しない」→
  `failed` を報告する / 観測ホストの driver が `""` → `terminal` (2 か所)。

## Refs

- waired-ai/waired-agent#790 / #791 / #797
- waired-ai/waired#1227 (L57), waired-ai/waired#1213
- `docs/decisions/20260802/1757-setup-integration-persisted-front-loaded.md`
- `docs/decisions/20260805/1721-executor-lease-is-not-a-wizard.md`
