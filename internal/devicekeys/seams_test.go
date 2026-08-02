package devicekeys

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain installs a baseline in-memory Keychain for the whole test
// binary, so a test that forgets useMemKeychain still cannot exec
// /usr/bin/security or touch the developer's real machine key.
// useMemKeychain stays for the tests that want a FRESH store (#386).
func TestMain(m *testing.M) {
	restore := securestore.SwapStoreForTest(securestore.NewMemStore())
	code := m.Run()
	restore()
	os.Exit(code)
}
