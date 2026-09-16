# Claude Code leftovers corpus

Cases for removing the Claude Code settings Waired writes, shared by every
implementation of those rules (waired-agent#1398):

| Implementation | Runs where | Held to the corpus by |
|---|---|---|
| Go: `claudemanaged.RemoveWithOptions`, the per-user steps of `waired claude disable`, `claudecode.RemoveRetiredCacheOwned` | `waired claude disable` | `scripts/install/claude_leftovers_corpus_test.go` (`unit`, `unit-multios`) |
| PowerShell: `Edit-ClaudeLeftovers` in `packaging/install/uninstall.ps1` | `uninstall.ps1` when `waired.exe` is missing, refused or old | `scripts/dev/claude-leftovers-corpus.ps1`, from `installtest-pwsh.ps1` (pwsh 7, `ci.yml` `installer-pwsh`) and `installtest-windows.ps1` (Windows PowerShell 5.1, `installtest.yml`) |
| Python: `claude_leftovers_py` in `packaging/install/uninstall.sh` | `uninstall.sh` on Linux | `scripts/dev/claude-leftovers-corpus.sh python3`, from `installtest-dash.sh` (`ci.yml` `install-scripts`) |
| JavaScript for Automation: `claude_leftovers_jxa` in `packaging/install/uninstall.sh` | `uninstall.sh` on macOS | `scripts/dev/claude-leftovers-corpus.sh osascript`, from `installtest-macos.sh` (`installtest.yml`) |

The Go side is the reference. The expected files record what the Go removers
do; the script copies follow them. `internal/integration/claudemanaged` and
`internal/integration/claudecode` each have a `leftovers_contract_test.go`
that fails when a literal the Go rules recognise is missing from either script
or, for managed settings, from every case here.

## Layout

```
managed/<NN-name>/        %ProgramFiles%\ClaudeCode\managed-settings.json,
                          /etc/claude-code/..., /Library/Application Support/ClaudeCode/...
user-settings/<NN-name>/  ~/.claude/settings.json
retired-cache/<NN-name>/  ~/.claude/cache/gateway-models.json
```

Each case directory holds `input.json` and exactly one of:

- `expected.json`: the file is rewritten, and parses to this document. Key
  order and formatting are not compared (Go sorts keys; the scripts keep the
  file's order).
- `expected.absent`: the file is removed. For `user-settings`, a file left as
  `{}` counts as removed: some Go removers delete an emptied file and some
  write `{}`, and Claude Code reads the two the same way.
- `expected.unchanged`: the file is left byte for byte as it was.

The files are committed with `-text` (see `/.gitattributes`), so a Windows
checkout keeps their bytes, including the leading BOM in the `utf8-bom` cases.

## Known differences the corpus avoids

These inputs are rare enough that the corpus leaves them out rather than
bending a copy to match. A script that meets one leaves the file unchanged or
keeps the value, which is the cautious direction.

- Field names that differ only by case (`"Command"`, `"Model"`). `encoding/json`
  matches struct fields case-insensitively; the scripts match exactly.
- `null` inside a per-user `env` block. Go reads it as an empty string; the
  scripts treat the block as unreadable.
- Integers beyond 2^53. JavaScript loses their precision when it rewrites a
  file, and so does Go's `map[string]any`.
- A tier marker splitting the word, as in `wai[1m]red`, when the file has no
  other mention of `waired`. The rules match it, but `uninstall.sh` skips files
  that don't mention `waired` or `127.0.0.1` before starting an interpreter.
