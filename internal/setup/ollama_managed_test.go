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
// It replaces the old TestWairedManagedMarker, and deliberately INVERTS two of
// its pins (#329): a marker at the .app bundle root is no longer the macOS
// signal (writing it there is what broke the bundle's signature seal), and the
// state-dir record outside the bundle — which the old test pinned as "not
// picked up" — is now the primary one. The legacy in-bundle marker still
// counts, so hosts the repair has not reached yet stay recognisably ours.
func TestManagedFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		goos  string
		facts ManagedFacts
		want  bool
	}{
		{"windows same-dir marker", "windows", ManagedFacts{MarkerBesideBinary: true}, true},
		{"windows no marker", "windows", ManagedFacts{}, false},
		{"linux same-dir marker", "linux", ManagedFacts{MarkerBesideBinary: true}, true},
		{"linux no marker", "linux", ManagedFacts{}, false},

		{"darwin state-dir record", "darwin", ManagedFacts{StateRecordMatches: true}, true},
		{"darwin legacy in-bundle marker still counts", "darwin",
			ManagedFacts{LegacyBundleMarker: true}, true},
		{"darwin both", "darwin",
			ManagedFacts{StateRecordMatches: true, LegacyBundleMarker: true}, true},
		{"darwin nothing", "darwin", ManagedFacts{}, false},
		// The polarity that matters: on macOS a same-dir marker is not a
		// signal at all. The binary lives at Contents/Resources/ollama, so a
		// marker beside it would be buried INSIDE the bundle — even more
		// seal-breaking than the bundle-root one this issue removes.
		{"darwin ignores a marker beside the binary", "darwin",
			ManagedFacts{MarkerBesideBinary: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedFrom(tc.goos, tc.facts); got != tc.want {
				t.Errorf("managedFrom(%q, %+v) = %v, want %v", tc.goos, tc.facts, got, tc.want)
			}
		})
	}
}

// TestBundleRoot pins the pure path walk. Real-filesystem-free and
// GOOS-agnostic (filepath.Join keeps it valid on every runner).
func TestBundleRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  string
		want string
	}{
		{
			name: "app bundle",
			bin:  filepath.Join("Applications", "Ollama.app", "Contents", "Resources", "ollama"),
			want: filepath.Join("Applications", "Ollama.app"),
		},
		{
			name: "plain unix path has no bundle",
			bin:  filepath.Join("usr", "local", "bin", "ollama"),
			want: "",
		},
		{
			name: "windows path has no bundle",
			bin:  filepath.Join("Program Files", "Ollama", "ollama.exe"),
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Anchor under a root so filepath.Dir terminates the same way on
			// every OS.
			root := t.TempDir()
			got := bundleRoot(filepath.Join(root, tc.bin))
			want := ""
			if tc.want != "" {
				want = filepath.Join(root, tc.want)
			}
			if got != want {
				t.Errorf("bundleRoot(%q) = %q, want %q", tc.bin, got, want)
			}
		})
	}
}

// TestGatherManagedFacts exercises the real filesystem observations, so the
// judgement above is fed by something that actually ran.
func TestGatherManagedFacts(t *testing.T) {
	appBin := filepath.Join("Applications", "Ollama.app", "Contents", "Resources", "ollama")

	for _, tc := range []struct {
		name string
		bin  string
		// files created relative to the temp root, before gathering.
		files []string
		// recordApp, when set, writes a state-dir record naming this
		// (root-relative) app path.
		recordApp     string
		wantFacts     ManagedFacts
		wantLegacySet bool
	}{
		{
			name:      "windows marker beside the binary",
			bin:       filepath.Join("Program Files", "Ollama", "ollama.exe"),
			files:     []string{filepath.Join("Program Files", "Ollama", WairedManagedMarkerName)},
			wantFacts: ManagedFacts{MarkerBesideBinary: true},
		},
		{
			name:      "bare install, no evidence",
			bin:       appBin,
			wantFacts: ManagedFacts{},
		},
		{
			name:          "legacy in-bundle marker is found and reported for repair",
			bin:           appBin,
			files:         []string{filepath.Join("Applications", "Ollama.app", WairedManagedMarkerName)},
			wantFacts:     ManagedFacts{LegacyBundleMarker: true},
			wantLegacySet: true,
		},
		{
			name:      "state-dir record naming this bundle",
			bin:       appBin,
			recordApp: filepath.Join("Applications", "Ollama.app"),
			wantFacts: ManagedFacts{StateRecordMatches: true},
		},
		{
			// A record left behind by a previous install does not make the
			// user's own replacement bundle ours.
			name:      "state-dir record naming a different bundle",
			bin:       appBin,
			recordApp: filepath.Join("Applications", "SomethingElse.app"),
			wantFacts: ManagedFacts{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			bin := filepath.Join(root, tc.bin)
			touch(t, bin)
			for _, f := range tc.files {
				touch(t, filepath.Join(root, f))
			}
			if tc.recordApp != "" {
				if err := WriteDarwinManagedRecord(stateDir,
					filepath.Join(root, tc.recordApp), DarwinManagedInstallerFresh); err != nil {
					t.Fatal(err)
				}
			}

			facts, legacy := gatherManagedFacts(stateDir, bin)
			if facts != tc.wantFacts {
				t.Errorf("facts = %+v, want %+v", facts, tc.wantFacts)
			}
			if (legacy != "") != tc.wantLegacySet {
				t.Errorf("legacy marker path = %q, want set=%v", legacy, tc.wantLegacySet)
			}
			if tc.wantLegacySet {
				if want := filepath.Join(root, "Applications", "Ollama.app", WairedManagedMarkerName); legacy != want {
					t.Errorf("legacy marker path = %q, want %q", legacy, want)
				}
			}
		})
	}
}

// TestWriteAndReadDarwinManagedRecord pins the round trip, including the
// MkdirAll: on a fresh host <stateDir>/runtimes/ollama/ does not exist yet.
func TestWriteAndReadDarwinManagedRecord(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	app := "/Applications/Ollama.app"

	if got := readDarwinManagedRecord(stateDir); got != "" {
		t.Fatalf("read before write = %q, want empty", got)
	}
	if err := WriteDarwinManagedRecord(stateDir, app, DarwinManagedInstallerFresh); err != nil {
		t.Fatal(err)
	}
	if got := readDarwinManagedRecord(stateDir); got != app {
		t.Errorf("read after write = %q, want %q", got, app)
	}
	// The record must not be anywhere near the bundle.
	if got := DarwinManagedRecordPath(stateDir); got != filepath.Join(stateDir, "runtimes", "ollama", "darwin-managed.json") {
		t.Errorf("record path = %q", got)
	}

	// A corrupt record degrades to "not ours" rather than erroring out.
	if err := os.WriteFile(DarwinManagedRecordPath(stateDir), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readDarwinManagedRecord(stateDir); got != "" {
		t.Errorf("read of corrupt record = %q, want empty", got)
	}
}

func TestWriteDarwinManagedRecord_NoStateDir(t *testing.T) {
	if err := WriteDarwinManagedRecord("", "/Applications/Ollama.app", DarwinManagedInstallerFresh); err == nil {
		t.Fatal("want an error when there is no state dir to write into")
	}
}

// TestRepairDarwinBundleMarker is a PRODUCT CONTRACT test: deleting the
// in-bundle marker is the whole repair, and managed-ness survives it.
func TestRepairDarwinBundleMarker(t *testing.T) {
	appRel := filepath.Join("Applications", "Ollama.app")
	binRel := filepath.Join(appRel, "Contents", "Resources", "ollama")

	t.Run("removes the marker and records the replacement", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		bin := filepath.Join(root, binRel)
		marker := filepath.Join(root, appRel, WairedManagedMarkerName)
		touch(t, bin)
		touch(t, marker)

		det := OllamaDetection{Installed: true, Path: bin, LegacyBundleMarkerPath: marker}
		changed, err := RepairDarwinBundleMarker("darwin", stateDir, det)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Error("changed = false, want true")
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("marker still present (stat err = %v)", err)
		}
		if got := readDarwinManagedRecord(stateDir); got != filepath.Join(root, appRel) {
			t.Errorf("state record = %q, want the bundle path", got)
		}

		// And the host now detects as managed with the marker gone.
		facts, legacy := gatherManagedFacts(stateDir, bin)
		if !managedFrom("darwin", facts) {
			t.Errorf("after repair the install is no longer recognised as ours: %+v", facts)
		}
		if legacy != "" {
			t.Errorf("legacy marker path = %q, want empty after repair", legacy)
		}
	})

	t.Run("idempotent: nothing to repair", func(t *testing.T) {
		root := t.TempDir()
		det := OllamaDetection{Installed: true, Path: filepath.Join(root, binRel)}
		changed, err := RepairDarwinBundleMarker("darwin", filepath.Join(root, "state"), det)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("changed = true, want false when there is no legacy marker")
		}
	})

	t.Run("never touches non-darwin hosts", func(t *testing.T) {
		root := t.TempDir()
		marker := filepath.Join(root, appRel, WairedManagedMarkerName)
		touch(t, marker)
		det := OllamaDetection{Installed: true, Path: filepath.Join(root, binRel), LegacyBundleMarkerPath: marker}

		for _, goos := range []string{"windows", "linux"} {
			changed, err := RepairDarwinBundleMarker(goos, filepath.Join(root, "state"), det)
			if err != nil || changed {
				t.Errorf("%s: changed=%v err=%v, want false/nil", goos, changed, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("%s: marker was removed on a non-darwin host", goos)
			}
		}
	})

	t.Run("reports the repair even when the record cannot be written", func(t *testing.T) {
		root := t.TempDir()
		bin := filepath.Join(root, binRel)
		marker := filepath.Join(root, appRel, WairedManagedMarkerName)
		touch(t, bin)
		touch(t, marker)
		// A regular file where the state dir should be: MkdirAll fails.
		stateDir := filepath.Join(root, "state")
		touch(t, stateDir)

		det := OllamaDetection{Installed: true, Path: bin, LegacyBundleMarkerPath: marker}
		changed, err := RepairDarwinBundleMarker("darwin", stateDir, det)
		if !changed {
			t.Error("changed = false; the bundle WAS repaired and callers must be told")
		}
		if err == nil {
			t.Error("want an error describing the failed record write")
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Error("the seal-breaking marker must be gone regardless of the record write")
		}
	})
}
