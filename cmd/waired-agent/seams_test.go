package main

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// TestMain makes this package's suite independent of the machine it runs
// on (#386).
//
// The well-known ollama paths are the reason
// TestSetupEngineState_SeesStateDirEngineImmediately failed on any Mac
// with Ollama.app installed: sealPATH closes $PATH and
// $WAIRED_OLLAMA_BINARY, but download.ResolveBinary's fourth source
// stats /Applications/Ollama.app/Contents/Resources/ollama, so
// "before the install" was already installed. setupEngineState takes no
// goos, so unlike its siblings that test could not pin itself to linux
// to dodge it.
//
// The Keychain swap generalises what token_refresher_test.go's
// probeRefresher helper did for one code path: securestore execs
// /usr/bin/security on darwin, which is both slow and machine-global.
// Doing it here means a test added later cannot miss it.
func TestMain(m *testing.M) {
	restoreStore := securestore.SwapStoreForTest(securestore.NewMemStore())
	restoreCands := download.SwapCandidatesForTest(nil)
	code := m.Run()
	restoreCands()
	restoreStore()
	os.Exit(code)
}
