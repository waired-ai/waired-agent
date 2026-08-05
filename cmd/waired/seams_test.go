package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain seals every machine-global input this package's tests reach,
// for the whole test binary. Per-test opt-in was tried and rots: a test
// added later compiles, passes on CI, and only misbehaves on a developer
// machine, which is the one place nobody treats a red suite as news
// (#386).
//
// What it seals, and why each one matters:
//
//   - The Keychain. logout's securestore.Remove would otherwise delete
//     the developer's real items on darwin. (On Linux/CI the keychain
//     stub returns ErrUnsupported and this is a no-op.)
//
//   - (The OS well-known ollama paths used to be sealed here too. Since
//     #493 nothing in the tree looks for an ollama outside the state dir,
//     so there is no host install left to leak in.)
//
//   - The user cache / home directory. os.UserCacheDir reads HOME on
//     darwin, LocalAppData on Windows and XDG_CACHE_HOME elsewhere. The
//     claude statusline fallback tests set only XDG_CACHE_HOME, which is
//     a no-op on macOS — so runFallbackHook wrote its per-session marker
//     into the developer's real ~/Library/Caches and the test passed
//     exactly once per machine, then failed forever after. All three
//     variables are pointed at one temp dir so the same hole cannot open
//     on a different OS.
//
// It also clears mgmtWriteBase for the whole binary: since waired#838
// management writes travel over a local IPC socket, which httptest cannot
// serve, so these tests address their httptest TCP servers verbatim. They
// cover command logic and endpoint semantics, both transport-independent;
// the socket routing itself is asserted in main_ipcwrite_unix_test.go and
// the transport in internal/management/ipcclient.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests exists so the cleanup below actually runs: os.Exit skips
// defers, and the temp home has to be removed on the way out.
func runTests(m *testing.M) int {
	restoreStore := securestore.SwapStoreForTest(securestore.NewMemStore())
	defer restoreStore()
	home, err := os.MkdirTemp("", "waired-cmd-test-home")
	if err != nil {
		panic("seal home: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(home) }()
	// os.UserCacheDir wants these to exist on darwin; creating them here
	// keeps every test from having to.
	_ = os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o755)
	os.Setenv("HOME", home)
	os.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	os.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))

	mgmtWriteBase = ""
	return m.Run()
}
