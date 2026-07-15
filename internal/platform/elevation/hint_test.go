package elevation

import (
	"runtime"
	"testing"
)

func TestHintFor(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		cmdline string
		want    string
	}{
		{"windows with cmd", "windows", "waired init", "re-run `waired init` from an elevated (Administrator) prompt"},
		{"windows empty", "windows", "", "re-run from an elevated (Administrator) prompt"},
		{"linux with cmd", "linux", "waired init", "run `sudo waired init`"},
		{"linux empty", "linux", "", "re-run with sudo"},
		{"darwin with cmd", "darwin", "waired runtimes install ollama", "run `sudo waired runtimes install ollama`"},
		{"darwin empty", "darwin", "", "re-run with sudo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HintFor(tc.goos, tc.cmdline); got != tc.want {
				t.Errorf("HintFor(%q, %q) = %q, want %q", tc.goos, tc.cmdline, got, tc.want)
			}
		})
	}
}

// TestHintDelegatesToRuntimeGOOS pins that the live-OS wrapper is exactly
// HintFor keyed on runtime.GOOS (the two must never drift).
func TestHintDelegatesToRuntimeGOOS(t *testing.T) {
	for _, cmd := range []string{"", "waired init"} {
		if got, want := Hint(cmd), HintFor(runtime.GOOS, cmd); got != want {
			t.Errorf("Hint(%q) = %q, want HintFor(%q, %q) = %q", cmd, got, runtime.GOOS, cmd, want)
		}
	}
}
