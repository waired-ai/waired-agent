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
	// Recommendation carries LIGHTER suggestions only — its wire
	// semantics are frozen so old clients keep rendering it as "local
	// inference is slow". Upgrades ride the separate Upgrade key,
	// which old clients simply ignore.
	Recommendation *BenchmarkRecommendation `json:"recommendation,omitempty"`
	Upgrade        *BenchmarkRecommendation `json:"upgrade,omitempty"`
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

	resp := BenchmarkRunResponse{Ran: true, MeasuredTokps: out.MeasuredTokps}
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
