package download

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"testing"
)

// noEnv is the empty environment: every lookup misses. Used for the rows
// where the answer must not depend on the process the test runs in.
func noEnv(string) string { return "" }

// TestOllamaCandidates pins the well-known install paths per goos.
//
// Product contract, not a record of today's behaviour: waired init's
// DetectOllama relies on this list to find the Ollama.app GUI install
// (which is NOT on $PATH unless the user runs "Install command line"),
// Homebrew installs under /opt/homebrew/bin, and — on Windows — an
// install a LocalSystem service cannot see through $PATH at all (#268).
//
// This replaces TestPlatformOllamaCandidatesDarwin, which asserted the
// same darwin list from a darwin-tagged file: the linux and windows lists
// were never asserted anywhere, because no runner ran a test that could
// see them. Now all three are checked on all three runners (#386).
func TestOllamaCandidates(t *testing.T) {
	winEnv := func(k string) string {
		switch k {
		case "ProgramFiles":
			return `C:\Program Files`
		case "LOCALAPPDATA":
			return `C:\Users\dev\AppData\Local`
		}
		return ""
	}

	cases := []struct {
		name string
		goos string
		env  func(string) string
		want []string
	}{
		{
			name: "darwin",
			goos: "darwin",
			env:  noEnv,
			want: []string{
				"/Applications/Ollama.app/Contents/Resources/ollama",
				"/usr/local/bin/ollama",
				"/opt/homebrew/bin/ollama",
			},
		},
		{
			name: "linux",
			goos: "linux",
			env:  noEnv,
			want: []string{
				"/usr/local/bin/ollama",
				"/usr/bin/ollama",
			},
		},
		{
			name: "windows",
			goos: "windows",
			env:  winEnv,
			// System-wide first: a LocalSystem service must prefer it over
			// a user's possibly-stale per-user copy.
			want: []string{
				`C:\Program Files\Ollama\ollama.exe`,
				`C:\Users\dev\AppData\Local\Programs\Ollama\ollama.exe`,
			},
		},
		{
			// The Windows list is entirely env-derived, so a stripped
			// environment must yield nothing rather than a bogus relative
			// path like `\Ollama\ollama.exe`.
			name: "windows with no environment",
			goos: "windows",
			env:  noEnv,
			want: nil,
		},
		{
			// An unknown goos must not fall through to another OS's paths.
			name: "unknown goos",
			goos: "plan9",
			env:  winEnv,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ollamaCandidates(tc.goos, tc.env)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ollamaCandidates(%q) = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}

// TestOllamaCandidatesDarwinPathsAreAbsolute guards the one property a
// per-goos literal list can silently lose: filepath.Join on the running
// host would rewrite these separators, so they are written literally.
func TestOllamaCandidatesAreAbsolute(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		for _, c := range ollamaCandidates(goos, noEnv) {
			if c == "" || c[0] != '/' {
				t.Errorf("%s candidate %q is not an absolute unix path", goos, c)
			}
		}
	}
}

// TestSwapCandidatesForTestRestores pins the seam's own contract — the
// consuming packages install it from TestMain and never restore, so a
// broken restore would only show up as cross-package contamination.
func TestSwapCandidatesForTestRestores(t *testing.T) {
	before := currentCandidates()

	restore := SwapCandidatesForTest([]string{"/nowhere/ollama"})
	if got := currentCandidates(); !slices.Equal(got, []string{"/nowhere/ollama"}) {
		t.Fatalf("after swap, candidates = %v", got)
	}
	restore()

	if got := currentCandidates(); !slices.Equal(got, before) {
		t.Errorf("after restore, candidates = %v, want %v", got, before)
	}
}

// TestResolveBinaryUsesSwappedCandidate proves the seam is wired into
// ResolveBinary's fourth source, not merely settable. Without this, the
// TestMain calls in the consuming packages could be no-ops.
func TestResolveBinaryUsesSwappedCandidate(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")

	dir := t.TempDir()
	stub := writeStubBinary(t, dir)
	t.Cleanup(SwapCandidatesForTest([]string{stub}))

	got, err := ResolveBinary("")
	if err != nil {
		t.Fatalf("ResolveBinary: %v", err)
	}
	if got != stub {
		t.Errorf("got %q, want the swapped candidate %q", got, stub)
	}

	// And a candidate that does not exist is skipped, not returned.
	t.Cleanup(SwapCandidatesForTest([]string{"", dir + "/absent"}))
	if _, err := ResolveBinary(""); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled for a non-existent candidate", err)
	}
}

// TestResolveBinaryRealCandidatesRunUnsealed is the table-test-on-realFn
// that CLAUDE.md §"Test discipline" requires of a `var xFn = realFn`
// seam: without it, nothing would ever call the production closure that
// reads runtime.GOOS and os.Getenv.
func TestResolveBinaryRealCandidatesRunUnsealed(t *testing.T) {
	want := ollamaCandidates(runtime.GOOS, os.Getenv)
	if got := currentCandidates(); !slices.Equal(got, want) {
		t.Errorf("unsealed candidates = %v, want the running OS's list %v", got, want)
	}
}
