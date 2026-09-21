package main

import (
	"os"
	"sync"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
)

// How a vLLM host moves to the model chosen for it (waired-agent#1515).
//
// The chosen model's weights used to be fetched INSIDE the engine start:
// bootstrapVLLM downloaded them before it spawned anything, so nothing
// answered for the whole download (23 GB, on a real host), the download was
// not in the in-flight registry and could not be cancelled, and the old
// model was never started. Owner decision 2026-09-21: a vLLM host keeps
// answering with the model it was running until the new one's weights are
// on disk, as an ollama host does, and switches then.
//
// A vLLM engine serves one model per process, so the switch itself is a
// restart: drained first, minutes with nothing answering locally while the
// new one loads (the mesh takes those turns).

// vllmTargetAction is what the bootstrap does about the chosen model.
type vllmTargetAction string

const (
	// vllmKeep: leave the running engine alone.
	vllmKeep vllmTargetAction = "keep"
	// vllmStartTarget: nothing is up; spawn the chosen model.
	vllmStartTarget vllmTargetAction = "start_target"
	// vllmSwitchToTarget: another model is up; drain, stop it, spawn the
	// chosen one.
	vllmSwitchToTarget vllmTargetAction = "switch"
	// vllmStartPrevious: nothing is up and the chosen model is not on disk;
	// spawn the model this host was running, to answer until it is.
	vllmStartPrevious vllmTargetAction = "start_previous"
	// vllmWait: nothing to start until the chosen model's weights land.
	vllmWait vllmTargetAction = "wait"
	// vllmRefuseNoPull: the weights are absent, downloads are turned off,
	// and there is nothing else to run.
	vllmRefuseNoPull vllmTargetAction = "refuse_no_pull"
)

// vllmTargetFacts is everything the plan is made from.
type vllmTargetFacts struct {
	// EngineUp: an adapter is registered and ready or on its way up.
	EngineUp bool
	// ServingModel / ServingVariant: what that adapter serves.
	ServingModel, ServingVariant string
	// TargetModel / TargetVariant: the chosen model and the build of it
	// this engine will load.
	TargetModel, TargetVariant string
	// TargetOnDisk: that build's weights are on disk and Ready.
	TargetOnDisk bool
	// TargetDownloading: a download of the target is in flight.
	TargetDownloading bool
	// AllowPull is inference.allow_pull.
	AllowPull bool
	// AlreadyDispatched: this process already started the target's download
	// once. A person who cancelled it is not overruled by the next trigger;
	// a new choice, an explicit pull or the next daemon start asks again.
	AlreadyDispatched bool
	// HasPrevious: the model this host was running qualifies to answer in
	// the meantime (vllmPreviousCandidate).
	HasPrevious bool
}

// vllmTargetPlan is the answer: an action, and whether to start the
// target's download alongside it.
type vllmTargetPlan struct {
	Action   vllmTargetAction
	Download bool
}

// planVLLMTarget is the whole rule, untagged so it is table-tested on every
// leg.
func planVLLMTarget(f vllmTargetFacts) vllmTargetPlan {
	if f.TargetOnDisk {
		switch {
		case f.EngineUp && f.ServingModel == f.TargetModel && f.ServingVariant == f.TargetVariant:
			return vllmTargetPlan{Action: vllmKeep}
		case f.EngineUp:
			return vllmTargetPlan{Action: vllmSwitchToTarget}
		default:
			return vllmTargetPlan{Action: vllmStartTarget}
		}
	}
	download := f.AllowPull && !f.TargetDownloading && !f.AlreadyDispatched
	switch {
	case f.EngineUp:
		// Whatever it serves keeps answering until the weights land.
		return vllmTargetPlan{Action: vllmKeep, Download: download}
	case f.HasPrevious:
		return vllmTargetPlan{Action: vllmStartPrevious, Download: download}
	case !f.AllowPull && !f.TargetDownloading:
		return vllmTargetPlan{Action: vllmRefuseNoPull}
	default:
		return vllmTargetPlan{Action: vllmWait, Download: download}
	}
}

// vllmPreviousCandidate is the model this host was running, when it may
// answer while the chosen model downloads: state.json's Active, on vLLM,
// not the chosen model, still in this build's catalog under exactly that
// id, loadable by this engine version, with its weights Ready on disk.
//
// A retired model never qualifies (owner decision 2026-09-21). Its manifest
// is gone, so there is nothing to start it with — no context window, KV
// figures or tool parser — and no Waired request asks for a model the
// catalog no longer names: it would hold the card and answer nothing. The
// exact-id lookup is what enforces that; resolving the name would find a
// retired model's successor, whose weights are not the ones on disk.
func vllmPreviousCandidate(active *catalog.ActiveSelection, manifests []catalog.Manifest, st catalog.State,
	target, engineVersion string, dirExists func(string) bool) (catalog.Manifest, catalog.Variant, string, bool) {
	if active == nil || active.Runtime != catalog.RuntimeVLLM || active.ModelID == "" || active.ModelID == target {
		return catalog.Manifest{}, catalog.Variant{}, "", false
	}
	for _, m := range manifests {
		if m.ModelID != active.ModelID {
			continue
		}
		v, ok := variantByID(m, active.VariantID)
		if !ok || !router.VariantLoadable(v, catalog.RuntimeVLLM, engineVersion) {
			return catalog.Manifest{}, catalog.Variant{}, "", false
		}
		ms := st.Models[m.ModelID]
		if ms.State != catalog.ModelStateReady || ms.VariantID != active.VariantID || ms.LocalPath == "" || !dirExists(ms.LocalPath) {
			return catalog.Manifest{}, catalog.Variant{}, "", false
		}
		return m, v, ms.LocalPath, true
	}
	return catalog.Manifest{}, catalog.Variant{}, "", false
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// vllmServingModel is the model the registered vLLM adapter serves.
type vllmServingModel struct {
	ModelID, VariantID string
}

// vllmDispatched remembers the builds whose download the bootstrap started
// in this process (planVLLMTarget's AlreadyDispatched).
type vllmDispatched struct {
	mu   sync.Mutex
	keys map[string]bool
}

func (d *vllmDispatched) seen(model, variant string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.keys[model+"/"+variant]
}

func (d *vllmDispatched) mark(model, variant string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.keys == nil {
		d.keys = map[string]bool{}
	}
	d.keys[model+"/"+variant] = true
}

// forget clears the record for a model, so a new choice of it, or an
// explicit pull, asks for the download again.
func (d *vllmDispatched) forget(model string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.keys {
		if len(k) > len(model) && k[:len(model)+1] == model+"/" {
			delete(d.keys, k)
		}
	}
}

// vllmChosenAbsent reports whether, on a vLLM host, the model chosen for this
// computer is not on disk.
func (p *agentInferenceProvider) vllmChosenAbsent() bool {
	if p.servingEngine() != catalog.RuntimeVLLM || p.store == nil {
		return false
	}
	m, ok := p.preferredManifest()
	if !ok {
		return false
	}
	st, err := p.store.Load()
	if err != nil {
		return false
	}
	ms := st.Models[m.ModelID]
	return ms.State != catalog.ModelStateReady || ms.LocalPath == "" || !dirExists(ms.LocalPath)
}

// vllmTargetDownloading reports whether the model chosen for this computer
// is downloading right now on a vLLM host — the state that reads "loading"
// while nothing answers yet.
func (p *agentInferenceProvider) vllmTargetDownloading() bool {
	if p.servingEngine() != catalog.RuntimeVLLM {
		return false
	}
	m, ok := p.preferredManifest()
	return ok && p.pullInFlight(m.ModelID)
}
