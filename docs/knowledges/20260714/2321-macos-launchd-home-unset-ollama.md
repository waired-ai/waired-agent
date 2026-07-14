# macOS system LaunchDaemon は $HOME を持たない → spawn した ollama が起動失敗 (20260714 23:21)

## Issue

nightly `installtest-inference.yml` の macOS 両レグが恒常的に赤かった (#22)。
waired-agent が :9475 に spawn する `ollama serve` が起動中に `exit status 1` で
即死し、bundled model が ready にならず下流の inference assert が全滅していた。
症状だけ見ると Metal / GPU / OOM を疑いたくなるが、真因は別だった。

## Learnings

- **macOS の system LaunchDaemon (`/Library/LaunchDaemons`, root) は最小環境で
  起動し `$HOME` を持たない。** その環境から子プロセスを spawn すると子も `$HOME`
  無し。`ollama serve` は `OLLAMA_MODELS` でモデル blob を待避していても
  `~/.ollama`（鍵・config）を解決しようとするため、`os.UserHomeDir()` が
  `Error: $HOME is not defined` を返して**ポートを bind する前に死ぬ**。
  → Linux(systemd) は `$HOME` があるので無事。これが「macOS だけ落ちる」理由。
- 対策: agent は spawn する `ollama serve` の env に、launcher が `HOME` を
  与えていない（未設定/空）ときだけ writable な agent 所有ディレクトリ
  (`<stateDir>/runtimes/ollama`) を `HOME` として注入する
  (`OllamaConfig.StateHome` / `processEnv`)。HOME がある環境では no-op、Windows は
  `%USERPROFILE%` を使うので無害。**CPU 強制や backend 変更は不要**。
- **診断の教訓**: `ollama serve` の stdout+stderr は既に engine.log に捕捉されて
  いたが、mgmt API の `last_error` は Go の exit-code ラップ (`"...: exit status 1"`)
  だけで真因が出ず「opaque」だった。crash 分岐のエラーに engine.log の末尾を
  折り込む (`startupExitError` / `tailEngineLog`) だけで、harness が失敗時に既に
  出している mgmt-JSON からそのまま原因が読めるようになった。launchd 下で
  `$HOME` やその他 env に依存する他の spawn 系（codeui 等）も同様の注意が要る。

## Refs

- https://github.com/waired-ai/waired-agent/pull/24
- https://github.com/waired-ai/waired-agent/issues/22
- `internal/runtime/ollama.go` (`OllamaConfig.StateHome` / `processEnv`)
- `cmd/waired-agent/inference.go` (StateHome 配線)
