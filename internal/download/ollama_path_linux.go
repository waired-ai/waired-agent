//go:build linux

package download

// ollamaCmdName is the binary name exec.LookPath searches for. On
// Linux it is plain "ollama".
const ollamaCmdName = "ollama"

// platformOllamaCandidates returns well-known install paths to probe
// when LookPath fails. The Linux official install script puts ollama
// on /usr/local/bin (already in PATH for interactive shells) — we
// still list it explicitly so systemd-run with a stripped PATH can
// find the binary. Override + WAIRED_OLLAMA_BINARY remain available
// for non-standard installs.
func platformOllamaCandidates() []string {
	return []string{
		"/usr/local/bin/ollama",
		"/usr/bin/ollama",
	}
}
