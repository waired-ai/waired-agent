package catalog

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

const phaseAv1JSON = `{
  "version": 1,
  "models": {
    "qwen3-8b-instruct": {
      "variant_id": "q4-gguf",
      "ollama_tag": "qwen3:8b-q4_K_M",
      "state": "ready",
      "size_bytes": 5400000000,
      "pulled_at": "2026-05-02T10:30:00+09:00",
      "last_used_at": "2026-05-02T10:35:12+09:00"
    }
  },
  "endpoints": {
    "ep_local_ollama_qwen3_8b_instruct": {
      "runtime": "ollama",
      "model_id": "qwen3-8b-instruct",
      "variant_id": "q4-gguf",
      "state": "ready",
      "since": "2026-05-02T10:30:00+09:00"
    }
  }
}`

func unmarshalState(t *testing.T, body string) State {
	t.Helper()
	var st State
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return st
}

func TestMigrateInPlace_PhaseAPreservesActive(t *testing.T) {
	st := unmarshalState(t, phaseAv1JSON)
	fixedNow := time.Date(2026, 5, 3, 14, 0, 0, 0, time.FixedZone("JST", 9*3600))
	hw := HardwareSnapshot{
		GPUModel:       "NVIDIA RTX PRO 4000 Blackwell",
		GPUVRAMTotalMB: 24467,
		RAMTotalGB:     64,
	}

	rep := MigrateInPlace(&st, MigrationOpts{
		Now:      func() time.Time { return fixedNow },
		Hardware: hw,
	})

	if !rep.Migrated {
		t.Fatalf("expected Migrated=true for v1 input")
	}
	if rep.FromVersion != 1 || rep.ToVersion != StateVersion {
		t.Errorf("version transition = %d→%d, want 1→%d", rep.FromVersion, rep.ToVersion, StateVersion)
	}
	if rep.PreservedActive != "qwen3-8b-instruct" {
		t.Errorf("PreservedActive = %q, want qwen3-8b-instruct", rep.PreservedActive)
	}
	if st.Version != StateVersion {
		t.Errorf("st.Version = %d, want %d", st.Version, StateVersion)
	}
	if st.Active == nil {
		t.Fatalf("Active should be populated after migration")
	}
	if st.Active.Runtime != "ollama" || st.Active.ModelID != "qwen3-8b-instruct" || st.Active.VariantID != "q4-gguf" {
		t.Errorf("Active = %+v, want ollama/qwen3-8b-instruct/q4-gguf", st.Active)
	}
	if st.Active.DecidedBy != "migration" {
		t.Errorf("DecidedBy = %q, want migration", st.Active.DecidedBy)
	}
	if !st.Active.DecidedAt.Equal(fixedNow) {
		t.Errorf("DecidedAt = %v, want %v", st.Active.DecidedAt, fixedNow)
	}
	if st.Active.DecisionHardwareSnapshot != hw {
		t.Errorf("HardwareSnapshot = %+v, want %+v", st.Active.DecisionHardwareSnapshot, hw)
	}
	if _, ok := st.Runtimes["ollama"]; !ok {
		t.Errorf("Runtimes should include inferred ollama entry, got %+v", st.Runtimes)
	}

	// Phase A model entries must survive verbatim.
	if got := st.Models["qwen3-8b-instruct"]; got.OllamaTag != "qwen3:8b-q4_K_M" || got.State != ModelStateReady {
		t.Errorf("Phase A model not preserved: %+v", got)
	}
}

func TestMigrateInPlace_NoReadyModel(t *testing.T) {
	v1 := `{
		"version": 1,
		"models": {
			"some-id": {"variant_id": "v", "state": "downloading"}
		}
	}`
	st := unmarshalState(t, v1)
	rep := MigrateInPlace(&st, MigrationOpts{})

	if !rep.Migrated {
		t.Errorf("expected Migrated=true")
	}
	if rep.PreservedActive != "" {
		t.Errorf("PreservedActive = %q, want empty (no Ready entries)", rep.PreservedActive)
	}
	if st.Active != nil {
		t.Errorf("Active should be nil when no ready model exists, got %+v", st.Active)
	}
	if len(st.Runtimes) != 0 {
		t.Errorf("Runtimes should be empty when no Active inferred, got %+v", st.Runtimes)
	}
	if len(rep.Notes) == 0 {
		t.Errorf("expected a note explaining why Active was left unset")
	}
}

func TestMigrateInPlace_MultipleReadyPicksMostRecent(t *testing.T) {
	v1 := `{
		"version": 1,
		"models": {
			"old-model": {"variant_id": "v1", "state": "ready", "last_used_at": "2026-05-01T00:00:00Z"},
			"new-model": {"variant_id": "v2", "state": "ready", "last_used_at": "2026-05-02T00:00:00Z"}
		}
	}`
	st := unmarshalState(t, v1)
	rep := MigrateInPlace(&st, MigrationOpts{})
	if rep.PreservedActive != "new-model" {
		t.Errorf("PreservedActive = %q, want new-model (most-recent last_used_at)", rep.PreservedActive)
	}
}

func TestMigrateInPlace_TieBrokenAlphabetically(t *testing.T) {
	// Both ready, identical (zero) last_used_at — alphabetic order
	// breaks the tie deterministically so test runs are reproducible.
	v1 := `{
		"version": 1,
		"models": {
			"bbb-model": {"variant_id": "v", "state": "ready"},
			"aaa-model": {"variant_id": "v", "state": "ready"}
		}
	}`
	st := unmarshalState(t, v1)
	rep := MigrateInPlace(&st, MigrationOpts{})
	if rep.PreservedActive != "aaa-model" {
		t.Errorf("PreservedActive = %q, want aaa-model (alphabetic tiebreak)", rep.PreservedActive)
	}
}

func TestMigrateInPlace_AlreadyV2Idempotent(t *testing.T) {
	v2 := `{
		"version": 2,
		"active": {
			"runtime": "vllm",
			"model_id": "qwen3-32b-instruct",
			"variant_id": "awq-int4",
			"decided_by": "user"
		},
		"models": {},
		"endpoints": {},
		"runtimes": {
			"vllm": {"version": "0.11.0"}
		}
	}`
	st := unmarshalState(t, v2)
	before := st
	rep := MigrateInPlace(&st, MigrationOpts{})

	if rep.Migrated {
		t.Errorf("expected Migrated=false for v2 input")
	}
	if st.Version != 2 {
		t.Errorf("Version = %d, want 2", st.Version)
	}
	if st.Active == nil || st.Active.ModelID != "qwen3-32b-instruct" {
		t.Errorf("v2 Active corrupted: %+v", st.Active)
	}
	if before.Active.DecidedBy != st.Active.DecidedBy {
		t.Errorf("DecidedBy mutated by no-op migration")
	}
}

func TestMigrateInPlace_NormalisesNilMaps(t *testing.T) {
	// State with no maps should still be safe to iterate after Load.
	st := State{Version: 0}
	MigrateInPlace(&st, MigrationOpts{})
	if st.Models == nil {
		t.Errorf("Models map should be non-nil after migration")
	}
	if st.Endpoints == nil {
		t.Errorf("Endpoints map should be non-nil after migration")
	}
	if st.Runtimes == nil {
		t.Errorf("Runtimes map should be non-nil after migration")
	}
}

func TestStore_LoadRunsMigration(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	if err := writeFile(path, []byte(phaseAv1JSON)); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	s := NewStore(path)
	st, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Step 5 wires migration into Load (next change in this commit).
	if st.Version != StateVersion {
		t.Errorf("post-Load Version = %d, want %d (migration must run on read)", st.Version, StateVersion)
	}
	if st.Active == nil || st.Active.ModelID != "qwen3-8b-instruct" {
		t.Errorf("post-Load Active = %+v, want qwen3-8b-instruct", st.Active)
	}
}
