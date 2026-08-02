package setup

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain makes this package's suite independent of the machine it runs
// on. Two machine-global sources reach in (#386):
//
//   - The Keychain. Integration() goes through
//     integration.LoadOrCreateGatewayToken -> securestore.Read, which is
//     Keychain-first and only reads the passed path on a miss. On a host
//     where `sudo waired init` has run, the real gateway-token item hits,
//     LoadOrCreateGatewayToken returns early, and the token file under
//     the test's state dir is never written — which is why
//     TestIntegration_LoadsTokenAndDispatches failed with ENOENT on a
//     developer Mac while passing on a clean CI runner.
//
//   - The OS well-known ollama paths. DetectOllama calls
//     download.ResolveBinary, whose fourth source stats
//     /Applications/Ollama.app/... regardless of $PATH.
//
// Sealed for the whole package rather than per test: both sources are
// reached indirectly, several layers down, so an opt-in helper is easy to
// forget and impossible to notice forgetting.
func TestMain(m *testing.M) {
	restoreStore := securestore.SwapStoreForTest(securestore.NewMemStore())
	restoreCands := download.SwapCandidatesForTest(nil)
	code := m.Run()
	restoreCands()
	restoreStore()
	os.Exit(code)
}
