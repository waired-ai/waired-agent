package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// touch creates an empty file (parents included) and fails the test on error.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWairedManagedMarker(t *testing.T) {
	cases := []struct {
		name string
		// paths are relative to a fresh temp root; bin is the binary path
		// probed, marker ("" = none) is the marker file to create.
		bin    string
		marker string
		want   bool
	}{
		{
			name:   "windows-style same-dir marker",
			bin:    filepath.Join("Program Files", "Ollama", "ollama.exe"),
			marker: filepath.Join("Program Files", "Ollama", WairedManagedMarkerName),
			want:   true,
		},
		{
			name: "windows-style no marker",
			bin:  filepath.Join("Program Files", "Ollama", "ollama.exe"),
			want: false,
		},
		{
			name:   "app-bundle marker at bundle root",
			bin:    filepath.Join("Applications", "Ollama.app", "Contents", "Resources", "ollama"),
			marker: filepath.Join("Applications", "Ollama.app", WairedManagedMarkerName),
			want:   true,
		},
		{
			name: "app-bundle no marker",
			bin:  filepath.Join("Applications", "Ollama.app", "Contents", "Resources", "ollama"),
			want: false,
		},
		{
			name: "plain PATH install never walks to root",
			bin:  filepath.Join("usr", "local", "bin", "ollama"),
			want: false,
		},
		{
			name:   "marker outside the bundle is not picked up",
			bin:    filepath.Join("Applications", "Ollama.app", "Contents", "Resources", "ollama"),
			marker: filepath.Join("Applications", WairedManagedMarkerName),
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, tc.bin)
			touch(t, bin)
			if tc.marker != "" {
				touch(t, filepath.Join(root, tc.marker))
			}
			if got := wairedManagedMarker(bin); got != tc.want {
				t.Errorf("wairedManagedMarker(%q) = %v, want %v", tc.bin, got, tc.want)
			}
		})
	}
}
