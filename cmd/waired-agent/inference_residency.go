package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

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
	ad.SetResidency(residencyFromPS(ps, time.Now().UTC()))
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
			out.Until = t.UTC()
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
	res := s.provider.ollama.Residency()
	if !res.Observed {
		return false, false
	}
	return res.Resident(), true
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
		return infruntime.ResolveKeepAlive(0)
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
	return p.ollama.KeepAliveDuration(), true
}

// ApplyResidency changes the residency setting on the running engine
// (#861).
//
// The two-step shape is forced by how the engine reads the setting.
// OLLAMA_KEEP_ALIVE is consumed once, at spawn, so the obvious way to
// apply a change is to restart the engine — which unloads the model,
// i.e. does the exact thing the operator is configuring whether or not
// they asked for it. Instead the loaded copy is re-stamped by loading it
// again with the new keep_alive: measured against a live engine, that
// moves expires_at and does NOT reload the weights.
//
// Nothing resident is not a failure. The value is set for the next load,
// which is all there is to do.
func (p *agentInferenceProvider) ApplyResidency(ctx context.Context, idle time.Duration) error {
	if p == nil || p.ollama == nil {
		return errors.New("no ollama engine on this host")
	}
	p.ollama.SetKeepAlive(idle)

	client := &http.Client{}
	baseURL := p.ollama.BaseURL()
	var ps psResponse
	if err := getJSON(ctx, client, baseURL+"/api/ps", probeHTTPTimeout, &ps); err != nil {
		// The setting is stored; the engine is simply not answering right
		// now. Report it so the caller can say the change lands on the
		// next load rather than immediately.
		return fmt.Errorf("setting stored, but the engine did not answer: %w", err)
	}
	if len(ps.Models) == 0 {
		return nil
	}
	if err := loadOllamaModel(ctx, client, baseURL, ps.Models[0].Name, infruntime.ResolveKeepAlive(idle)); err != nil {
		return fmt.Errorf("setting stored, but restamping %s failed: %w", ps.Models[0].Name, err)
	}
	refreshOllamaResidency(ctx, p.ollama, client)
	return nil
}
