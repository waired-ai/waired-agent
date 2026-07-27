---
status: accepted
---

# 認証キーは daemon が引き換える資格情報として実装する (20260727 19:00)

## Status
Accepted

## Context

#175 の終点は local enrollment(`routeLocal`)の全廃だが、PR-1 の時点では
明示選択子（`--bypass-mode` / `--google-sa-login` / re-auth）が残っていた。
`--google-sa-login` は installtest 3 OS すべてが使っており、この経路を
消すと**全 PR で回る 3 OS の installtest が止まる**。

control plane 側に認証キー(waired#976)が入り、`POST /v1/auth/login-sessions`
の `auth_key` フィールドで無人 enrollment ができるようになった。

## Decision

認証キーを **daemon 経路の資格情報**として実装する。local enrollment の
新しい選択子にはしない。

1. `chooseEnrollRoute` に `authKey` を fact として足し、**キーがあるときは
   決して `routeLocal` を返さない**。他のどの選択子よりも優先する。
2. `--auth-key` の値は `resolveAuthKey` という純関数で解決する
   （そのままの値 / `file:<path>` / `$WAIRED_AUTH_KEY`、前後の空白は除去）。
3. `LoginStartRequest.AuthKey` を経由して daemon に渡す。このフィールドは
   `internal/management` にあり `proto/` ではないので、proto タグも
   CP 側の依存 bump も不要。`/login/start` は writeGuard により
   ローカル IPC ソケット限定なので、キーが TCP に出ることもない。
4. `RunInit` は作成レスポンスに `registration_ticket` があれば
   `OnLoginURL` も poll ループも飛ばして enroll に直行する。
5. `--google-sa-login` / `--impersonate-sa` / `--oidc-id-token` /
   `--oidc-audience` と `oidc_grant.go` を削除し、installtest 3 OS と
   Hyper-V edge verify を `--auth-key` に移行する。

## Consequences

**得たもの**

- キーを渡した実行が local enrollment に落ちることが構造的に無くなった。
  もし落ちれば資格情報が捨てられ、capability を宣言しないデバイスが
  登録される — #175 が消そうとしている失敗そのものになる。
- installtest が**本物のヘッドレスインストールと同じ経路**を通るように
  なった。これまでは daemon を止めて local 経路を通しており、production の
  journey を検証していなかった。
- 引き換えは 1 リクエストで完結するので、CP の instance 親和な
  ticket 受け渡しマップを経由しない。

**払ったもの / 制約**

- `--bypass-mode` は**まだ消せない**。testnet のコンテナ
  (`build/agent-bootstrap.sh`)が使っており、コンテナの反転には
  waired 側の terraform 配線(waired#982)が先に deploy されている必要がある。
  これは後続 PR に分けた。
- macOS / Windows の `--daemon-engine` レグは認証キーを**使わない**。
  あのレグの目的は「init 実行中に常駐 executor がエンジンを入れる様子」を
  観測することで、認証キーは作成リクエスト内で session を authorize して
  しまうため観測窓が消える。よって従来どおり OIDC grant で out-of-band に
  完了させる。
- 認証キーで参加したデバイスには NAVI の setup window が開かない
  （setup ticket がブラウザの web session に束縛されるため）。
  ヘッドレス向けの意図的な仕様。

## Refs
- https://github.com/waired-ai/waired-agent/issues/175
- https://github.com/waired-ai/waired/issues/976
- PR #271（暗黙の local enrollment フォールバック廃止）
