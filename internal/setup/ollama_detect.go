package setup

import (
	"context"
	"runtime"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// OllamaDetection summarises an ollama install already on this host.
// Used by the install decision (`waired init` / the setup executor), by
// `waired doctor`, and by Deploy.
type OllamaDetection struct {
	Installed bool
	Path      string
	Version   string // raw `ollama --version` token, e.g. "0.24.0"; "" if unknown
	// WairedManaged reports that this install was made BY waired. Only
	// Windows can still answer yes, via a marker file next to the binary:
	// there a waired-made install and the user's own live at the exact same
	// well-known path (%ProgramFiles%\Ollama), so path alone cannot tell
	// them apart. Linux and macOS install under the state dir, where the
	// path IS the answer (#492).
	WairedManaged bool
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
	return det
}

// DetectOllamaPathOnly is DetectOllama without the version probe — resolution
// and marker inspection only, and therefore no exec of the engine binary.
// Version is left zero.
//
// stateDir is unused since #492 (macOS recorded its ownership there while the
// engine was an app bundle at /Applications) and is kept so the two detection
// entry points still read alike from a call site.
func DetectOllamaPathOnly(stateDir string) OllamaDetection {
	_ = stateDir
	path, err := download.ResolveBinary("")
	if err != nil {
		return OllamaDetection{}
	}
	return OllamaDetection{
		Installed:     true,
		Path:          path,
		WairedManaged: managedFrom(runtime.GOOS, gatherManagedFacts(path)),
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
