package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"

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
	// WairedManaged reports that this install was made BY waired (the
	// installer helpers drop a marker file next to the binary / app
	// bundle). The bundled-vs-reuse prompt skips itself for such installs:
	// asking "reuse the existing Ollama?" about an Ollama waired itself put
	// there confused every Windows/macOS first run (the installer used to
	// pre-install the engine right before `waired init` re-detected it as
	// "existing").
	WairedManaged bool
}

// WairedManagedMarkerName is the marker file the waired installer helpers
// (scripts/install/ollama-windows.ps1, the macOS Ollama.app installer) write
// next to an Ollama install they created. Its presence is the only reliable
// way to tell a waired-made install from the user's own: both live at the
// exact same well-known paths (%ProgramFiles%\Ollama, /Applications).
const WairedManagedMarkerName = ".waired-managed.json"

// wairedManagedMarker reports whether the install at binPath carries the
// waired-managed marker. Pure path logic (no GOOS branches): the marker is
// looked up in the binary's own directory and in every ancestor up to and
// including a `*.app` bundle root — the macOS layout puts the binary at
// Ollama.app/Contents/Resources/ollama while the marker sits at the bundle
// root, and Windows/Linux land on the first (same-dir) probe.
func wairedManagedMarker(binPath string) bool {
	dir := filepath.Dir(binPath)
	if _, err := os.Stat(filepath.Join(dir, WairedManagedMarkerName)); err == nil {
		return true
	}
	// App-bundle layout: probe the nearest *.app ancestor (the marker sits
	// at the bundle root, three levels above Contents/Resources/ollama).
	// Plain directory installs never match an .app ancestor and fall
	// through to false without extra stats.
	for d := dir; ; {
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
		if strings.HasSuffix(strings.ToLower(d), ".app") {
			_, err := os.Stat(filepath.Join(d, WairedManagedMarkerName))
			return err == nil
		}
	}
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
func DetectOllama(ctx context.Context) OllamaDetection {
	path, err := download.ResolveBinary("")
	if err != nil {
		return OllamaDetection{}
	}
	ver := detectOllamaVersion(ctx, path)
	return OllamaDetection{
		Installed:     true,
		Path:          path,
		Version:       ver,
		Supported:     ver != "" && infruntime.OllamaVersionAtLeast(ver, infruntime.OllamaSupportedMinVersion),
		WairedManaged: wairedManagedMarker(path),
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
