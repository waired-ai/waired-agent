package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The SIZE column read "-" for every model, including the several
// gigabytes actually on disk, because catalog.ModelState.SizeBytes is
// declared and read but never written (#661). The figure lives on the
// engine, so the listing asks for it — and only the listing does, because
// asking costs a round trip.

// TestModelsCollection_FillsSizesFromTheEngine is the fix: what the engine
// reports wins over the state file's zero.
//
// Product contract, ratified by #661.
func TestModelsCollection_FillsSizesFromTheEngine(t *testing.T) {
	inf := &fakeInference{
		models: []ModelEntry{
			{ModelID: "qwen3-8b", State: "ready"},
			{ModelID: "gemma3-4b", State: "ready"},
		},
		modelSizes: map[string]int64{"qwen3-8b": 5_100_000_000},
	}
	got := decodeModels(t, inf)

	if got["qwen3-8b"] != 5_100_000_000 {
		t.Errorf("qwen3-8b size = %d, want the engine's figure", got["qwen3-8b"])
	}
	// A model the engine did not account for keeps what the state file
	// held rather than inheriting its neighbour's number.
	if got["gemma3-4b"] != 0 {
		t.Errorf("gemma3-4b size = %d, want 0 — the engine said nothing about it", got["gemma3-4b"])
	}
}

// TestModelsCollection_EngineSilentLeavesEntriesAlone: a stopped or wedged
// engine reports nothing, and the endpoint then answers exactly as it did
// before sizes existed. Unknown is not zero, and it is not a failure
// either — the listing still comes back.
func TestModelsCollection_EngineSilentLeavesEntriesAlone(t *testing.T) {
	inf := &fakeInference{
		models:     []ModelEntry{{ModelID: "qwen3-8b", State: "ready", SizeBytes: 42}},
		modelSizes: nil,
	}
	got := decodeModels(t, inf)
	if got["qwen3-8b"] != 42 {
		t.Errorf("size = %d, want the state file's 42 left untouched", got["qwen3-8b"])
	}
}

// TestModelSizes_NotOnTheControlPath is why this is a separate method
// rather than something ListModels does.
//
// modelDownloaded (the preferred-model flow) reads the model list to
// decide whether a download is in flight. An engine round trip there buys
// nothing and can hang, so that path must never trigger one. The fake
// records whether it was asked.
func TestModelSizes_NotOnTheControlPath(t *testing.T) {
	inf := &sizeCountingInference{fakeInference: fakeInference{
		models: []ModelEntry{{ModelID: "qwen3-8b", State: "ready"}},
	}}
	if !modelDownloaded(inf.ListModels(context.Background()), "qwen3-8b") {
		t.Fatal("fixture wrong: the model is ready, so modelDownloaded should say so")
	}
	if inf.sizeCalls != 0 {
		t.Errorf("the control path asked the engine for sizes %d times, want 0", inf.sizeCalls)
	}
}

type sizeCountingInference struct {
	fakeInference
	sizeCalls int
}

func (f *sizeCountingInference) ModelSizes(ctx context.Context) map[string]int64 {
	f.sizeCalls++
	return f.fakeInference.ModelSizes(ctx)
}

// decodeModels drives GET /waired/v1/models and returns model_id -> size.
func decodeModels(t *testing.T, inf InferenceProvider) map[string]int64 {
	t.Helper()
	srv := newServerWithInference(inf)
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:1" // the endpoint is loopback-only
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Models []ModelEntry `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make(map[string]int64, len(body.Models))
	for _, m := range body.Models {
		out[m.ModelID] = m.SizeBytes
	}
	return out
}
