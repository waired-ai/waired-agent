package main

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain makes this package's suite independent of the machine it runs
// on (#386).
//
// The Keychain swap generalises what token_refresher_test.go's
// probeRefresher helper did for one code path: securestore execs
// /usr/bin/security on darwin, which is both slow and machine-global.
// Doing it here means a test added later cannot miss it.
//
// The OS well-known ollama paths used to be sealed here too, and were the
// reason TestSetupEngineState_SeesStateDirEngineImmediately failed on any
// Mac with Ollama.app installed: sealPATH closed $PATH and
// $WAIRED_OLLAMA_BINARY, but download.ResolveBinary's fourth source stat'd
// /Applications/Ollama.app/Contents/Resources/ollama, so "before the
// install" was already installed. #493 removed that walk — nothing in the
// tree looks for an ollama outside the state dir — so there is no longer a
// host install to seal out.
func TestMain(m *testing.M) {
	restore := securestore.SwapStoreForTest(securestore.NewMemStore())
	code := m.Run()
	restore()
	os.Exit(code)
}
