//go:build windows

package download

// ollamaCmdName is the binary name exec.LookPath searches for. See the
// linux || darwin file for why this stays build-tagged rather than
// taking a goos.
const ollamaCmdName = "ollama.exe"
