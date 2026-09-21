package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// vllmVenvKeeper knows which vLLM venvs this daemon is using, and reclaims
// the ones nothing uses (waired-agent#1431).
//
// Every vLLM build goes into a directory of its own and the old one stays
// until something removes it (VLLMInstaller.Install). The converge used to
// remove it right after swapping `current` — under the engine still
// running from it. Python loads files lazily, so that engine failed its
// next request with FileNotFoundError on a flashinfer template it had not
// opened yet (HTTP 500, no recovery until a restart); and an adapter whose
// start was still retrying respawned from the deleted interpreter. Which
// venvs are in use is something only this process knows: it starts the
// engine, the host-speed probe and the weight downloads, each from the
// venv `current` named when it resolved it. So the daemon is the one that
// reclaims, and it keeps whatever it holds.
//
// hold is how every one of those resolves the venv: it reads `current` and
// marks that directory in use in one step, under the same mutex reclaim
// decides under, so nothing can be resolved and reclaimed at once. reclaim
// also takes the installer's cross-process lock, so it never runs between
// another process claiming a directory for a build and activating it.
type vllmVenvKeeper struct {
	active func() (infruntime.InstallResult, bool)
	prune  func(inUse map[string]bool) ([]string, error)
	lock   func(ctx context.Context) (func(), error)

	mu   sync.Mutex
	refs map[string]int
}

// vllmReclaimLockWait bounds how long a reclaim waits for an install
// another process is running (the CLI's, on the apt path) before it gives
// up until the next chance.
const vllmReclaimLockWait = 30 * time.Minute

func newVLLMVenvKeeper(stateDir string) *vllmVenvKeeper {
	inst := infruntime.NewVLLMInstallerAt(filepath.Join(stateDir, "runtimes", "vllm"))
	return &vllmVenvKeeper{
		active: inst.Active,
		prune:  inst.PruneUnused,
		lock:   func(ctx context.Context) (func(), error) { return inst.Lock(ctx, nil) },
		refs:   map[string]int{},
	}
}

// hold resolves the active venv and marks it in use until release is
// called; release is safe to call more than once. ok=false when no venv is
// active, and release is then a no-op.
func (k *vllmVenvKeeper) hold() (venv infruntime.InstallResult, release func(), ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	venv, ok = k.active()
	if !ok {
		return venv, func() {}, false
	}
	k.refs[venv.Dir]++
	var once sync.Once
	return venv, func() {
		once.Do(func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.refs[venv.Dir]--; k.refs[venv.Dir] <= 0 {
				delete(k.refs, venv.Dir)
			}
		})
	}, true
}

// inUseLocked lists the directories held right now. Callers hold k.mu.
func (k *vllmVenvKeeper) inUseLocked() map[string]bool {
	out := make(map[string]bool, len(k.refs))
	for dir := range k.refs {
		out[dir] = true
	}
	return out
}

// reclaim removes every venv nothing uses and says what it kept. Never
// fails the caller: a venv it could not remove now is removed by a later
// pass.
func (k *vllmVenvKeeper) reclaim(ctx context.Context, logger *slog.Logger) {
	if _, ok := k.active(); !ok {
		return // nothing installed, so nothing to reclaim
	}
	lctx, cancel := context.WithTimeout(ctx, vllmReclaimLockWait)
	defer cancel()
	unlock, err := k.lock(lctx)
	if err != nil {
		logger.Warn("did not reclaim superseded vLLM venvs this time", "err", err)
		return
	}
	defer unlock()

	k.mu.Lock()
	defer k.mu.Unlock()
	inUse := k.inUseLocked()
	removed, err := k.prune(inUse)
	if len(removed) > 0 {
		logger.Info("removed the superseded vLLM venv(s)", "dirs", removed)
	}
	if current, ok := k.active(); ok {
		var kept []string
		for dir := range inUse {
			if dir != current.Dir {
				kept = append(kept, dir)
			}
		}
		if len(kept) > 0 {
			sort.Strings(kept)
			logger.Info("kept a superseded vLLM venv that is still in use; it is removed once nothing uses it",
				"dirs", kept, "current", current.Dir)
		}
	}
	if err != nil {
		logger.Warn("could not remove a superseded vLLM venv; it stays on disk", "err", err)
	}
}

// vllmVenvKeeper is the provider's keeper, built on first use for a provider
// constructed without the one startInferenceSubsystem shares with the
// start-up converge (tests).
func (p *agentInferenceProvider) vllmVenvKeeper() *vllmVenvKeeper {
	p.vllmVenvsOnce.Do(func() {
		if p.vllmVenvs == nil {
			p.vllmVenvs = newVLLMVenvKeeper(p.stateDir)
		}
	})
	return p.vllmVenvs
}

// holdVLLMVenvForAdapter makes dir the venv the registered vLLM adapter
// holds, and lets go of the one the adapter it replaced held.
func (p *agentInferenceProvider) holdVLLMVenvForAdapter(dir string, release func()) {
	p.vllmHeldMu.Lock()
	prev := p.vllmHeldRelease
	p.vllmHeldDir, p.vllmHeldRelease = dir, release
	p.vllmHeldMu.Unlock()
	if prev != nil {
		prev()
	}
}

// vllmAdapterVenvVersion is the vLLM release the registered adapter spawns
// from, or "" when there is none.
func (p *agentInferenceProvider) vllmAdapterVenvVersion() string {
	p.vllmHeldMu.Lock()
	defer p.vllmHeldMu.Unlock()
	if p.vllmHeldDir == "" {
		return ""
	}
	return infruntime.VLLMVersionOfDir(p.vllmHeldDir)
}
