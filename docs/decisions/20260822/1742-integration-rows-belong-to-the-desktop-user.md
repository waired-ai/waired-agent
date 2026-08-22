---
status: accepted
---

# コーディングツール連携の「見る」も「直す」もデスクトップユーザの面で行う (20260822 17:42)

## Status
Accepted

## Context

tray の「OpenCode integration」「OpenClaw integration」行は、daemon の
`GET /waired/v1/integration/{opencode,openclaw}` が返す検出結果をそのまま
描いていた。daemon はその検出を **自分の** `os.UserHomeDir()` に対して
行っていたが、既定のインストール形態(system service)では daemon は
サービスアカウントで動く — Windows は LocalSystem、Linux は `waired`、
macOS は root — ので、見ていたのは
`C:\WINDOWS\system32\config\systemprofile\.config\opencode\...` や
`/var/lib/waired/.openclaw/...` だった。どのコーディングエージェントも
読まない場所である。

結果、実機(Windows と Linux の service install 2台)では、デスクトップ
ユーザとしてプラグインが在り `waired doctor` が全行 ✓ を出し
`opencode run` が `:9479` 経由で応答している状態で、tray だけが
`○ not configured` と言い続けた(waired-agent#986)。

同じ配線には書き込み口もあった。`POST .../reconfigure` は daemon の中で
`setup.IntegrationOne` を呼ぶので、押せば**サービスアカウントのホーム**に
プラグインを書く。これは waired#935 が明示的に禁じている形でもある
(`cmd/waired/setup_integration.go` 冒頭: 「daemon にやらせると、管理 API に
届く任意のローカルプロセスにとっての権限ブリッジになる」)。加えて
OpenCode 側の「Reconfigure…」項目はクリックハンドラが配線されておらず、
押しても何も起きない行として出荷されていた。

「daemon がコンソールユーザを解決して代わりに覗く/書く」案も検討したが、
採らなかった。Linux の daemon は root ですらない(`User=waired`)ので他人の
home を読めるとは限らず、Windows には降格の仕組みを持ち込まないと
決めてある(docs/decisions/20260726/1805-browser-open-single-entry-drop-to-user.md、
Windows 側のセッション模型は #116 で未決)。そして何より、それは上記の
権限ブリッジそのものである。

## Decision

**daemon は「期待値」だけを答え、観測と修復はデスクトップユーザとして動く面が行う。**

- `GET /waired/v1/integration/{opencode,openclaw}` の本体を
  `{"expected_base_url": "..."}` の 1 項目にする。daemon が単独で知っている
  事実(自分の data-plane gateway が何番で待っているか)だけを返す。
- tray は自分の home に対して `internal/integration/detect` を走らせて行を組む
  (`internal/gui/tray/integration_probe.go`)。tray はデスクトップユーザの
  プロセスであり(`internal/platform/paths`: 「tray と CLI はデスクトップ
  ユーザ、daemon はサービスユーザとして動く」)、プラグインの持ち主と同じ
  ユーザである。home が引けないときは行を**隠す** — 「未設定」と言うのは
  別の嘘になる。
- 「Reconfigure…」は `waired link <target> --force --no-prompt` を**非昇格で**
  実行する。クリップボード代替で提示していたコマンドと同じものを、同じ
  ユーザとして走らせるだけ。OpenCode 側の死んでいたクリック配線もここで
  塞ぐ。
- `POST .../reconfigure` は**撤去**する。daemon がユーザーの home に書く口を
  残さない。

私有側の決定 `waired/docs/decisions/20260506/1730-linux-desktop-tray-tailscale-model.md`
の「tray は MGMT API オンリー」とは矛盾しない。あちらが禁じているのは
**daemon の state(`identity.json`)を直接読むこと**で、ここで読むのは tray
自身のユーザーの設定ファイルである。daemon 由来の値(gateway の URL)は
これまでどおり MGMT API 越しにしか取らない。

## Consequences

- service install(インストーラが既定で作る形)でも行が事実を言うようになる。
  per-user install では以前と同じ答えになる — 同じ home を見るため。
- daemon から `internal/setup` への依存が消えた。統合ファイルを書くのは
  昇格した CLI(ウィザード経路)か、デスクトップユーザの CLI/tray だけになる。
- 版ずれはリリース前につきゲートしない。**訂正(20260822、実機 rc3 のボディを
  マージ済みコードに通して実測)**: 当初ここに「新 tray × 旧 daemon はグループを
  隠す」と書いたが、それが当てはまるのは旧 daemon が**そのエンドポイントを
  持たない**場合だけだった。実際は 2 通りに分かれる。
  - 旧 daemon が 404 を返す面(rc3 の `/integration/opencode`)→ `unsupported`
    センチネル → snapshot が nil → **グループを隠す**。
  - 旧 daemon が**旧ボディで 200 を返す**面(rc3 の `/integration/openclaw` は
    `{"config":{…}}` を返す)→ `getJSON` は未知フィールドを捨てるのでデコードは
    成功し、`ExpectedBaseURL` が**空**になる。tray は自分の home を見て
    `OpenClaw integration: ● configured` と**正しく**描く。失われるのは
    ドリフト検出だけ(空の期待値は「見つかったものはすべて fresh」)。
  つまり旧 daemon 相手でも行は嘘をつかない。旧 tray × 新 daemon は `config` が
  無いので「未設定」相当を描く。どちらも edge 更新で解消する。
- `management.IntegrationExpectation` は OpenCode/OpenClaw 共通の 1 型。
  答えている事実が同種なので、面ごとに型を分ける理由が無くなった。

## Refs
- https://github.com/waired-ai/waired-agent/issues/986
- https://github.com/waired-ai/waired-agent/issues/116 (Windows の昇格セッション模型・未決)
- docs/decisions/20260726/1805-browser-open-single-entry-drop-to-user.md
- waired#935 (ウィザードの統合適用は昇格 CLI が行う), waired#1263 / waired#1265 (発見の経緯)
