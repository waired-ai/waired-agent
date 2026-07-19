package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleInferenceBenchmark_Recommendation(t *testing.T) {
	inf := &fakeInference{
		benchOK: true,
		benchOut: BenchmarkOutcome{
			MeasuredTokps: 12,
			Lighter: &BenchmarkRecommendation{
				Direction:   RecommendationLighter,
				FromModelID: "qwen3-8b-instruct", FromVariantID: "q4-gguf",
				ToModelID: "qwen3-4b-instruct", ToVariantID: "q4-gguf",
				MeasuredTokps: 12, FloorTokps: 30,
			},
		},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())

	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/benchmark", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var got BenchmarkRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Ran || got.Recommendation == nil {
		t.Fatalf("got = %+v, want ran with recommendation", got)
	}
	if got.Recommendation.ToModelID != "qwen3-4b-instruct" {
		t.Errorf("to = %q, want qwen3-4b-instruct", got.Recommendation.ToModelID)
	}
	if got.Upgrade != nil {
		t.Errorf("upgrade = %+v, want nil alongside a lighter recommendation", got.Upgrade)
	}
	if got.MeasuredTokps != 12 {
		t.Errorf("measured_tokps = %v, want 12", got.MeasuredTokps)
	}
}

// TestHandleInferenceBenchmark_Upgrade pins the wire split: upgrades
// ride the NEW "upgrade" key while "recommendation" stays empty, so an
// old CLI/tray decoding only the legacy key sees "nothing to suggest"
// instead of mis-rendering a headroom host as slow.
func TestHandleInferenceBenchmark_Upgrade(t *testing.T) {
	inf := &fakeInference{
		benchOK: true,
		benchOut: BenchmarkOutcome{
			MeasuredTokps: 101,
			Upgrade: &BenchmarkRecommendation{
				Direction:   RecommendationUpgrade,
				FromModelID: "qwen2.5-coder-7b-instruct", FromVariantID: "q4-gguf",
				ToModelID: "qwen3-coder-30b-a3b-instruct", ToVariantID: "q4-gguf",
				MeasuredTokps: 101, FloorTokps: 30, PredictedTokps: 236,
			},
		},
	}
	s := newCatalogTestServer(t, inf, t.TempDir())

	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/benchmark", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, `"recommendation"`) {
		t.Errorf("legacy recommendation key present in upgrade-only response: %s", body)
	}
	var got BenchmarkRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Upgrade == nil || got.Upgrade.ToModelID != "qwen3-coder-30b-a3b-instruct" {
		t.Fatalf("upgrade = %+v, want qwen3-coder-30b-a3b-instruct", got.Upgrade)
	}
	if got.Upgrade.Direction != RecommendationUpgrade || got.Upgrade.PredictedTokps != 236 {
		t.Errorf("upgrade = %+v, want direction=upgrade predicted=236", got.Upgrade)
	}
}

func TestHandleInferenceBenchmark_RanNoSuggestion(t *testing.T) {
	// ok=true but empty recommendation (benched at/above floor).
	inf := &fakeInference{benchOK: true}
	s := newCatalogTestServer(t, inf, t.TempDir())

	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/benchmark", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var got BenchmarkRunResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Ran || got.Recommendation != nil {
		t.Errorf("got = %+v, want ran with nil recommendation", got)
	}
}

func TestHandleInferenceBenchmark_NotReady(t *testing.T) {
	inf := &fakeInference{benchOK: false}
	s := newCatalogTestServer(t, inf, t.TempDir())

	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/benchmark", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusTooEarly {
		t.Errorf("code = %d, want 425 TooEarly", w.Code)
	}
}

func TestHandleInferenceBenchmark_GETRejected(t *testing.T) {
	s := newCatalogTestServer(t, &fakeInference{}, t.TempDir())
	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/benchmark", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", w.Code)
	}
}

func TestHandleInferenceRecommendationDismiss(t *testing.T) {
	inf := &fakeInference{}
	s := newCatalogTestServer(t, inf, t.TempDir())

	body := strings.NewReader(`{"from_variant_id":"q4-gguf","to_variant_id":"q4-tiny"}`)
	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/recommendation/dismiss", body)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if inf.dismissedFrom != "q4-gguf" || inf.dismissedTo != "q4-tiny" {
		t.Errorf("dismissed = %q→%q, want q4-gguf→q4-tiny", inf.dismissedFrom, inf.dismissedTo)
	}
}

func TestHandleInferenceRecommendationDismiss_EmptyBody(t *testing.T) {
	inf := &fakeInference{}
	s := newCatalogTestServer(t, inf, t.TempDir())

	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/recommendation/dismiss", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204 (empty body dismisses current)", w.Code)
	}
}

func TestHandleInferenceBenchmarkStatus(t *testing.T) {
	inf := &fakeInference{benchStatus: BenchmarkStatusResponse{
		State: BenchmarkStateDone, Gen: 3, MeasuredTokps: 78.2,
		MeasuredAt: "2026-07-19T00:00:00Z",
	}}
	s := newCatalogTestServer(t, inf, t.TempDir())

	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/benchmark/status", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var got BenchmarkStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != BenchmarkStateDone || got.Gen != 3 || got.MeasuredTokps != 78.2 {
		t.Errorf("status = %+v, want done/gen=3/78.2", got)
	}
}

func TestHandleInferenceBenchmarkStatus_POSTRejected(t *testing.T) {
	inf := &fakeInference{}
	s := newCatalogTestServer(t, inf, t.TempDir())

	r := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/benchmark/status", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", w.Code)
	}
}
