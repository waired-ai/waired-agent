package main

import "time"

// The engine install used to be bounded by a fixed 15-minute wall clock on
// all three OSes. A ~1.43 GB download needs about 16 minutes at 1.5 MB/s, so
// a perfectly healthy slow link was killed just short of finishing — and on
// Windows the kill is TerminateProcess(handle, 1), whose exit code 1 Wait
// returns in preference to context.DeadlineExceeded, erasing the timeout
// from the report entirely (#189).
//
// The bound that belongs on a download is time-without-progress, not total
// elapsed time, and that is where it now lives: the PowerShell downloader
// has always had it (60 s connect, 120 s per-read), and internal/download's
// Fetch grew the same watchdog for the Linux/macOS transfers. What remains
// here is a generous backstop against a genuinely wedged installer, plus an
// escape hatch for links slow enough to make even that unreasonable.
const (
	// ollamaInstallTimeoutDefault is the backstop, not the working bound.
	ollamaInstallTimeoutDefault = 60 * time.Minute

	// ollamaInstallTimeoutEnv overrides it, e.g. "3h" on a very slow link.
	ollamaInstallTimeoutEnv = "WAIRED_OLLAMA_INSTALL_TIMEOUT"
)

// ollamaInstallTimeout resolves the install backstop from the environment.
// lookup is os.Getenv in production and a stub in tests. An unparseable or
// non-positive value falls back to the default rather than failing the
// install: the knob is an escape hatch, and a typo in it must not be a
// harder failure than not setting it at all.
func ollamaInstallTimeout(lookup func(string) string) time.Duration {
	raw := lookup(ollamaInstallTimeoutEnv)
	if raw == "" {
		return ollamaInstallTimeoutDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return ollamaInstallTimeoutDefault
	}
	return d
}
