package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// writeHFFile creates dir/rel with n bytes, making parents.
func writeHFFile(t *testing.T, dir, rel string, n int) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A second download into the same directory waits for the first to let go,
// and one whose context ends while waiting gives up rather than hanging
// (waired-agent#1519). Record of today's behaviour.
func TestHFDirLocks_OneHolderPerDirectory(t *testing.T) {
	var l hfDirLocks
	release, err := l.acquire(context.Background(), "/d", nil)
	if err != nil {
		t.Fatal(err)
	}

	// A different directory is not blocked.
	other, err := l.acquire(context.Background(), "/e", nil)
	if err != nil {
		t.Fatalf("another directory: %v", err)
	}
	other()

	waits := 0
	got := make(chan error, 1)
	go func() {
		r, err := l.acquire(context.Background(), "/d", func() { waits++ })
		if err == nil {
			r()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the second holder got the directory while the first held it (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second acquire: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never got the directory after release")
	}
	if waits != 1 {
		t.Errorf("onWait ran %d times, want once", waits)
	}

	hold, _ := l.acquire(context.Background(), "/d", nil)
	defer hold()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.acquire(ctx, "/d", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("acquire on a cancelled context = %v, want context.Canceled", err)
	}
}

// DeleteModel removes only what sits directly under the agent's model
// directories: a record is data, and a path in it must not turn `waired
// models rm` into a delete anywhere else on the disk.
func TestHFDirOwnedBy(t *testing.T) {
	state := filepath.Join(string(filepath.Separator), "var", "lib", "waired")
	root := hfModelsRoot(state)
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{filepath.Join(root, "Qwen__Qwen3.5-0.8B"), true},
		{filepath.Join(root, "Qwen__Qwen3.5-0.8B") + string(filepath.Separator), true},
		{root, false},
		{filepath.Join(root, "a", "b"), false},
		{filepath.Join(root, "..", "x"), false},
		{filepath.Join(state, "models", "x"), false},
		{filepath.Join(string(filepath.Separator), "home", "someone"), false},
		{"", false},
	} {
		if got := hfDirOwnedBy(state, c.dir); got != c.want {
			t.Errorf("hfDirOwnedBy(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}

// PRODUCT CONTRACT (waired-agent#1519): a service stop kills `hf download`
// before it can delete its own partial file, so the next start clears them.
// On the host that found this, three interrupted attempts had left 17.8 GB.
func TestSweepHFPartialsAtBoot_ClearsEveryModelDirectory(t *testing.T) {
	state := t.TempDir()
	a := filepath.Join(hfModelsRoot(state), "org__a")
	b := filepath.Join(hfModelsRoot(state), "org__b")
	pa := writeHFFile(t, a, ".cache/huggingface/download/x.etag.1a2b3c4d.incomplete", 10)
	pb := writeHFFile(t, b, ".cache/huggingface/download/original/y.etag.5e6f7a8b.incomplete", 20)
	meta := writeHFFile(t, b, ".cache/huggingface/download/z.metadata", 1)
	landed := writeHFFile(t, a, "model.safetensors", 30)

	sweepHFPartialsAtBoot(slog.New(slog.NewTextHandler(io.Discard, nil)), state)

	if exists(pa) || exists(pb) {
		t.Errorf("partials survived the boot sweep: a=%v b=%v", exists(pa), exists(pb))
	}
	if !exists(meta) || !exists(landed) {
		t.Errorf("the sweep removed more than partials: metadata=%v weights=%v", exists(meta), exists(landed))
	}

	// A host with no model directory at all is the common case.
	sweepHFPartialsAtBoot(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
}

func hfDeleteProvider(t *testing.T, models map[string]catalog.ModelState) (*agentInferenceProvider, *catalog.Store, string) {
	t.Helper()
	p, store := deleteModelProvider(t, &rmRunner{}, models)
	p.stateDir = t.TempDir()
	return p, store, hfModelsRoot(p.stateDir)
}

// PRODUCT CONTRACT (waired-agent#641, extended to vLLM by #1519): answering
// "deleted" means the weights are gone. A vLLM model's weights are its
// directory, which `waired models rm` used to leave behind in full.
func TestDeleteModel_RemovesAVLLMModelsDirectory(t *testing.T) {
	p, store, root := hfDeleteProvider(t, nil)
	dir := filepath.Join(root, "Qwen__Qwen3.5-9B")
	writeHFFile(t, dir, "model.safetensors", 64)
	seedVLLMModels(t, store, map[string]catalog.ModelState{
		"qwen3.5-9b": {VariantID: "bf16", HFRepo: "Qwen/Qwen3.5-9B", LocalPath: dir, State: catalog.ModelStateReady},
	})

	if err := p.DeleteModel(context.Background(), "qwen3.5-9b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if exists(dir) {
		t.Error("the model's directory survived `models rm`")
	}
	st, _ := store.Load()
	if _, still := st.VLLMModels["qwen3.5-9b"]; still {
		t.Error("the record survived a successful deletion")
	}
}

// `waired models rm` names a model, not an engine (waired-agent#1520): with
// both engines holding it, both engines' weights and records go.
func TestDeleteModel_RemovesEveryEnginesCopy(t *testing.T) {
	r := &rmRunner{}
	p, store := deleteModelProvider(t, r, map[string]catalog.ModelState{
		"qwen3.5-4b": {VariantID: "q8", OllamaTag: "qwen3.5:4b-q8_0", State: catalog.ModelStateReady},
	})
	p.stateDir = t.TempDir()
	dir := filepath.Join(hfModelsRoot(p.stateDir), "Qwen__Qwen3.5-4B")
	writeHFFile(t, dir, "model.safetensors", 64)
	seedVLLMModels(t, store, map[string]catalog.ModelState{
		"qwen3.5-4b": {VariantID: "bf16", HFRepo: "Qwen/Qwen3.5-4B", LocalPath: dir, State: catalog.ModelStateReady},
	})

	if err := p.DeleteModel(context.Background(), "qwen3.5-4b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0][1] != "rm" || r.calls[0][2] != "qwen3.5:4b-q8_0" {
		t.Errorf("engine calls = %v, want `ollama rm qwen3.5:4b-q8_0`", r.calls)
	}
	if exists(dir) {
		t.Error("vLLM's directory survived `models rm`")
	}
	st, _ := store.Load()
	if len(st.RecordsFor("qwen3.5-4b")) != 0 {
		t.Errorf("records left after the deletion: %+v", st.RecordsFor("qwen3.5-4b"))
	}
}

// The directory is named after the repository, so two model ids can name
// one; removing it for one would take the other's weights.
func TestDeleteModel_KeepsAVLLMDirectoryAnotherModelNames(t *testing.T) {
	p, store, root := hfDeleteProvider(t, nil)
	dir := filepath.Join(root, "org__shared")
	writeHFFile(t, dir, "model.safetensors", 64)
	seedVLLMModels(t, store, map[string]catalog.ModelState{
		"model-a": {VariantID: "bf16", LocalPath: dir, State: catalog.ModelStateReady},
		"model-b": {VariantID: "bf16", LocalPath: dir + string(filepath.Separator), State: catalog.ModelStateReady},
	})

	if err := p.DeleteModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !exists(dir) {
		t.Error("removed a directory another model's record still names")
	}
	st, _ := store.Load()
	if _, still := st.VLLMModels["model-a"]; still {
		t.Error("the record should still go; only the shared weights stay")
	}
	if _, kept := st.VLLMModels["model-b"]; !kept {
		t.Error("the sharing model lost its record")
	}
}

// A record that names a path outside the model directories loses its
// record and nothing else.
func TestDeleteModel_LeavesAPathOutsideTheModelDirectories(t *testing.T) {
	p, store, _ := hfDeleteProvider(t, nil)
	elsewhere := t.TempDir()
	keep := writeHFFile(t, elsewhere, "precious", 8)
	seedVLLMModels(t, store, map[string]catalog.ModelState{
		"odd": {VariantID: "bf16", LocalPath: elsewhere, State: catalog.ModelStateReady},
	})

	if err := p.DeleteModel(context.Background(), "odd"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !exists(keep) {
		t.Error("`models rm` deleted a path outside the agent's model directories")
	}
}

// seedVLLMModels records models as vLLM's: a model directory is where the
// vLLM engine keeps what it fetched (waired-agent#1520).
func seedVLLMModels(t *testing.T, store *catalog.Store, models map[string]catalog.ModelState) {
	t.Helper()
	if err := store.Update(func(s *catalog.State) {
		for id, m := range models {
			s.SetModel(catalog.RuntimeVLLM, id, m)
		}
	}); err != nil {
		t.Fatal(err)
	}
}
