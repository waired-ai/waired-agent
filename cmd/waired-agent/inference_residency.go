package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// Model residency — whether the weights are in (V)RAM right now — is
// observed here and cached on the adapter for the status surfaces
// (waired-agent#879).
//
// Every readiness signal in the product answers "process alive + model
// file on disk": subsystemState's inputs carry no residency term, and
// the peer health probe's EngineReady is the same question. So a host
// that unloaded an hour ago and a host mid-token report the identical
// state, while the first spends a weights reload and a full prefill
// before its first token — 17-56 s on the measured fleet (#861).
//
// The observation rides the local inference probe loop rather than the
// status call: that loop already runs at state.HeartbeatInterval and
// already talks to the engine, whereas status is polled independently by
// the tray, the CLI and the management API. Reading /api/ps per status
// call would multiply engine requests by the number of watchers to learn
// a fact that changes on the scale of the keep-alive.

// refreshOllamaResidency reads /api/ps once and records the answer on the
// adapter. Best-effort: an unreachable engine leaves the previous
// observation in place rather than asserting "not resident", because the
// two are different answers and a transient probe failure must not be
// rendered as an unload.
func refreshOllamaResidency(ctx context.Context, ad *infruntime.OllamaAdapter, client *http.Client) {
	if ad == nil {
		return
	}
	var ps psResponse
	if err := getJSON(ctx, client, ad.BaseURL()+"/api/ps", probeHTTPTimeout, &ps); err != nil {
		return
	}
	next := residencyFromPS(ps, time.Now().UTC())
	// waired-agent#837: log the EDGE, not the reading. Every status surface
	// shows a snapshot, and a snapshot cannot answer the question a bug
	// report actually poses — when did this model arrive, and what took it
	// away. One comparison per probe tick buys that timeline; logging the
	// reading itself would write a line every five seconds forever.
	if prev := ad.Residency(); prev.Observed && prev.Model != next.Model {
		slog.Info("engine residency changed",
			"was", residencyTagOrNone(prev.Model),
			"now", residencyTagOrNone(next.Model))
	}
	ad.SetResidency(next)
}

// residencyTagOrNone renders a residency tag for a log line. "none" rather
// than an empty value, because an empty field reads as a field nobody set.
func residencyTagOrNone(tag string) string {
	if tag == "" {
		return "none"
	}
	return tag
}

// applyToColdEngine decides what a new setting can do when the engine
// holds no model, which is the only state in which the process
// environment still decides the next load.
//
// The engine mode is a parameter rather than read here so both answers
// are reachable in a test: adopting an orphan is a whole bootstrap, and
// the branch that must not go untested is precisely the one that has to
// speak up (waired#1067 — no surface refuses silently).
func (p *agentInferenceProvider) applyToColdEngine(mode infruntime.EngineMode) management.ResidencyEffect {
	// An adopted engine was spawned by a previous run: its environment is
	// not ours to set and we hold no process handle to re-spawn it
	// (waired-agent#320). Nothing available here can make the next
	// request-driven load honour the setting, so say so rather than
	// report a success the operator would have no reason to doubt.
	if mode == infruntime.EngineModeAdopted {
		return management.ResidencyEffectNeedsEngineRestart
	}
	p.requestEngineRespawn()
	return management.ResidencyEffectEngineRestarted
}

// residencyFromPS maps an /api/ps body onto the recorded observation.
// Factored out so the mapping is testable without an engine, including
// the cases that matter: nothing loaded, and an expiry the agent cannot
// parse (which must still count as resident — the weights are in memory
// whether or not we can read the clock).
func residencyFromPS(ps psResponse, now time.Time) infruntime.ModelResidency {
	out := infruntime.ModelResidency{Observed: true, At: now}
	if len(ps.Models) == 0 {
		return out
	}
	// Under infruntime.MaxResidentModels the engine holds one model at a
	// time (owner ruling 2026-08-10, waired-agent#644), so the first row
	// is the answer; taking it explicitly keeps this correct rather than
	// accidentally so if that ever changes.
	m := ps.Models[0]
	out.Model = m.Name
	if m.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, m.ExpiresAt); err == nil {
			// An indefinite hold is reported as a date centuries out, not
			// as a sentinel. Recording that date as Until would hand every
			// surface a deadline to render, and "until 2318-11-30" is not
			// something an operator can read as "kept" (waired-agent#910).
			if infruntime.ExpiryIsIndefinite(t, now) {
				out.Indefinite = true
			} else {
				out.Until = t.UTC()
			}
		}
	}
	return out
}

// ModelResident is the closure shape inference.Config.ModelResidentFn
// expects: (weights in (V)RAM, whether that has been observed at all).
//
// observed=false is returned for every case where the answer is not
// known — no provider, no ollama adapter, no probe yet — because a peer
// reading this must not mistake "we have not looked" for "cold".
func (s *inferenceSubsystem) ModelResident() (bool, bool) {
	if s == nil || s.provider == nil || s.provider.ollama == nil {
		return false, false
	}
	// Only the ollama engine has a residency to observe. On a vLLM host this
	// used to report the ollama adapter's cache — a different engine's, and
	// on a host that also runs an unmanaged `ollama serve`, a STRANGER's
	// (waired-ai/waired-agent#943). "Not observed" is the ratified spelling
	// for an answer we do not have: docs/decisions/20260820/0130-model-
	// residency-is-a-setting.md says nil means "not observed", not "cold".
	if s.provider.servingEngine() != catalog.RuntimeOllama {
		return s.provider.vllmResident()
	}
	res := s.provider.ollama.Residency()
	if !res.Observed {
		return false, false
	}
	return res.Resident(), true
}

// vllmResident answers the residency question for a host serving on vLLM
// (waired-agent#965).
//
// A ready vLLM engine is resident BY CONSTRUCTION, and that is an
// observation rather than an assumption: waitReady only reports ready after
// /health returns 200 and /v1/models confirms the served model is the
// configured one, so the weights are in VRAM at that point, and
// --gpu-memory-utilization holds the pool until the process exits. There is
// no idle unload to lose it to.
//
// Which matters because the field feeds the peer preference in
// waired-agent#880. Reporting "not observed" here — correct as far as it
// went (waired-agent#943 removed a reading of a DIFFERENT engine's cache) —
// made vLLM hosts permanently invisible to a term meant to prefer warm
// peers, and they are by construction the warmest hosts on the mesh.
//
// Not-ready is a real "not resident", not a shrug: waired-agent#946 put a
// supervisor on the process, so the adapter's state moves off Ready when the
// engine dies. Before that it could sit on a stale Ready indefinitely and
// this would have been asserting rather than observing. No adapter at all is
// the one genuinely unobserved case.
func (p *agentInferenceProvider) vllmResident() (bool, bool) {
	a := p.vllmAdapter()
	if a == nil {
		return false, false
	}
	return a.Health(context.Background()).State == infruntime.StateReady, true
}

// LocalResidency is gateway.Deps.LocalResidency: the last /api/ps
// observation, whole (waired-agent#837).
//
// Sibling of ModelResident above, which answers a yes/no for the peer health
// probe. The gateway needs the TAG and the timestamp instead, because
// "something is resident" and "what this request needs is resident" are
// different answers and merging them is what makes a log line lie.
//
// The zero value carries Observed=false, which every reader must render as
// "we have not looked" rather than "nothing is loaded".
func (p *agentInferenceProvider) LocalResidency() infruntime.ModelResidency {
	if p == nil || p.ollama == nil {
		return infruntime.ModelResidency{}
	}
	return p.ollama.Residency()
}

// activeServingTags names the engine-native tags that count as "the model
// this computer serves" (waired-agent#837). Empty when that cannot be
// resolved, which callers must render as "no claim" rather than as a
// mismatch: under one-model-resident a wrong "not the model this computer
// serves" would appear on a perfectly warm machine.
//
// A slice, though it holds at most one entry today: it held two while a
// waired#642 derived batch model could give this host a second name for
// same weights, and the shape outlived that override
// (waired-agent#1079). Callers already treat an empty result as "no
// claim", so the arity is theirs to stop caring about.
func (p *agentInferenceProvider) activeServingTags() []string {
	if p == nil || p.store == nil {
		return nil
	}
	st, err := p.store.Load()
	if err != nil || st.Active == nil || st.Active.Runtime != catalog.RuntimeOllama {
		return nil
	}
	ms, ok := st.Models[st.Active.ModelID]
	if !ok {
		return nil
	}
	var tags []string
	if ms.OllamaTag != "" {
		tags = append(tags, ms.OllamaTag)
	}
	return tags
}

// keepAlive renders this host's configured residency for a per-request
// keep_alive field.
//
// Sent explicitly rather than left to the serve-level variable because
// an ADOPTED engine was spawned by a previous run and its environment is
// not ours to set (waired-agent#320) — a warm that trusted
// OLLAMA_KEEP_ALIVE would be undone minutes later on exactly the hosts
// that cannot be bounced to fix it.
func (p *agentInferenceProvider) keepAlive() string {
	if p == nil || p.ollama == nil {
		return infruntime.ResolveRequestKeepAlive(0)
	}
	return p.ollama.KeepAlive()
}

// UnloadServingModel releases the serving model's memory while leaving
// the engine running (waired-agent#861).
//
// Until this existed the only way to get the memory back was to stop the
// engine itself (`waired inference engine stop` / Park), which also ends
// the ability to serve. That is a gap against every comparable local-LLM
// application: LM Studio has an Eject control and an unload endpoint,
// Ollama upstream has `ollama stop <model>`. It matters more now that
// residency is held by default, because it is the release valve that
// makes that default safe to live with.
//
// Reports the tag that was unloaded, or an empty tag when nothing was
// resident — which is a success, not an error: the caller asked for the
// memory back and the memory is back.
func (p *agentInferenceProvider) UnloadServingModel(ctx context.Context) (string, error) {
	if p == nil || p.ollama == nil {
		return "", errors.New("no ollama engine on this host")
	}
	// The guard that matters is WHICH ENGINE SERVES, not whether an ollama
	// adapter exists: one always does (inference.go builds it whatever the
	// host serves with), so the check above never fires. On a vLLM host this
	// function used to read that idle adapter's /api/ps and report "nothing
	// was loaded" while vLLM held the weights — or, on a host with an
	// unmanaged `ollama serve`, read a stranger's engine
	// (waired-ai/waired-agent#943).
	//
	// vLLM has no unload axis to reach: --gpu-memory-utilization reserves
	// the pool at start-up and it is held to process exit, which is why
	// `waired inference engine stop` is the only release valve there.
	if p.servingEngine() != catalog.RuntimeOllama {
		return "", fmt.Errorf("%w: %s", management.ErrUnloadNotSupported, engineHoldsModelForLife)
	}
	client := &http.Client{}
	baseURL := p.ollama.BaseURL()
	var ps psResponse
	if err := getJSON(ctx, client, baseURL+"/api/ps", probeHTTPTimeout, &ps); err != nil {
		return "", fmt.Errorf("read loaded models: %w", err)
	}
	if len(ps.Models) == 0 {
		refreshOllamaResidency(ctx, p.ollama, client)
		return "", nil
	}
	tag := ps.Models[0].Name
	// keep_alive 0 is ollama's "unload as soon as this request is done".
	if err := loadOllamaModel(ctx, client, baseURL, tag, "0"); err != nil {
		return "", fmt.Errorf("unload %s: %w", tag, err)
	}
	// Re-read rather than assume: the status surfaces must not claim an
	// unload that did not happen.
	refreshOllamaResidency(ctx, p.ollama, client)
	return tag, nil
}

// CurrentResidency reports the live residency setting, and whether there
// is an engine to have one.
func (p *agentInferenceProvider) CurrentResidency() (time.Duration, bool) {
	if p == nil || p.ollama == nil {
		return 0, false
	}
	// A vLLM host holds the model from engine start to engine stop, so the
	// live answer is "indefinitely" — reported as a real reading rather than
	// as "no engine to have one", which would make residencyController fall
	// back to the persisted agent.json value. That value describes an
	// engine this host is not serving with (waired-ai/waired-agent#943).
	if p.servingEngine() != catalog.RuntimeOllama {
		return 0, true
	}
	return p.ollama.KeepAliveDuration(), true
}

// ResidencySupported reports whether the SERVING engine has a residency axis
// at all. See engineHoldsModelForLife for the vLLM case.
func (p *agentInferenceProvider) ResidencySupported() bool {
	return p != nil && p.ollama != nil && p.servingEngine() == catalog.RuntimeOllama
}

// engineHoldsModelForLife is the reason both refusals carry, and it is the
// same sentence in both places on purpose: it is one fact about the host.
//
// It names "the inference engine" as the generic noun (waired-ai/waired#1272), per the owner
// ruling pinned in docs-site/TRANSLATION.md (waired-agent#836/#850): a user
// does not choose the engine and cannot act on knowing which one it is. The
// quoted span is exactly a command and nothing else, so a reader can copy it
// (waired-agent#862).
const engineHoldsModelForLife = "the inference engine on this computer holds the model for as long as the engine runs. " +
	"To free the memory, stop the engine: `waired inference engine stop`"

// ApplyResidency changes the residency setting on the running engine
// (#861), and reports HOW it got there (#908).
//
// The engine reads OLLAMA_KEEP_ALIVE once, at spawn, and there is no
// second route: waired serves over ollama's OpenAI-compatible
// /v1/chat/completions, which accepts a keep_alive field and silently
// discards it (measured against a live engine — the model came back
// holding the spawn value). So the value that governs a model loaded by
// a REQUEST is whatever the process was spawned with, full stop.
//
// That leaves two cases, and they want opposite treatment:
//
//   - A model is resident. Re-stamp it by loading it again with the new
//     keep_alive — measured, that moves expires_at and does NOT reload
//     the weights. Bouncing here would unload the very model being
//     configured.
//   - Nothing is resident. Re-spawn, so the process env is right for the
//     next load. The objection to bouncing does not apply when there is
//     no model to lose, and this is the only branch on which a request-
//     driven load can happen next.
//
// A third shape was tried and rejected: re-stamping whatever the probe
// loop observes. It cannot work for a finite setting — each re-stamp
// sets expires_at to now+idle, so a model re-stamped on a 5 s cadence
// never expires at all, which is precisely what a finite setting asks
// for.
func (p *agentInferenceProvider) ApplyResidency(ctx context.Context, idle time.Duration) (management.ResidencyEffect, error) {
	if p == nil || p.ollama == nil {
		return "", errors.New("no ollama engine on this host")
	}
	// Nothing to apply, and nothing to lie about: the engine serving here has
	// no idle timeout to honour (waired-ai/waired-agent#943). Reported as an
	// effect rather than an error so the value is still PERSISTED — residency
	// is a setting, and #339 lets this host adopt an engine that does have
	// the axis without a restart, at which point the operator's choice has to
	// be waiting for it.
	if p.servingEngine() != catalog.RuntimeOllama {
		return management.ResidencyEffectUnsupported, nil
	}
	p.ollama.SetKeepAlive(idle)

	// Parked: there is no process to carry the value and no model to
	// re-stamp. The next start spawns from the live setting, so this is
	// done — but it is not "live", and saying so would be a lie the
	// operator could act on.
	if p.ollama.IsParked() {
		return management.ResidencyEffectOnEngineStart, nil
	}

	client := &http.Client{}
	baseURL := p.ollama.BaseURL()
	var ps psResponse
	if err := getJSON(ctx, client, baseURL+"/api/ps", probeHTTPTimeout, &ps); err != nil {
		// The setting is stored; the engine is simply not answering right
		// now. Report it so the caller can say the change lands on the
		// next load rather than immediately.
		return "", fmt.Errorf("setting stored, but the engine did not answer: %w", err)
	}
	if len(ps.Models) == 0 {
		return p.applyToColdEngine(p.ollama.Mode()), nil
	}
	if err := loadOllamaModel(ctx, client, baseURL, ps.Models[0].Name, infruntime.ResolveRequestKeepAlive(idle)); err != nil {
		return "", fmt.Errorf("setting stored, but restamping %s failed: %w", ps.Models[0].Name, err)
	}
	refreshOllamaResidency(ctx, p.ollama, client)
	return management.ResidencyEffectLive, nil
}
