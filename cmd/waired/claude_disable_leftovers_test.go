package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// TestDisableRemovesRetiredUserLeftovers drives the per-user picker step
// `waired claude disable` runs, on the no-hop path (Windows, a root login, an
// unelevated run), with no ANTHROPIC_BASE_URL anywhere, which is the state
// disable itself has put managed settings in by the time it gets here.
//
// Before waired-agent#1398 this path never called the cache removal at all,
// and the hop path compared the cache's baseUrl with a base URL disable had
// already deleted, so both files survived every disable on every host.
func TestDisableRemovesRetiredUserLeftovers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	// No hop: the step runs in this process, against this home.
	t.Setenv("SUDO_USER", "")

	cachePath := claudecode.RetiredCachePath("", home)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(
		`{"baseUrl":"http://127.0.0.1:9472","models":[{"id":"claude-waired-auto"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	userCache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	markers := filepath.Join(userCache, "waired", "claude-fallback")
	if err := os.MkdirAll(markers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markers, "session"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	removePickerRowsForInvoker()

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("%s survived disable (stat err = %v)", cachePath, err)
	}
	if _, err := os.Stat(markers); !os.IsNotExist(err) {
		t.Errorf("%s survived disable (stat err = %v)", markers, err)
	}
}
