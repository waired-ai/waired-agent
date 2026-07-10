//go:build windows

package download

import (
	"os"
	"path/filepath"
)

const ollamaCmdName = "ollama.exe"

// platformOllamaCandidates returns the well-known Windows install
// locations of `ollama.exe`. LocalSystem (= the account waired-agent
// runs under as a service) does not inherit any user's PATH, so a
// plain exec.LookPath call returns ErrNotFound even on machines where
// Ollama is installed. These candidates cover:
//
//   - the system-wide installer (recent versions of Ollama for Windows
//     write here when run as Administrator)
//   - the per-user installer's default location (Ollama for Windows
//     default when run non-elevated)
//
// Order matters: system-wide first so a LocalSystem service prefers
// the system-wide install even if a user has a stale per-user copy.
func platformOllamaCandidates() []string {
	var cands []string
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		cands = append(cands, filepath.Join(pf, "Ollama", "ollama.exe"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		cands = append(cands, filepath.Join(local, "Programs", "Ollama", "ollama.exe"))
	}
	return cands
}
