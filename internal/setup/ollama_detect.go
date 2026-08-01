package setup

import (
	"context"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// OllamaDetection summarises a pre-existing ollama install. Used by the
// `waired init` bundled-vs-reuse prompt (#188) and by Deploy.
type OllamaDetection struct {
	Installed bool
	Path      string
	Version   string // raw `ollama --version` token, e.g. "0.24.0"; "" if unknown
	Supported bool   // Version >= OllamaSupportedMinVersion
	// WairedManaged reports that this install was made BY waired. Windows
	// answers this with a marker file next to the binary; macOS with a
	// record in the state dir (a marker inside the signed .app bundle
	// breaks its signature — #329). It exists because a waired-made install
	// and the user's own live at the exact same well-known paths
	// (%ProgramFiles%\Ollama, /Applications/Ollama.app), so path alone
	// cannot tell them apart.
	WairedManaged bool
	// LegacyBundleMarkerPath is non-empty when this host still carries the
	// pre-#329 marker inside the Ollama.app bundle root — i.e. this install
	// is one waired broke, and this is the file to delete to fix it.
	// RepairDarwinBundleMarker consumes it.
	LegacyBundleMarkerPath string
}

// DetectOllama resolves a pre-existing ollama and (best-effort) its
// version. It never errors: an absent or unreadable ollama yields a
// zero-value (Installed=false) detection.
//
// Resolution goes through download.ResolveBinary so init's detection
// matches what waired-agent's runtime will actually find at boot:
// $WAIRED_OLLAMA_BINARY, then $PATH, then OS well-known install paths.
// The last step matters on macOS (the Ollama.app GUI install lands at
// /Applications/Ollama.app/Contents/Resources/ollama and is NOT on
// $PATH unless the user runs "Install command line") and on Windows
// (a LocalSystem service does not inherit a user PATH). A plain
// exec.LookPath there falsely reports "ollama missing" and skips the
// bundled-model pre-pull. (#268)
//
// stateDir is where macOS keeps its waired-managed record (#329); "" is
// allowed and simply means "no record available", which downgrades
// WairedManaged to what the on-disk markers alone can prove.
func DetectOllama(ctx context.Context, stateDir string) OllamaDetection {
	det := DetectOllamaPathOnly(stateDir)
	if !det.Installed {
		return det
	}
	det.Version = detectOllamaVersion(ctx, det.Path)
	det.Supported = det.Version != "" &&
		infruntime.OllamaVersionAtLeast(det.Version, infruntime.OllamaSupportedMinVersion)
	return det
}

// DetectOllamaPathOnly is DetectOllama without the version probe — resolution
// and marker inspection only, and therefore no exec of the engine binary.
//
// That distinction is load-bearing for the repair path (#329). On a host whose
// Ollama.app has a broken signature seal, exec'ing the bundled binary is
// SIGKILLed by Gatekeeper AND re-raises the "Ollama is damaged" dialog — so
// the code whose whole job is to fix that must not run the version probe
// first. Version/Supported are left zero.
func DetectOllamaPathOnly(stateDir string) OllamaDetection {
	path, err := download.ResolveBinary("")
	if err != nil {
		return OllamaDetection{}
	}
	facts, legacyMarker := gatherManagedFacts(stateDir, path)
	return OllamaDetection{
		Installed:              true,
		Path:                   path,
		WairedManaged:          managedFrom(runtime.GOOS, facts),
		LegacyBundleMarkerPath: legacyMarker,
	}
}

// detectOllamaVersion returns the version of the ollama at path, or ""
// on any error. Best-effort: the init prompt only uses it to decide
// whether to show an "unsupported" warning, never to block.
//
// hardware.EngineVersionAt does the running and the parsing — it is the
// same "execute the RESOLVED path, parse as the engine kind" operation
// the profiler needs (#238), and it skips the "could not connect to a
// running Ollama instance" line a fresh install prints (the old
// last-token-of-the-first-line logic returned "instance" there and
// mis-flagged a healthy engine as unsupported).
func detectOllamaVersion(ctx context.Context, path string) string {
	_, ver := hardware.EngineVersionAt(ctx, "ollama", path)
	return ver
}
