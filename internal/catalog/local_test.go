package catalog

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestStore_LoadEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "state.json"))
	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if st.Version != StateVersion {
		t.Errorf("Version on empty load = %d, want %d", st.Version, StateVersion)
	}
	if st.Models == nil || st.Endpoints == nil {
		t.Errorf("maps must be non-nil after Load: %+v", st)
	}
	if len(st.Models) != 0 || len(st.Endpoints) != 0 {
		t.Errorf("maps must be empty initially: %+v", st)
	}
}

func TestStore_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := NewStore(path)
	now := time.Date(2026, 5, 2, 17, 30, 0, 0, time.UTC)
	st := State{
		Version: StateVersion,
		Models: map[string]ModelState{
			"qwen3-8b-instruct": {
				VariantID: "q4-gguf",
				OllamaTag: "qwen3:8b-q4_K_M",
				State:     ModelStateReady,
				SizeBytes: 5_400_000_000,
				PulledAt:  now,
			},
		},
		Endpoints: map[string]EndpointState{
			"ep_local_ollama_qwen3_8b_instruct": {
				Runtime:   RuntimeOllama,
				ModelID:   "qwen3-8b-instruct",
				VariantID: "q4-gguf",
				State:     "ready",
				Since:     now,
			},
		},
	}
	if err := s.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File should exist with 0600 perms (Unix). Windows ignores Go's
	// file-mode bits and reports 0o666; permission enforcement comes
	// from the NTFS ACL on the parent directory.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("state.json mode = %o, want 600", mode)
		}
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Models["qwen3-8b-instruct"].OllamaTag != "qwen3:8b-q4_K_M" {
		t.Errorf("round-trip lost data: %+v", got)
	}
	if !got.Models["qwen3-8b-instruct"].PulledAt.Equal(now) {
		t.Errorf("PulledAt round-trip: %v vs %v", got.Models["qwen3-8b-instruct"].PulledAt, now)
	}
}

func TestStore_AtomicWrite(t *testing.T) {
	// After a successful Save, no `*.tmp` debris remains in the
	// state directory.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := NewStore(path)
	if err := s.Save(State{Version: StateVersion, Models: map[string]ModelState{}, Endpoints: map[string]EndpointState{}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}
}

func TestStore_Update(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "state.json"))
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Update(func(st *State) {
		st.Models["qwen3-8b-instruct"] = ModelState{
			VariantID: "q4-gguf",
			OllamaTag: "qwen3:8b-q4_K_M",
			State:     ModelStateDownloading,
			PulledAt:  now,
		}
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Models["qwen3-8b-instruct"].State != ModelStateDownloading {
		t.Errorf("Update didn't persist: %+v", st)
	}
}

func TestStore_UpdateConcurrent(t *testing.T) {
	// Sequential serialisation: 100 concurrent Update calls should all
	// land without dropping any (counted via a marker counter on a
	// shared model entry).
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "state.json"))
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_ = s.Update(func(st *State) {
				m := st.Models["counter"]
				m.SizeBytes++
				st.Models["counter"] = m
			})
		}()
	}
	wg.Wait()
	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.Models["counter"].SizeBytes; got != int64(N) {
		t.Errorf("counter = %d, want %d (some Updates were lost)", got, N)
	}
}

func TestDefaultStatePath(t *testing.T) {
	// DefaultStatePath delegates to platform/paths.StateDir; the only
	// thing we promise is that it appends "inference/state.json" to
	// whatever the platform resolver returns. Use $WAIRED_STATE_DIR
	// (the override path that bypasses every OS-specific branch) to
	// make the test portable across Linux / macOS / Windows.
	dir := t.TempDir()
	t.Setenv("WAIRED_STATE_DIR", dir)
	got := DefaultStatePath()
	want := filepath.Join(dir, "inference", "state.json")
	if got != want {
		t.Errorf("DefaultStatePath = %q, want %q", got, want)
	}
}

func TestModelStateConstants(t *testing.T) {
	// Sanity check spec §9.3 enum stays stringly-stable.
	cases := map[string]string{
		ModelStateNotPresent:  "not_present",
		ModelStateQueued:      "queued",
		ModelStateDownloading: "downloading",
		ModelStateVerifying:   "verifying",
		ModelStateReady:       "ready",
		ModelStateFailed:      "failed",
		ModelStateEvicted:     "evicted",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("constant value %q != %q", got, want)
		}
	}
}
