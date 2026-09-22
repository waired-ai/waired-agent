package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// Untagged, though only a vLLM host ever has a Hugging Face model directory:
// the boot sweep and DeleteModel read the directory layout on every OS, and on
// a host without one the directory is simply absent.

// hfModelsRoot is the directory every Hugging Face model directory sits in.
func hfModelsRoot(stateDir string) string {
	return filepath.Join(stateDir, "models", "hf")
}

// hfDirLocks hands out one Hugging Face model directory at a time.
//
// Two downloads can target the same directory: the directory is named after
// the repository, not the model, and the host-speed probe downloads its model
// outside the pull registry (host_cutoff_vllm_linux.go). A partial file of a
// live download looks exactly like one a killed download left, so the sweep
// that clears the second kind may run only while nothing else writes there.
// Holding the directory for the whole download also stops two `hf download`
// processes from fetching the same bytes twice.
type hfDirLocks struct {
	mu   sync.Mutex
	held map[string]chan struct{}
}

// acquire waits for dir to be free, or for ctx to end. onWait, when non-nil,
// runs once if it has to wait. The returned release must be called exactly
// once.
func (l *hfDirLocks) acquire(ctx context.Context, dir string, onWait func()) (release func(), err error) {
	for {
		l.mu.Lock()
		if l.held == nil {
			l.held = make(map[string]chan struct{})
		}
		busy, ok := l.held[dir]
		if !ok {
			done := make(chan struct{})
			l.held[dir] = done
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				delete(l.held, dir)
				l.mu.Unlock()
				close(done)
			}, nil
		}
		l.mu.Unlock()
		if onWait != nil {
			onWait()
			onWait = nil
		}
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// sweepHFPartials deletes the partial files in localDir and logs what that
// freed. The caller holds localDir (hfDirLocks) or knows no download can be
// running, as at boot.
func sweepHFPartials(logger *slog.Logger, localDir, when string) {
	removed, bytes, err := download.SweepHFIncomplete(localDir)
	if err != nil {
		logger.Warn("removing partial download files failed; they stay on disk",
			"dir", localDir, "when", when, "removed", removed, "err", err)
		return
	}
	if removed > 0 {
		logger.Info("removed partial download files a stopped download left behind",
			"dir", localDir, "when", when, "files", removed, "bytes", bytes)
	}
}

// sweepHFPartialsAtBoot clears every Hugging Face model directory of the
// partials a previous run left: a service stop kills `hf download` without
// letting it delete its own, and so does a crash (waired-agent#1519). Runs
// before the provider exists, so no download of this process can be writing.
func sweepHFPartialsAtBoot(logger *slog.Logger, stateDir string) {
	entries, err := os.ReadDir(hfModelsRoot(stateDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sweepHFPartials(logger, filepath.Join(hfModelsRoot(stateDir), e.Name()), "boot")
	}
}

// removeHFModelDir deletes a vLLM model's weights directory on behalf of
// DeleteModel. It keeps the directory when another record still names it,
// and leaves alone a path that is not one of the agent's model directories.
//
// The sharing check runs with the directory held, against a fresh read of the
// catalog: a download for another model id into the same repository directory
// may be finishing, and its record is what says the bytes are wanted.
func (p *agentInferenceProvider) removeHFModelDir(ctx context.Context, modelID, dir string) error {
	if !hfDirOwnedBy(p.stateDir, dir) {
		p.logger.Warn("the model record names weights outside the model directory; leaving them",
			"model", modelID, "dir", dir)
		return nil
	}
	release, err := p.hfDirs.acquire(ctx, dir, func() {
		p.logger.Info("waiting for a download into this model's directory before deleting it",
			"model", modelID, "dir", dir)
	})
	if err != nil {
		return err
	}
	defer release()
	st, err := p.store.Load()
	if err != nil {
		return err
	}
	if shared := modelIDsForDir(st.VLLMModels, dir, modelID); len(shared) > 0 {
		p.logger.Info("model record removed; weights kept, another model names the same directory",
			"model", modelID, "dir", dir, "shared_with", shared)
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	p.logger.Info("model weights deleted", "model", modelID, "dir", dir)
	return nil
}

// hfDirOwnedBy reports whether dir is a model directory this agent manages:
// directly under hfModelsRoot. DeleteModel removes a directory only when this
// holds, so a record naming any other path cannot turn `waired models rm`
// into a delete somewhere else on the disk.
func hfDirOwnedBy(stateDir, dir string) bool {
	if dir == "" {
		return false
	}
	root := filepath.Clean(hfModelsRoot(stateDir))
	clean := filepath.Clean(dir)
	return filepath.Dir(clean) == root && filepath.Base(clean) != "." && filepath.Base(clean) != ".."
}

// hfLocalDir is the on-disk directory the safetensors for a model land in.
// The repo id's "/" is flattened to "__" so the whole repo maps to a single
// directory under hfModelsRoot without nesting or traversal risk.
//
// A custom model's directory also names its commit: an import pins one, and
// two imports of one repository at different commits are two models
// (waired-ai/waired#1473 ruling 2) that must not write into — or delete —
// each other's weights (waired-ai/waired#1480). A bundled build keeps the
// directory it always had, so its weights are not downloaded again.
func (p *agentInferenceProvider) hfLocalDir(modelID string, v catalog.Variant) string {
	name := strings.ReplaceAll(v.Source.RepoID, "/", "__")
	if catalog.IsCustomModelID(modelID) && len(v.Source.Revision) >= 12 {
		name += "@" + v.Source.Revision[:12]
	}
	return filepath.Join(hfModelsRoot(p.stateDir), name)
}

// derivedHFDir is where a model's weights would be for its vLLM record when
// the record never got as far as naming them: a download that failed, was
// stopped or never started records no LocalPath, and the shards it finished
// stay in this directory (waired-ai/waired#1480). "" when the model or its
// build is not in the catalog, or nothing is on disk there.
func (p *agentInferenceProvider) derivedHFDir(modelID string, rec catalog.ModelState) string {
	for _, m := range p.catalogManifests() {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.Source.RepoID == "" || (rec.VariantID != "" && v.VariantID != rec.VariantID) {
				continue
			}
			if dir := p.hfLocalDir(modelID, v); dirExists(dir) {
				return dir
			}
		}
	}
	return ""
}
