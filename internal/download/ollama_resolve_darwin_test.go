//go:build darwin

package download

import (
	"os"
	"testing"
)

// TestPlatformOllamaCandidatesDarwin locks the macOS candidate set that
// ResolveBinary stats when ollama is not on $PATH. waired init's
// DetectOllama relies on this list to find the Ollama.app GUI install
// (which is NOT on $PATH unless the user runs "Install command line")
// and Homebrew installs under /opt/homebrew/bin. (#268)
func TestPlatformOllamaCandidatesDarwin(t *testing.T) {
	got := platformOllamaCandidates()
	want := []string{
		"/Applications/Ollama.app/Contents/Resources/ollama",
		"/usr/local/bin/ollama",
		"/opt/homebrew/bin/ollama",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestResolveBinaryCandidatePathRealHost proves ResolveBinary discovers
// an ollama that lives at a well-known macOS install path but is NOT on
// $PATH — the exact situation a stock Ollama.app install creates.
//
// Gated by WAIRED_OLLAMA_REALHOST=1 because it writes a throwaway
// executable into a real candidate directory (and skips if none is
// writable). t.Cleanup removes it. Run manually:
//
//	WAIRED_OLLAMA_REALHOST=1 go test ./internal/download/ -run CandidatePathRealHost -v
func TestResolveBinaryCandidatePathRealHost(t *testing.T) {
	if os.Getenv("WAIRED_OLLAMA_REALHOST") == "" {
		t.Skip("set WAIRED_OLLAMA_REALHOST=1 to exercise candidate-path resolution")
	}
	// Pick the first writable candidate directory and confirm nothing
	// real is already there (never shadow a genuine install).
	var target string
	for _, cand := range platformOllamaCandidates() {
		if _, err := os.Stat(cand); err == nil {
			t.Skipf("real ollama already present at %q; skipping to avoid shadowing", cand)
		}
		dir := dirOf(cand)
		if isDirWritable(dir) {
			target = cand
			break
		}
	}
	if target == "" {
		t.Skip("no writable macOS candidate directory available")
	}

	if err := os.WriteFile(target, []byte("#!/bin/sh\necho \"ollama version is 0.0.0-realhost-stub\"\n"), 0o755); err != nil {
		t.Fatalf("write stub at %q: %v", target, err)
	}
	t.Cleanup(func() { _ = os.Remove(target) })

	t.Setenv("PATH", "")                 // force the candidate-stat branch
	t.Setenv("WAIRED_OLLAMA_BINARY", "") // and skip the env override

	got, err := ResolveBinary("")
	if err != nil {
		t.Fatalf("ResolveBinary: %v", err)
	}
	if got != target {
		t.Errorf("got %q, want %q (off-PATH candidate)", got, target)
	}
	t.Logf("resolved off-PATH candidate: %s", got)
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func isDirWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := dir + "/.waired-write-probe"
	f, err := os.Create(probe)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}
