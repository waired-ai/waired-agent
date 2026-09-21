package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

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
