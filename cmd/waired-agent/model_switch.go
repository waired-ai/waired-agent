package main

import (
	"context"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
)

// What a model switch does before the chosen model answers
// (waired-agent#1515): whether the engine restarts, whether anything answers
// meanwhile, and whether the build to start is expected to fit. `waired
// models use`, the Waired app and `waired inference status` each pick their
// sentence from it, so the three cannot tell the same switch three ways.

// switchFacts is what the outcome is decided from.
type switchFacts struct {
	// Engine is the engine this computer serves with.
	Engine string
	// TargetModel / TargetVariant: the chosen model and the build of it
	// the engine will load ("" when not known).
	TargetModel, TargetVariant string
	// Downloading: the chosen model's weights are downloading.
	Downloading bool
	// EngineUp: the engine has a process up (ready or starting).
	EngineUp bool
	// ServingModel / ServingVariant: what the running engine answers with.
	ServingModel, ServingVariant string
	// PreviousWillAnswer: nothing is up, and the model this computer was
	// running will be started to answer while the chosen one downloads
	// (vLLM; planVLLMTarget's start_previous).
	PreviousWillAnswer bool
	// PreviousModel names it.
	PreviousModel string
	// NeedVRAMMB / HaveVRAMMB: the build's catalog minimum and this computer's vLLM
	// VRAM budget; 0 when unknown.
	NeedVRAMMB, HaveVRAMMB int
}

// switchOutcome is the rule, pure so every combination is table-tested.
func switchOutcome(f switchFacts) management.ModelSwitchOutcome {
	o := management.ModelSwitchOutcome{Downloading: f.Downloading}
	servingTarget := f.EngineUp && f.ServingModel == f.TargetModel &&
		(f.TargetVariant == "" || f.ServingVariant == "" || f.ServingVariant == f.TargetVariant)
	answering := f.EngineUp && f.ServingModel != "" && !servingTarget
	if f.Engine == catalog.RuntimeVLLM {
		// One model per vLLM process: anything but the build already up
		// means a restart onto it.
		o.EngineRestarts = !servingTarget
		answering = answering || f.PreviousWillAnswer
		if f.NeedVRAMMB > 0 && f.HaveVRAMMB > 0 && f.NeedVRAMMB > f.HaveVRAMMB {
			o.NeedVRAMMB, o.HaveVRAMMB = f.NeedVRAMMB, f.HaveVRAMMB
		}
	}
	o.NothingAnswers = f.Downloading && !answering
	return o
}

// answeringModel is the model answering while the switch is under way, ""
// when nothing does.
func (f switchFacts) answeringModel() string {
	switch {
	case f.EngineUp && f.ServingModel != "" && f.ServingModel != f.TargetModel:
		return f.ServingModel
	case f.Engine == catalog.RuntimeVLLM && f.PreviousWillAnswer:
		return f.PreviousModel
	}
	return ""
}

// switchFactsFor reads the facts for a switch to modelID. downloading is
// passed in: right after a choice it is what the switch just started, and
// later it is whether the download is still in flight.
func (p *agentInferenceProvider) switchFactsFor(ctx context.Context, modelID string, downloading bool) switchFacts {
	f := switchFacts{
		Engine:      p.servingEngine(),
		TargetModel: modelID,
		Downloading: downloading,
		EngineUp:    p.engineIsUp(ctx),
	}
	var st catalog.State
	if p.store != nil {
		st, _ = p.store.Load()
	}
	if f.Engine == catalog.RuntimeVLLM {
		if s := p.vllmServing.Load(); s != nil {
			f.ServingModel, f.ServingVariant = s.ModelID, s.VariantID
		}
		if variantID, need, have, ok := p.vllmSwitchTarget(ctx, modelID); ok {
			f.TargetVariant, f.NeedVRAMMB, f.HaveVRAMMB = variantID, need, have
		}
		if !f.EngineUp {
			if prev, _, _, ok := vllmPreviousCandidate(st.Active, p.manifests, st, modelID,
				p.engineVersionFor(ctx, catalog.RuntimeVLLM), dirExists); ok {
				f.PreviousWillAnswer, f.PreviousModel = true, prev.ModelID
			}
		}
		return f
	}
	// ollama answers with the Active model while it is on disk.
	if a := st.Active; a != nil && a.Runtime == catalog.RuntimeOllama {
		if st.Models[a.ModelID].State == catalog.ModelStateReady {
			f.ServingModel, f.ServingVariant = a.ModelID, a.VariantID
		}
	}
	return f
}

// modelSwitchOutcome is what `/preferred-model` reports for a switch the
// daemon just applied in process.
func (p *agentInferenceProvider) modelSwitchOutcome(ctx context.Context, modelID string, downloading bool) management.ModelSwitchOutcome {
	return switchOutcome(p.switchFactsFor(ctx, modelID, downloading))
}

// modelSwitchStatus is the status line's view of a switch still under way:
// nil unless the chosen model is not what answers and either its download
// or a fit estimate stands in the way.
func (p *agentInferenceProvider) modelSwitchStatus(ctx context.Context) *management.ModelSwitchStatus {
	m, ok := p.preferredManifest()
	if !ok {
		return nil
	}
	f := p.switchFactsFor(ctx, m.ModelID, p.pullInFlight(m.ModelID))
	o := switchOutcome(f)
	if !o.Downloading && o.NeedVRAMMB == 0 {
		return nil
	}
	if !o.Downloading && f.EngineUp && f.ServingModel == f.TargetModel {
		return nil // already serving it; the fit estimate is moot
	}
	return &management.ModelSwitchStatus{
		ModelID:          m.ModelID,
		AnsweringModelID: f.answeringModel(),
		Downloading:      o.Downloading,
		EngineRestarts:   o.EngineRestarts,
		NeedVRAMMB:       o.NeedVRAMMB,
		HaveVRAMMB:       o.HaveVRAMMB,
	}
}
