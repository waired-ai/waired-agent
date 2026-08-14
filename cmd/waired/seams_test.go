package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
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
//   - The Claude Code managed-settings path. It is machine-global
//     (/etc/claude-code, /Library/Application Support/ClaudeCode,
//     %ProgramFiles%\ClaudeCode), and since waired-agent#796 init's closing card
//     reads it to decide what to say about Claude Code routing. Unsealed, this
//     package's tests would answer that question from whatever the developer's
//     own machine has — green on a clean runner, and differently wrong on the
//     machine editing the code, which is exactly #386's shape.
//
//   - The ASCII fold (ascii.go). Whether output is rewritten depends on the
//     host OS and its console code page, so the same assertion on a printed
//     string passed on Linux and failed on the Windows job, where `go test`
//     has no console and asciiOnlySink is therefore true (#629). Sealed off
//     for the whole binary: these tests assert product strings as authored,
//     and the folded rendering has its own guard in
//     TestPlainInitOutputIsPureASCII, which pins it back on for itself.
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
	// Under the sealed home, so a test that writes one gets a fresh tree and
	// nothing lands in the real machine-wide location.
	restorePath := claudemanaged.SwapPathForTest(
		filepath.Join(home, "claude-code", "managed-settings.json"))
	defer restorePath()

	mgmtWriteBase = ""
	off := false
	foldCached = &off
	// The step-6 host-speed wait is minutes in production (the probe
	// model has to download); at test scale every login-flow fixture that
	// parks on a measurement-less status would otherwise burn the full
	// wait. Sealed here for the same reason as the rest: an opt-in shrink
	// only protects the tests that remember it. Tests that exercise the
	// wait itself re-shrink locally.
	hostSpeedAskWait, hostSpeedAskPoll = 250*time.Millisecond, 10*time.Millisecond
	// Same reasoning one wait later. engineWaitForStatus bounds how long
	// the daemon path lets a just-started daemon settle, and since #746
	// the state-dir read waits on it too — in front of the gate that used
	// to return instantly. Every login-flow fixture that serves a setup
	// state without a state dir would otherwise burn the full 20 s.
	// Tests that exercise the give-up itself re-shrink locally.
	engineWaitForStatus = 250 * time.Millisecond
	return m.Run()
}
