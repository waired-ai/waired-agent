package deauth

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain makes this package's suite independent of the machine it runs
// on. Deregister reads the access token through identity.LoadAccessToken
// -> securestore.Read, which prefers the macOS Keychain and only falls
// back to the file under the state dir. seedEnrolled writes "tok" into a
// t.TempDir(), but on a developer Mac with a real enrollment the Keychain
// hit wins and the temp dir is never read — so the test sent the
// machine's REAL device token to its httptest server and printed it in
// the failure message (#386).
//
// The mirror hazard is just as bad: on a Keychain *miss* securestore
// opportunistically migrates whatever the file holds into the Keychain,
// so an unsealed run would write the literal "tok" over the developer's
// own access-token item.
//
// Installed here rather than per test (the pattern the rest of the repo
// had) so a test added later is hermetic by construction instead of by
// remembering to call a helper.
func TestMain(m *testing.M) {
	restore := securestore.SwapStoreForTest(securestore.NewMemStore())
	code := m.Run()
	restore()
	os.Exit(code)
}
