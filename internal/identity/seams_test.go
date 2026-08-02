package identity

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain installs a baseline in-memory Keychain for the whole test
// binary, so a test that forgets useMemKeychain still cannot exec
// /usr/bin/security or read the developer's real tokens. useMemKeychain
// stays for the tests that want a FRESH store — every secret here shares
// one (account, service) item, so isolation is still per test (#386).
func TestMain(m *testing.M) {
	restore := securestore.SwapStoreForTest(securestore.NewMemStore())
	code := m.Run()
	restore()
	os.Exit(code)
}
