package setup

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain makes this package's suite independent of the machine it runs
// on. The Keychain reaches in (#386): Integration() goes through
// integration.LoadOrCreateGatewayToken -> securestore.Read, which is
// Keychain-first and only reads the passed path on a miss. On a host where
// `sudo waired init` has run, the real gateway-token item hits,
// LoadOrCreateGatewayToken returns early, and the token file under the
// test's state dir is never written — which is why
// TestIntegration_LoadsTokenAndDispatches failed with ENOENT on a developer
// Mac while passing on a clean CI runner.
//
// The other machine-global source this used to seal — the OS well-known
// ollama paths, reached through download.ResolveBinary — is gone with the
// walk itself (#493). No code in the tree looks for an ollama outside the
// state dir any more, so there is nothing left to contaminate.
//
// Sealed for the whole package rather than per test: it is reached
// indirectly, several layers down, so an opt-in helper is easy to forget
// and impossible to notice forgetting.
func TestMain(m *testing.M) {
	restore := securestore.SwapStoreForTest(securestore.NewMemStore())
	code := m.Run()
	restore()
	os.Exit(code)
}
