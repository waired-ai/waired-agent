package claudemanaged

import (
	"os"
	"path/filepath"
	"testing"
)

// waired-agent#1308: the SessionStart command was renamed
// `waired claude _models-cache write` -> `waired claude _picker write` in
// waired-agent#1185 / #1241, and nothing was left that recognised the old
// spelling. On a host enabled before that rename, `waired claude disable` —
// and so `uninstall.sh --clean`, which runs it — left the entry behind, and a
// re-enable stacked a second entry beside it instead of replacing it. The
// stale command names a subcommand that no longer exists, so it fails at every
// Claude Code session start.
//
// PIN: product contract — a disable removes every SessionStart command waired
// has ever written (waired-ai/waired-agent#1308). The retired Stop hook,
// fallbackHookMarker, is the same shape and the precedent for it.
//
// preRenameRefreshCommand is the string measured on a real Ubuntu host running
// 0.0.3-rc6 after `uninstall.sh --clean` (evidence table K2 of
// waired-ai/waired#1353).
const preRenameRefreshCommand = "command -v waired >/dev/null 2>&1 && " +
	"waired claude _models-cache write --from-managed --peer-entries 5 || true"

func retiredRefreshObject() map[string]any {
	return map[string]any{
		"hooks": map[string]any{
			sessionStartHookEvent: []any{
				map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": preRenameRefreshCommand, "timeout": 5.0,
				}}},
			},
		},
	}
}

func seedSettings(t *testing.T, path string, obj map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeJSON(t, path, obj)
}

func TestRetiredRefreshHookMarkerIsRecognised(t *testing.T) {
	t.Run("disable strips a pre-rename entry", func(t *testing.T) {
		obj := retiredRefreshObject()
		if !removeRefreshHook(obj) {
			t.Fatal("remove reported nothing removed — the old spelling is invisible to it")
		}
		if _, ok := obj["hooks"]; ok {
			t.Errorf("the pre-rename entry survived: %+v", obj)
		}
	})

	t.Run("enable replaces it rather than stacking a second", func(t *testing.T) {
		obj := retiredRefreshObject()
		ensureRefreshHook("linux", obj, 5)
		entries := hookEvent(t, obj, sessionStartHookEvent)
		if len(entries) != 1 {
			t.Fatalf("SessionStart entries = %d, want 1 — the pre-rename entry must be replaced, not joined", len(entries))
		}
		if cmd := entryCommand(entries[0], refreshHookMarker); cmd == "" {
			t.Errorf("the surviving entry is not today's command: %+v", entries[0])
		}
	})

	t.Run("an operator's own entry beside it survives", func(t *testing.T) {
		obj := seedHooks(t, `{"hooks":{"SessionStart":[
			{"hooks":[{"type":"command","command":"my-own-thing"}]},
			{"hooks":[{"type":"command","command":"`+preRenameRefreshCommand+`"}]}]}}`)
		if !removeRefreshHook(obj) {
			t.Fatal("remove reported nothing removed")
		}
		entries := hookEvent(t, obj, sessionStartHookEvent)
		if len(entries) != 1 {
			t.Fatalf("entries = %d, want the operator's alone", len(entries))
		}
		if entryCommand(entries[0], "my-own-thing") == "" {
			t.Errorf("the operator's entry is gone: %+v", entries[0])
		}
	})

	// `waired claude status` has to be able to say "installed, but not in the
	// form this computer runs" about one of these.
	t.Run("status sees it and reports it as not runnable here", func(t *testing.T) {
		path := withTempPath(t)
		seedSettings(t, path, retiredRefreshObject())
		cmd := RefreshHookCommandAt(path)
		if cmd == "" {
			t.Fatal("status cannot see the pre-rename hook at all")
		}
		if RefreshHookRunsOn("linux", cmd) {
			t.Errorf("the pre-rename command was reported as runnable: %q", cmd)
		}
	})

	// The whole point: what the uninstall path leaves on disk.
	t.Run("the uninstall path leaves nothing", func(t *testing.T) {
		path := withTempPath(t)
		obj := retiredRefreshObject()
		obj["env"] = map[string]any{baseURLKey: "http://127.0.0.1:9472"}
		seedSettings(t, path, obj)
		removed, err := RemoveWithOptions(RemoveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatal("remove reported nothing removed")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			body, _ := os.ReadFile(path)
			t.Errorf("the file survived with %s", body)
		}
	})
}
