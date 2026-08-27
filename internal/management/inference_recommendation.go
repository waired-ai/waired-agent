package management

import (
	"encoding/json"
	"net/http"
)

// handleInferenceBenchmark forces a fresh on-device throughput benchmark
// of the active model and returns the resulting recommendation — lighter
// when below the interactive floor (issue #133), upgrade when the host
// has headroom for a higher quality tier.
//
//	POST /waired/v1/inference/benchmark
//	200 → {"ran":true,"measured_tokps":N,"recommendation":{...}|absent,"upgrade":{...}|absent}
//	425 → engine/model not ready yet (the caller should poll status)
//
// Acceptance is out-of-band: the caller POSTs /preferred-model with the
// recommendation's to_model_id. Decline goes to /recommendation/dismiss.
type BenchmarkRunResponse struct {
	Ran bool `json:"ran"`
	// MeasuredTokps is the fresh measurement, recommendation or not.
	// 0 on responses from pre-upgrade daemons.
	MeasuredTokps float64 `json:"measured_tokps,omitempty"`
	// ModelID names what MeasuredTokps was measured on, so a caller can
	// report the figure as a property of a model rather than of the host
	// (waired-agent#1027). Empty from a pre-upgrade daemon, which is what
	// the CLI falls back on: the rate alone, exactly as it read before.
	ModelID string `json:"model_id,omitempty"`
	// Recommendation carries LIGHTER suggestions only — its wire
	// semantics are frozen so old clients keep rendering it as "local
	// inference is slow". Upgrades ride the separate Upgrade key,
	// which old clients simply ignore.
	Recommendation *BenchmarkRecommendation `json:"recommendation,omitempty"`
	Upgrade        *BenchmarkRecommendation `json:"upgrade,omitempty"`

	// BelowFloor and FloorTokps report the speed verdict independently
	// of whether there is a lighter model to propose.
	//
	// Absent recommendation used to be read as "fast enough", which is
	// false on a host already serving the smallest model Waired offers:
	// there is nothing lighter, so no recommendation is produced, and
	// the run's own conclusion was lost (waired-agent#784). An older
	// client that does not decode these keeps its previous reading,
	// which is what it had before the fields existed.
	BelowFloor bool    `json:"below_floor,omitempty"`
	FloorTokps float64 `json:"floor_tokps,omitempty"`
	// DepthOOMTokens is the shallowest prompt depth at which the
	// accelerator ran out of memory during the long-context sweep, or 0
	// if it did not (waired-agent#1058).
	//
	// Carried separately from BelowFloor because it answers a different
	// question. BelowFloor drives "this is slow; here is a lighter
	// model", and a host that cannot serve its window at all is not
	// slow — a lighter model at the same window is not obviously the
	// remedy, and telling a person their computer is slow when it ran
	// out of memory sends them somewhere else entirely.
	//
	// An older client that does not decode this reads BelowFloor alone
	// and says "slow", which is what it would have said anyway.
	DepthOOMTokens int `json:"depth_oom_tokens,omitempty"`
}

func (s *Server) handleInferenceBenchmark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "POST only"))
		return
	}
	if s.inference == nil {
		http.Error(w, "inference not configured", http.StatusNotFound)
		return
	}

	out, ok, err := s.inference.RunBenchmark(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("benchmark_failed", err.Error()))
		return
	}
	if !ok {
		// Engine or model not ready yet — the installer flow should poll
		// /inference/status until subsystem_state == "ready" and retry.
		writeJSON(w, http.StatusTooEarly, errorBody("engine_not_ready",
			"engine or active model is not ready yet; poll /waired/v1/inference/status and retry"))
		return
	}

	if out.Failed {
		// The benchmark RAN and failed (the warm-up got an engine 5xx via
		// warmUpEngine → failBench, the measurement timed out, …). Returning
		// 200 with measured_tokps=0 is how a dead engine came to print a
		// green "Local inference works": both this handler and the CLI read
		// a zero rate as "old daemon that doesn't report the figure".
		//
		// 503 rather than 500 because the 500 above means the provider itself
		// errored; a benchmark that ran and failed is a retryable engine
		// condition. The distinct error_code keeps them apart — and a non-200
		// is what makes an OLD CLI safe too: it falls into "Benchmark
		// unavailable (HTTP 503); skipping" and prints no success line.
		writeJSON(w, http.StatusServiceUnavailable,
			errorBody("benchmark_did_not_complete", out.Error))
		return
	}

	resp := BenchmarkRunResponse{
		Ran:            true,
		MeasuredTokps:  out.MeasuredTokps,
		ModelID:        out.ModelID,
		BelowFloor:     out.BelowFloor,
		FloorTokps:     out.FloorTokps,
		DepthOOMTokens: out.DepthOOMTokens,
	}
	// A nil / empty-ToModelID entry means "benched fine, nothing to
	// suggest" in that direction.
	if out.Lighter != nil && out.Lighter.ToModelID != "" {
		rc := *out.Lighter
		resp.Recommendation = &rc
	}
	if out.Upgrade != nil && out.Upgrade.ToModelID != "" {
		rc := *out.Upgrade
		resp.Upgrade = &rc
	}
	writeJSON(w, http.StatusOK, resp)
}

// BenchmarkStatusResponse is the body of
// GET /waired/v1/inference/benchmark/status (waired#835 §12). The
// benchmark is a single-flight job detached from request contexts, so
// a caller whose POST /inference/benchmark timed out (or the NAVI
// setup flow, which never blocks on the POST) can poll this for the
// eventual result. Gen is the declarative generation counter of the
// last COMPLETED run (0 = a run not requested via the counter, e.g.
// CLI/boot); it survives daemon restarts.
type BenchmarkStatusResponse struct {
	// State is one of "idle" (never ran), "running", "done", "failed".
	State string `json:"state"`
	// Gen is the generation of the last completed run.
	Gen int `json:"gen,omitempty"`
	// MeasuredTokps is the throughput this host has on record for the
	// model it is SERVING (0 / absent when it has none). It used to be
	// "the last completed measurement", which named no model and so
	// outlived the one it described — see servedModelFigure for the rule
	// that replaced it (waired-agent#971).
	MeasuredTokps float64 `json:"measured_tokps,omitempty"`
	// ModelID names the model MeasuredTokps describes. Empty means the
	// figure is unlabelled — a record written before the field existed —
	// and NOT that it belongs to no model; a consumer that needs a
	// subject should say nothing rather than guess one.
	ModelID string `json:"model_id,omitempty"`
	// MeasuredAt is the completion time of the last run, RFC3339. Empty
	// while idle.
	MeasuredAt string `json:"measured_at,omitempty"`
	// Error carries the failure detail when State is "failed".
	Error string `json:"error,omitempty"`
	// Outcome is WHICH ending the run had — "measured", "failed",
	// "engine_not_ready", "skipped" — where State says only whether a
	// figure is usable. The two answer different questions: State
	// "failed" covers both a host that measured badly and a host that
	// was never asked, and only the first is a statement about this
	// machine's performance (waired-agent#203).
	//
	// Empty means "not recorded", which every consumer must treat as the
	// unspecific failure it used to be: a record written before the
	// field existed, or a run that predates it.
	Outcome string `json:"outcome,omitempty"`

	// Per-measurement progress (waired-agent#199). The benchmark takes
	// benchSampleCount samples after a warm-up that can itself run for
	// ~180 s on a cold multi-GB model; with nothing emitted in between,
	// the setup wizard could only show an unbounded spinner for the
	// whole of it.
	//
	// Phase is "warmup" or "measuring" while State is "running", and
	// empty otherwise. Trial is the 1-based index of the last completed
	// sample — 0 during warm-up, which is exactly how the §7 wire
	// distinguishes the two without a phase field of its own.
	Phase  string `json:"phase,omitempty"`
	Trial  int    `json:"trial,omitempty"`
	Trials int    `json:"trials,omitempty"`
	// SampleTokps is the last completed sample; MedianTokps and
	// SpreadPct are over the samples so far while running, and over the
	// whole run once it is done. MeasuredTokps above stays the FINAL
	// answer and is never a running value — shipped consumers render it
	// as "Speed: about N" (waired#934 §7.2).
	SampleTokps float64 `json:"sample_tokps,omitempty"`
	MedianTokps float64 `json:"median_tokps,omitempty"`
	SpreadPct   float64 `json:"spread_pct,omitempty"`
	// Method is how the figure was obtained: ollama_eval | openai_slope |
	// wall_clock. A wall_clock result carries request overhead and must
	// be treated as low-confidence downstream.
	Method string `json:"method,omitempty"`
}

// Benchmark job states — values of BenchmarkStatusResponse.State.
const (
	BenchmarkStateIdle    = "idle"
	BenchmarkStateRunning = "running"
	BenchmarkStateDone    = "done"
	BenchmarkStateFailed  = "failed"
)

func (s *Server) handleInferenceBenchmarkStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	if s.inference == nil {
		http.Error(w, "inference not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.inference.BenchmarkStatus())
}

// RecommendationDismissRequest is the body of
// POST /waired/v1/inference/recommendation/dismiss. Empty fields dismiss
// the current live recommendation.
type RecommendationDismissRequest struct {
	FromVariantID string `json:"from_variant_id"`
	ToVariantID   string `json:"to_variant_id"`
}

func (s *Server) handleInferenceRecommendationDismiss(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "POST only"))
		return
	}
	if s.inference == nil {
		http.Error(w, "inference not configured", http.StatusNotFound)
		return
	}
	// Body is optional; an empty/absent body dismisses the live one.
	var req RecommendationDismissRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if err := s.inference.DismissRecommendation(req.FromVariantID, req.ToVariantID); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("dismiss_failed", err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
