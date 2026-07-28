# コンテナには管理 API の書き込みソケットを作る主体がいない (20260728 11:00)

## Issue

testnet の docker エージェント (`a1-docker`) が登録されず、
`discover_agents did not converge within 1200s` で 20 分後に落ちる状態が続いた。
コンテナは 12 秒周期でクラッシュループしており、Cloud Logging の
`waired-bootstrap` には `boot_observed → agent_exec → enroll_start` だけが
繰り返し記録され、`enroll_done` は一度も出ていなかった。

エージェント本体のログは Cloud Logging に 1 行も無い（publisher は enroll 後に
立ち上がるため）。コンテナの stdout は docker の json-file ドライバ止まりで
誰も収集していないため、**失敗理由がどこにも残らなかった**。

## Learnings

**根本原因**: 管理 API の *書き込み* は waired#838 以降ローカル IPC ソケット
（`/run/waired/mgmt.sock`）専用で、ループバック TCP は mutating verb を 403 で
拒否する。そのディレクトリを作るのは systemd unit の
`RuntimeDirectory=waired` だが、**コンテナには systemd が無く、非 root の
`waired` ユーザで動くため `/run` に mkdir できない**。

- daemon は `localipc: create runtime dir /run/waired: permission denied` を
  ログして **fail-open** する（読み取りは TCP で提供し続ける）。
- bootstrap の readiness プローブは `GET /waired/v1/status` = **読み取り**なので
  **通ってしまう**。
- 直後の `waired init` は書き込み（`POST /login/start`）なので、ソケットが無く
  1 秒で失敗する。

つまり「healthy に見えるのに enroll だけできない」状態になる。
`waired init --bypass-mode` を先に走らせていた旧コンテナ経路は daemon を
経由しなかったため、この依存が存在しなかった。

**確認方法（ローカルで再現できる）**:

```sh
# 書けない runtime dir を指定して daemon を起動
WAIRED_MGMT_SOCKET=/run/waired-sim/mgmt.sock waired-agent --state-dir <dir> ...
#   -> "management local IPC socket stopped: ... permission denied" を吐いて起動継続
curl -fsS http://127.0.0.1:9476/waired/v1/status   # 200 が返る（読み取りは生きている）
waired init --auth-key ... --mgmt http://127.0.0.1:9476
#   -> waired management socket (...) not found; is waired-agent running?
```

書き込みソケットが存在する状態に変えると、同じコマンドが CP まで到達して
`auth_key_invalid` を返す（＝経路自体は正常）。

**socket path の決まり方**（`internal/platform/paths/paths_linux.go`）:
state dir が OS 既定（linux では `/var/lib/waired`）と一致すると `System` モード
になり `/run/waired/mgmt.sock`。既定以外の state dir なら instance ソケット
（state dir 直下、長すぎる場合は `$TMPDIR/waired-<hash>/`）。
コンテナは Dockerfile の `ENV WAIRED_STATE_DIR=/var/lib/waired` によって
必ず前者になる。

**教訓**: readiness プローブは「必要な物」を見なければ意味が無い。
`/status` の 200 は「読み取り経路が生きている」ことしか証明しておらず、
その直後に使う書き込み経路については何も言っていなかった。

## Refs
- https://github.com/waired-ai/waired-agent/pull/293
- https://github.com/waired-ai/waired-agent/issues/175
- build/Dockerfile.waired-agent / build/agent-bootstrap.sh
- build/waired-agent.service (`RuntimeDirectory=waired`)
