package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// touch creates a file with placeholder JSON (parents included) and fails the
// test on error.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestManagedFrom is a PRODUCT CONTRACT test: which evidence counts as
// "waired installed this" per OS.
//
// Windows is the only OS left that has to ask. Its bundled install lives at
// the same %ProgramFiles%\Ollama a user's own install would, so a marker file
// is the only thing that can tell them apart. Linux and macOS install under
// the state dir, where the path itself is the answer and nothing else counts —
// which is exactly what #488 means by "waired serves only with an engine it
// installed".
func TestManagedFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		goos  string
		facts ManagedFacts
		want  bool
	}{
		{"windows same-dir marker", "windows", ManagedFacts{MarkerBesideBinary: true}, true},
		{"windows no marker", "windows", ManagedFacts{}, false},

		// #492/#493: a marker cannot make an install outside the state dir
		// ours on the two OSes that resolve strictly. Before #492 macOS
		// answered this with a record in the state dir naming an
		// /Applications bundle; that whole apparatus went with the bundle.
		{"linux marker proves nothing", "linux", ManagedFacts{MarkerBesideBinary: true}, false},
		{"linux nothing", "linux", ManagedFacts{}, false},
		{"darwin marker proves nothing", "darwin", ManagedFacts{MarkerBesideBinary: true}, false},
		{"darwin nothing", "darwin", ManagedFacts{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedFrom(tc.goos, tc.facts); got != tc.want {
				t.Errorf("managedFrom(%q, %+v) = %v, want %v", tc.goos, tc.facts, got, tc.want)
			}
		})
	}
}

// TestGatherManagedFacts observes a real directory: the marker is read from
// beside the binary, not from anywhere else on the host.
func TestGatherManagedFacts(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "Ollama", "ollama.exe")
	touch(t, bin)

	if got := gatherManagedFacts(bin); got.MarkerBesideBinary {
		t.Errorf("MarkerBesideBinary = true with no marker on disk")
	}
	touch(t, filepath.Join(filepath.Dir(bin), WairedManagedMarkerName))
	if got := gatherManagedFacts(bin); !got.MarkerBesideBinary {
		t.Errorf("MarkerBesideBinary = false with %s beside the binary", WairedManagedMarkerName)
	}
	// A marker one directory up is not this install's receipt.
	other := filepath.Join(root, "Elsewhere", "ollama.exe")
	touch(t, other)
	if got := gatherManagedFacts(other); got.MarkerBesideBinary {
		t.Errorf("a marker in a sibling directory was read as this install's")
	}
}
