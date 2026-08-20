package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Model residency as a setting (waired-agent#861).
//
// The engine unloads a model that has gone idle, and the next request
// then pays a weights reload AND a full prefill — 17-56 s on the
// measured fleet. Holding the model instead is the default (owner
// ruling, docs/decisions/20260820/0130-model-residency-is-a-setting.md),
// but an operator who wants the memory back on a timer needs somewhere
// to say so. These endpoints are that somewhere, and they back every
// surface that offers the choice: the CLI, the tray, and the control
// plane.

// ErrInvalidResidency is returned by a ResidencyController.SetResidency
// when the requested value is not usable. handleInferenceResidency maps
// it to HTTP 400; any other error is a 500. Implementations should wrap
// it: fmt.Errorf("%w: %v", management.ErrInvalidResidency, err).
var ErrInvalidResidency = errors.New("invalid residency")

// ResidencyController is the daemon hook the residency endpoints
// delegate to. The implementation owns both halves of the change — the
// live engine setting and persistence to agent.json — so a new value
// takes effect on the model that is loaded RIGHT NOW and also survives a
// restart. Pass nil to WithResidencyControl to disable the endpoints.
type ResidencyController interface {
	// Residency returns the current idle timeout. Zero or negative means
	// the model is held indefinitely.
	Residency(ctx context.Context) (time.Duration, error)
	// SetResidency applies idle to the running engine, persists it, and
	// returns the value now in force together with how it reached the
	// engine. A value it cannot accept must be reported as
	// ErrInvalidResidency so the endpoint answers 400.
	SetResidency(ctx context.Context, idle time.Duration) (time.Duration, ResidencyEffect, error)
}

// ResidencyEffect says how a new setting reached the engine, so the
// surfaces can report what actually happened instead of asserting the
// change is live in every case (waired-agent#908).
//
// The distinction is not cosmetic. The engine reads OLLAMA_KEEP_ALIVE
// once, at spawn, and the serving path cannot carry a per-request
// keep_alive: waired serves over ollama's OpenAI-compatible
// /v1/chat/completions, which accepts the field and silently discards it
// (measured against a live engine — the model came back holding the
// spawn value, not the one sent). So a change made while nothing is
// resident does NOT govern the next model a request loads unless
// something re-spawns the engine.
type ResidencyEffect string

const (
	// ResidencyEffectLive: a model was resident and was re-stamped with
	// the new keep_alive. No reload — expires_at moves and the weights
	// stay put.
	ResidencyEffectLive ResidencyEffect = "live"
	// ResidencyEffectEngineRestarted: nothing was resident, so the engine
	// was re-spawned to re-read the value. Free precisely because there
	// was no model to lose — which is what makes this safe here and not
	// on the branch above.
	ResidencyEffectEngineRestarted ResidencyEffect = "engine-restarted"
	// ResidencyEffectOnEngineStart: the engine is stopped. It reads the
	// new value when the operator starts it.
	ResidencyEffectOnEngineStart ResidencyEffect = "on-engine-start"
	// ResidencyEffectNeedsEngineRestart: an adopted engine — spawned by a
	// previous run, so its environment is not ours to set and we hold no
	// process handle to re-spawn it (waired-agent#320). The setting is
	// saved but cannot govern a fresh load until that engine is
	// restarted. Said out loud rather than swallowed: a surface may not
	// refuse silently (waired#1067).
	ResidencyEffectNeedsEngineRestart ResidencyEffect = "needs-engine-restart"
)

// ResidencyResponse is the body of GET and POST
// /waired/v1/inference/residency.
type ResidencyResponse struct {
	// IdleTimeout is a Go duration string ("0s" when the model is held
	// indefinitely). A string rather than a number because that is the
	// form agent.json, the flag and the env var all already use, so the
	// value an operator reads back is the value they could have typed.
	IdleTimeout string `json:"idle_timeout"`
	// HoldsIndefinitely saves every client from re-deriving the meaning
	// of a zero. It is the whole product decision in one boolean, and a
	// renderer that got it wrong would tell an operator their model is
	// unloaded in no time at all rather than never.
	HoldsIndefinitely bool `json:"holds_indefinitely"`
	// Effect is how the value reached the engine. Empty on a GET, and
	// empty from an agent that predates it, so a client must read the
	// unknown case as "no claim" rather than as failure.
	Effect ResidencyEffect `json:"effect,omitempty"`
}

// ResidencyRequest is the body of POST /waired/v1/inference/residency.
type ResidencyRequest struct {
	// IdleTimeout is parsed with time.ParseDuration. "0" (or any
	// non-positive value) means hold indefinitely.
	IdleTimeout string `json:"idle_timeout"`
}

func residencyResponse(d time.Duration) ResidencyResponse {
	if d < 0 {
		d = 0
	}
	return ResidencyResponse{IdleTimeout: d.String(), HoldsIndefinitely: d <= 0}
}

func residencyResponseWithEffect(d time.Duration, e ResidencyEffect) ResidencyResponse {
	out := residencyResponse(d)
	out.Effect = e
	return out
}

// WithResidencyControl attaches a ResidencyController so the server
// exposes GET and POST /waired/v1/inference/residency. The write flows
// over the local IPC socket like every other mutating verb (waired#838).
// Pass nil to disable.
func (s *Server) WithResidencyControl(c ResidencyController) *Server {
	s.residencyControl = c
	return s
}

// handleInferenceResidency serves the residency setting: GET reads it,
// POST changes it. One route for both because the value is a single
// scalar and a client that can set it always wants to read back what
// actually landed.
func (s *Server) handleInferenceResidency(w http.ResponseWriter, r *http.Request) {
	if s.residencyControl == nil {
		http.Error(w, "residency controller not configured", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		d, err := s.residencyControl.Residency(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, residencyResponse(d))
	case http.MethodPost:
		var req ResidencyRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		idle, err := ParseResidency(req.IdleTimeout)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		applied, effect, err := s.residencyControl.SetResidency(r.Context(), idle)
		if errors.Is(err, ErrInvalidResidency) {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, residencyResponseWithEffect(applied, effect))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ParseResidency turns an operator-supplied residency into a duration.
// Exported so the CLI and the tray parse it exactly as the daemon does,
// rather than each growing its own idea of what "never" is spelled.
//
// Accepted: any time.ParseDuration value, plus the words the surfaces
// offer for "hold it" — a client should not have to know that the
// product spells indefinite as a zero.
//
// "always" leads that list because it is the word the product actually
// shows: the CLI prints it, the tray and the console label the button
// with it, and it is the term pinned for the ja mirror
// (docs-site/TRANSLATION.md, waired-agent#904). It was the one word the
// parser rejected (waired-agent#909).
//
// "never" and "off" are NOT accepted, deliberately. Both read as the
// opposite of what they do here — "never keep it", "residency off" —
// while meaning "never unload it". A word that argues against its own
// effect is worse than no word for it.
func ParseResidency(s string) (time.Duration, error) {
	switch s {
	case "", "always", "indefinite", "keep", "0":
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, nil
	}
	return d, nil
}
