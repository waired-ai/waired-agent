package claudemanaged

import (
	"encoding/json"
	"strings"
	"testing"
)

// The SessionStart hook that keeps the /model picker entries current
// (waired-agent#830).
//
// PIN: product contract — SessionStart specifically, because the picker cache
// is read once per Claude Code process and a SessionStart hook runs before
// that read, so the refresh lands in the same session. Both halves measured on
// a real host: docs/knowledges/20260820/0300-model-picker-measured-on-device.md.

func hookEvent(t *testing.T, obj map[string]any, event string) []any {
	t.Helper()
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	entries, _ := hooks[event].([]any)
	return entries
}

func seedHooks(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestEnsureRefreshHook(t *testing.T) {
	t.Run("installs on SessionStart, carrying the peer cap", func(t *testing.T) {
		obj := map[string]any{}
		ensureRefreshHook("linux", obj, 5)
		entries := hookEvent(t, obj, sessionStartHookEvent)
		if len(entries) != 1 {
			t.Fatalf("SessionStart entries = %d, want 1", len(entries))
		}
		cmd := entryCommand(entries[0], refreshHookMarker)
		if !strings.Contains(cmd, "--peer-entries 5") {
			t.Errorf("command does not carry the cap: %q", cmd)
		}
		// The hook must not have to read a machine-wide agent.json from an
		// unprivileged user session, which is why the cap is baked in.
		if !strings.Contains(cmd, "--from-managed") {
			t.Errorf("command does not read its base URL from managed settings: %q", cmd)
		}
	})

	t.Run("refreshing replaces our entry rather than stacking a second", func(t *testing.T) {
		obj := map[string]any{}
		ensureRefreshHook("linux", obj, 5)
		ensureRefreshHook("linux", obj, 2)
		entries := hookEvent(t, obj, sessionStartHookEvent)
		if len(entries) != 1 {
			t.Fatalf("SessionStart entries = %d, want 1 — the marker must match the older command too", len(entries))
		}
		if cmd := entryCommand(entries[0], refreshHookMarker); !strings.Contains(cmd, "--peer-entries 2") {
			t.Errorf("the cap was not updated: %q", cmd)
		}
	})

	// The reason waired's hooks live in managed settings at all.
	t.Run("an operator's own SessionStart hook survives", func(t *testing.T) {
		obj := seedHooks(t, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"my-own-thing"}]}]}}`)
		ensureRefreshHook("linux", obj, 5)
		entries := hookEvent(t, obj, sessionStartHookEvent)
		if len(entries) != 2 {
			t.Fatalf("entries = %d, want the operator's plus ours", len(entries))
		}
		if removeRefreshHook(obj); len(hookEvent(t, obj, sessionStartHookEvent)) != 1 {
			t.Error("removing ours took the operator's with it")
		}
	})

	t.Run("a foreign Stop hook is untouched", func(t *testing.T) {
		obj := seedHooks(t, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-own-stop"}]}]}}`)
		ensureRefreshHook("linux", obj, 5)
		if len(hookEvent(t, obj, stopHookEvent)) != 1 {
			t.Error("installing the refresh hook disturbed the operator's Stop hook")
		}
		removeRefreshHook(obj)
		if len(hookEvent(t, obj, stopHookEvent)) != 1 {
			t.Error("removing the refresh hook took the operator's Stop hook with it")
		}
	})

	t.Run("removing the last one collapses the object", func(t *testing.T) {
		obj := map[string]any{}
		ensureRefreshHook("linux", obj, 5)
		if !removeRefreshHook(obj) {
			t.Fatal("remove reported nothing removed")
		}
		if _, ok := obj["hooks"]; ok {
			t.Errorf("an emptied hooks object was left behind: %+v", obj)
		}
		if removeRefreshHook(obj) {
			t.Error("removing twice reported a second removal")
		}
	})
}

// waired-agent#787: one command string has to survive whichever shell the OS
// hands a hook to, and Windows has no single answer. The refresh hook must not
// re-acquire the regression the fallback hook already fixed.
func TestRefreshHookCommandPerOS(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		wantGuard bool
	}{
		{"linux", true},
		{"darwin", true},
		{"windows", false},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			obj := map[string]any{}
			ensureRefreshHook(tc.goos, obj, 3)
			cmd := entryCommand(hookEvent(t, obj, sessionStartHookEvent)[0], refreshHookMarker)
			if got := strings.Contains(cmd, posixHookGuard); got != tc.wantGuard {
				t.Errorf("%s command = %q; POSIX guard present = %v, want %v", tc.goos, cmd, got, tc.wantGuard)
			}
			if !RefreshHookRunsOn(tc.goos, cmd) {
				t.Errorf("%s: the command we just wrote does not run on this OS: %q", tc.goos, cmd)
			}
		})
	}
	// And the pre-fix form must still be reported as unrunnable there.
	legacy := posixHookGuard + " " + refreshHookMarker + " || true"
	if RefreshHookRunsOn("windows", legacy) {
		t.Error("the POSIX form must not be reported as runnable on Windows")
	}
}

// Directives off means there is no picker cache to maintain — the enable path
// removes the file — so a hook that keeps rewriting it would be maintaining
// something nothing offers.
func TestWriteGatesTheRefreshHookOnTheDirectivesFlag(t *testing.T) {
	path := withTempPath(t)

	if _, err := WriteWithOptions("http://127.0.0.1:9472",
		WriteOptions{ModelRouteDirectives: true, ModelPeerEntries: 4}); err != nil {
		t.Fatal(err)
	}
	if RefreshHookCommandAt(path) == "" {
		t.Fatal("directives on: the refresh hook must be installed")
	}

	if _, err := WriteWithOptions("http://127.0.0.1:9472",
		WriteOptions{ModelRouteDirectives: false}); err != nil {
		t.Fatal(err)
	}
	if cmd := RefreshHookCommandAt(path); cmd != "" {
		t.Errorf("directives off: the refresh hook must be removed, got %q", cmd)
	}
	// And the retired Stop hook is not reinstated by the same write.
	if cmd := StopHookCommandAt(path); cmd != "" {
		t.Errorf("a retired Stop hook is present: %q", cmd)
	}
}
