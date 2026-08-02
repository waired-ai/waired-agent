//go:build linux || darwin

package download

// ollamaCmdName is the binary name exec.LookPath searches for. On both
// Unixes it is plain "ollama".
//
// Unlike the candidate list (ollamaCandidates, which takes a goos and is
// therefore table-testable from any host), this one is genuinely fixed by
// the build tag of the RUNNING OS: it names what this process would exec,
// not what some other OS would install.
const ollamaCmdName = "ollama"
