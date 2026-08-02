package download

import (
	"os"
	"runtime"
	"sync"
)

// ollamaCandidates lists the well-known install paths to probe for ollama
// on goos, in order, when it is not on $PATH. Pure: goos and env are
// inputs, so all three OSes are table-testable from any host (CLAUDE.md
// §Cross-OS parity). nvidiaSMICandidates in internal/hardware is the same
// shape and the model this follows.
//
// Paths use literal separators rather than filepath.Join because the
// answer is about goos, not about the host running the code — the unit
// suite runs this table on all three.
func ollamaCandidates(goos string, env func(string) string) []string {
	switch goos {
	case "darwin":
		// The desktop .app bundles ollama at Contents/Resources/ollama;
		// the CLI-only install (and brew on x86 Macs) puts it on
		// /usr/local/bin; Homebrew on Apple Silicon uses /opt/homebrew/bin.
		// launchd-started services do not inherit a Terminal-style PATH so
		// being explicit matters here.
		return []string{
			"/Applications/Ollama.app/Contents/Resources/ollama",
			"/usr/local/bin/ollama",
			"/opt/homebrew/bin/ollama",
		}
	case "linux":
		// The Linux official install script puts ollama on /usr/local/bin
		// (already in PATH for interactive shells) — we still list it
		// explicitly so systemd-run with a stripped PATH can find the
		// binary. Override + WAIRED_OLLAMA_BINARY remain available for
		// non-standard installs.
		return []string{
			"/usr/local/bin/ollama",
			"/usr/bin/ollama",
		}
	case "windows":
		// LocalSystem (= the account waired-agent runs under as a service)
		// does not inherit any user's PATH, so a plain exec.LookPath call
		// returns ErrNotFound even on machines where Ollama is installed.
		// These candidates cover the system-wide installer (recent Ollama
		// for Windows when run as Administrator) and the per-user
		// installer's default location. Order matters: system-wide first so
		// a LocalSystem service prefers it even if a user has a stale
		// per-user copy.
		var cands []string
		if pf := env("ProgramFiles"); pf != "" {
			cands = append(cands, pf+`\Ollama\ollama.exe`)
		}
		if local := env("LOCALAPPDATA"); local != "" {
			cands = append(cands, local+`\Programs\Ollama\ollama.exe`)
		}
		return cands
	}
	return nil
}

// candidatesFn is the seam ResolveBinary probes through. Production reads
// the running OS; SwapCandidatesForTest replaces it.
var (
	candidatesMu sync.RWMutex
	candidatesFn = func() []string { return ollamaCandidates(runtime.GOOS, os.Getenv) }
)

func currentCandidates() []string {
	candidatesMu.RLock()
	defer candidatesMu.RUnlock()
	return candidatesFn()
}

// SwapCandidatesForTest replaces the well-known-path candidate list
// process-wide and returns a function that restores the previous one.
//
// ResolveBinary consults four sources; a test can neutralise the first
// three (pass "" for override, t.Setenv the env override and $PATH) but
// not the fourth, which stats hardcoded absolute paths against the real
// filesystem. On a developer machine with Ollama installed that made
// "pretend nothing is installed" unwritable, so five tests across four
// packages dodged it with `if err == nil { t.Skip }` — a condition that
// cannot tell a contaminated host from a subject that wrongly succeeded.
// Passing nil closes the last source:
//
//	download.SwapCandidatesForTest(nil)
//
// It is exported (rather than living in an _test.go file) precisely so
// tests in the consuming packages — cmd/waired, cmd/waired-agent,
// internal/setup — can reach it, the same reason
// securestore.SwapStoreForTest is. Call it from TestMain so the package
// is hermetic by construction rather than per-test opt-in (#386).
func SwapCandidatesForTest(list []string) (restore func()) {
	candidatesMu.Lock()
	prev := candidatesFn
	candidatesFn = func() []string { return list }
	candidatesMu.Unlock()
	return func() {
		candidatesMu.Lock()
		candidatesFn = prev
		candidatesMu.Unlock()
	}
}
