package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// rmRunner records the ollama argv it was asked to run and returns a
// configured error. It keeps the real arguments (binary, args, env) so a
// test can assert WHICH tag was removed — dropping them would make the
// "removed the wrong model" case unwritable.
type rmRunner struct {
	calls [][]string
	err   error
}

func (r *rmRunner) Run(_ context.Context, binary string, args, _ []string, _ func(string)) error {
	r.calls = append(r.calls, append([]string{binary}, args...))
	return r.err
}

func deleteModelProvider(t *testing.T, runner download.CommandRunner, models map[string]catalog.ModelState) (*agentInferenceProvider, *catalog.Store) {
	t.Helper()
	store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
	st, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	st.Models = models
	if err := store.Save(st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return &agentInferenceProvider{
		store:  store,
		logger: slog.Default(),
		puller: download.NewPuller("ollama", runner),
	}, store
}

// PRODUCT CONTRACT (waired-agent#641): answering "deleted" means the
// weights are gone. Before this, deletion dropped the state.json record
// and left the bytes — so `waired models rm` reported success, freed
// nothing, and a later rescan re-adopted the entry as ready.
func TestDeleteModel_RemovesTheWeights(t *testing.T) {
	r := &rmRunner{}
	p, store := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b": {OllamaTag: "qwen3.5:4b-q4_K_M", State: "ready"},
		"qwen3.5-2b": {OllamaTag: "qwen3.5:2b-q4_K_M", State: "ready"},
	})

	if err := p.DeleteModel(context.Background(), "qwen3.5-4b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("engine calls = %v, want exactly one removal", r.calls)
	}
	if got := r.calls[0]; got[1] != "rm" || got[2] != "qwen3.5:4b-q4_K_M" {
		t.Errorf("ran %v, want `ollama rm qwen3.5:4b-q4_K_M`", got)
	}

	st, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, still := st.Models["qwen3.5-4b"]; still {
		t.Error("the record survived a successful deletion")
	}
	if _, other := st.Models["qwen3.5-2b"]; !other {
		t.Error("deleting one model took another one's record with it")
	}
}

// A failed removal is not a deletion. Reporting success here is the
// defect itself in a new place: the operator reads a freed disk that is
// still full, and #641's rescan brings the "deleted" entry back — so the
// record is deliberately kept, which is what lets a retry work.
func TestDeleteModel_KeepsTheRecordWhenTheWeightsSurvive(t *testing.T) {
	r := &rmRunner{err: errors.New("engine unreachable")}
	p, store := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b": {OllamaTag: "qwen3.5:4b-q4_K_M", State: "ready"},
	})

	err := p.DeleteModel(context.Background(), "qwen3.5-4b")
	if err == nil {
		t.Fatal("a failed removal reported success")
	}
	st, loadErr := store.Load()
	if loadErr != nil {
		t.Fatalf("load: %v", loadErr)
	}
	if _, still := st.Models["qwen3.5-4b"]; !still {
		t.Error("the record was dropped while the weights stayed: nothing can name them now")
	}
}

// The shared-tag policy the original Phase A comment deferred. Several
// manifests can resolve to one engine tag; removing it for one of them
// would take the other's weights too, so the tag stays and only the
// record goes.
func TestDeleteModel_KeepsWeightsAnotherModelShares(t *testing.T) {
	r := &rmRunner{}
	p, store := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b":       {OllamaTag: "qwen3.5:4b-q4_K_M", State: "ready"},
		"qwen3.5-4b-alias": {OllamaTag: "qwen3.5:4b-q4_K_M", State: "ready"},
	})

	if err := p.DeleteModel(context.Background(), "qwen3.5-4b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("removed weights another model still needs: %v", r.calls)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, still := st.Models["qwen3.5-4b"]; still {
		t.Error("the record should still go — only the shared weights stay")
	}
	if _, kept := st.Models["qwen3.5-4b-alias"]; !kept {
		t.Error("the sharing model lost its record")
	}
}

func TestModelIDsForTag(t *testing.T) {
	models := map[string]catalog.ModelState{
		"a": {OllamaTag: "shared"},
		"b": {OllamaTag: "shared"},
		"c": {OllamaTag: "own"},
		"d": {},
	}
	for _, tc := range []struct {
		name, tag, except string
		want              []string
	}{
		{"two models on one tag", "shared", "a", []string{"b"}},
		{"the only model on its tag", "own", "c", nil},
		{"a tag nobody has", "gone", "a", nil},
		// Record of today's behaviour, not a contract: models with no
		// recorded weights would read as sharing the empty tag. DeleteModel
		// never asks — it guards on tag != "" before calling — and this row
		// exists so a future caller that drops that guard sees the answer
		// it would get.
		{"no tag recorded", "", "d", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := modelIDsForTag(models, tc.tag, tc.except)
			if len(got) != len(tc.want) {
				t.Fatalf("modelIDsForTag(%q, except %q) = %v, want %v", tc.tag, tc.except, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("modelIDsForTag(%q, except %q) = %v, want %v", tc.tag, tc.except, got, tc.want)
				}
			}
		})
	}
}
