//go:build darwin

package download

// ollamaCmdName is the binary name exec.LookPath searches for on
// macOS.
const ollamaCmdName = "ollama"

// platformOllamaCandidates returns well-known macOS install paths.
// The desktop .app bundles ollama at Contents/Resources/ollama; the
// CLI-only install (and brew on x86 Macs) puts it on /usr/local/bin;
// Homebrew on Apple Silicon uses /opt/homebrew/bin. launchd-started
// services do not inherit a Terminal-style PATH so being explicit
// matters here.
func platformOllamaCandidates() []string {
	return []string{
		"/Applications/Ollama.app/Contents/Resources/ollama",
		"/usr/local/bin/ollama",
		"/opt/homebrew/bin/ollama",
	}
}
