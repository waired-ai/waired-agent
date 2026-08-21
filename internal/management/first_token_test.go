package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/observability"
)

// req builds one recorded request event. Newest is appended last, the
// way the gateway records them.
func req(model string, ttft uint32, in int64, peer string) observability.Event {
	return observability.Event{
		Kind: observability.KindRequest,
		TS:   time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
		Request: &observability.RequestEvent{
			Model: model, TTFTMs: ttft, InputTokens: in, PeerID: peer,
		},
	}
}

func ringOf(events ...observability.Event) *observability.Ring {
	r := observability.NewRing(64)
	for _, e := range events {
		r.Append(e)
	}
	return r
}

// TestFirstTokenReading_PairsTheLatestWithTheHostsBest is the shape the
// line exists to render: a slow reading next to the same host's fastest
// comparable one, which is what makes the slow one readable.
func TestFirstTokenReading_PairsTheLatestWithTheHostsBest(t *testing.T) {
	r := ringOf(
		req("m", 2600, 29000, ""),
		req("m", 2900, 30000, ""),
		req("m", 35400, 30500, ""),
	)
	got := firstTokenReading(r, "m")
	if got == nil {
		t.Fatal("no reading from a ring with three qualifying requests")
	}
	if got.Ms != 35400 {
		t.Errorf("Ms = %d, want 35400 (the latest, not the best)", got.Ms)
	}
	if got.BestMs != 2600 {
		t.Errorf("BestMs = %d, want 2600", got.BestMs)
	}
	if got.BestOfSamples != 2 {
		t.Errorf("BestOfSamples = %d, want 2", got.BestOfSamples)
	}
	if got.Model != "m" {
		t.Errorf("Model = %q, want %q", got.Model, "m")
	}
	if got.At == "" {
		t.Error("At is empty; a reading with no time reads as a promise about the next request")
	}
}

// TestFirstTokenReading_ExcludesPeerServed is the filter that stops the
// line being false rather than merely unhelpful: a peer-answered request
// measures the PEER's prefill, and this line sits under the local
// `model loaded:` row.
func TestFirstTokenReading_ExcludesPeerServed(t *testing.T) {
	r := ringOf(
		req("m", 9000, 30000, ""),
		req("m", 120, 30000, "peer_b"), // a fast peer, not this host
	)
	got := firstTokenReading(r, "m")
	if got == nil {
		t.Fatal("the local request should still produce a reading")
	}
	if got.Ms != 9000 {
		t.Errorf("Ms = %d, want 9000 — a peer-served request must not become THIS host's latest", got.Ms)
	}
	if got.BestMs != 0 {
		t.Errorf("BestMs = %d, want 0 — a peer's first token is not this host's best", got.BestMs)
	}

	// And a ring holding nothing but peer-served traffic says nothing.
	if only := firstTokenReading(ringOf(req("m", 120, 30000, "peer_b")), "m"); only != nil {
		t.Errorf("a host that only routes away reported %+v, want no reading", only)
	}
}

// TestFirstTokenReading_ExcludesOtherModels: a reading belongs to the
// weights that produced it, so one taken before a model switch says
// nothing about the model named on the line above it.
func TestFirstTokenReading_ExcludesOtherModels(t *testing.T) {
	r := ringOf(
		req("other", 200, 30000, ""),
		req("m", 8000, 30000, ""),
		req("other", 150, 30000, ""),
	)
	got := firstTokenReading(r, "m")
	if got == nil {
		t.Fatal("no reading despite a qualifying request for the active model")
	}
	if got.Ms != 8000 || got.BestMs != 0 {
		t.Errorf("got Ms=%d BestMs=%d, want 8000/0 — another model's readings must not leak in", got.Ms, got.BestMs)
	}
}

// TestFirstTokenReading_ExcludesUnobserved pins the #874 rule one level
// on: TTFTMs == 0 means the serving leg could not see a first token, so
// admitting it would report "not observed" as the fastest this host has
// ever been.
func TestFirstTokenReading_ExcludesUnobserved(t *testing.T) {
	r := ringOf(
		req("m", 0, 30000, ""),
		req("m", 0, 30000, ""),
		req("m", 12000, 30000, ""),
	)
	got := firstTokenReading(r, "m")
	if got == nil {
		t.Fatal("no reading from the one observed request")
	}
	if got.BestMs != 0 {
		t.Errorf("BestMs = %d, want 0 — an unobserved first token is not a fast one", got.BestMs)
	}

	// Nothing observed at all is nothing to say, not a zero.
	if only := firstTokenReading(ringOf(req("m", 0, 30000, "")), "m"); only != nil {
		t.Errorf("an unobserved-only ring reported %+v, want no reading", only)
	}
}

// TestFirstTokenReading_ShortPromptsDoNotSetTheFloor is the sample-
// comparability rule. Prefill dominates a cold first token, so a one-line
// question answers far faster than a coding turn on the same weights —
// letting those into the pool would set a floor no real turn can reach
// and make every ordinary answer read as slow.
func TestFirstTokenReading_ShortPromptsDoNotSetTheFloor(t *testing.T) {
	r := ringOf(
		req("m", 90, 40, ""),       // "hi"
		req("m", 2600, 29000, ""),  // a real turn, warm
		req("m", 35400, 30500, ""), // a real turn, cold
	)
	got := firstTokenReading(r, "m")
	if got == nil {
		t.Fatal("no reading")
	}
	if got.BestMs != 2600 {
		t.Errorf("BestMs = %d, want 2600 — a 40-token prompt is not a comparable sample", got.BestMs)
	}
	if got.BestOfSamples != 1 {
		t.Errorf("BestOfSamples = %d, want 1", got.BestOfSamples)
	}
}

// TestFirstTokenReading_NoComparableLeavesTheFigureAlone covers the
// ordinary state on a fresh daemon, and the case where the latest reading
// is already the best. Reporting a "fastest" equal to the figure itself
// would be a comparison with nothing in it.
func TestFirstTokenReading_NoComparableLeavesTheFigureAlone(t *testing.T) {
	alone := firstTokenReading(ringOf(req("m", 2600, 29000, "")), "m")
	if alone == nil {
		t.Fatal("a single reading should still be reported")
	}
	if alone.Ms != 2600 || alone.BestMs != 0 {
		t.Errorf("got Ms=%d BestMs=%d, want 2600/0", alone.Ms, alone.BestMs)
	}

	fastest := firstTokenReading(ringOf(
		req("m", 9000, 30000, ""),
		req("m", 2600, 30000, ""),
	), "m")
	if fastest == nil {
		t.Fatal("no reading")
	}
	if fastest.BestMs != 0 {
		t.Errorf("BestMs = %d, want 0 — the latest reading IS the best, so there is nothing to compare it to", fastest.BestMs)
	}

	// No prompt size on the latest means nothing to call comparable TO.
	sizeless := firstTokenReading(ringOf(
		req("m", 2600, 29000, ""),
		req("m", 35400, 0, ""),
	), "m")
	if sizeless == nil || sizeless.Ms != 35400 {
		t.Fatalf("got %+v, want the sizeless latest reported", sizeless)
	}
	if sizeless.BestMs != 0 {
		t.Errorf("BestMs = %d, want 0 when the latest carries no prompt size", sizeless.BestMs)
	}
}

// TestFirstTokenReading_NothingToSay: no ring, no active model, no
// events. Each answers nil rather than a zero-valued reading.
func TestFirstTokenReading_NothingToSay(t *testing.T) {
	if got := firstTokenReading(nil, "m"); got != nil {
		t.Errorf("a nil ring reported %+v", got)
	}
	if got := firstTokenReading(ringOf(req("m", 2600, 29000, "")), ""); got != nil {
		t.Errorf("no active model reported %+v", got)
	}
	if got := firstTokenReading(observability.NewRing(8), "m"); got != nil {
		t.Errorf("an empty ring reported %+v", got)
	}
}

// TestInferenceStatus_CarriesFirstToken is the reachability test: the
// derivation being correct is worth nothing if the field never leaves the
// handler. It also pins the absent cases, so a client can tell "this
// agent does not report it" from "there was nothing to report".
func TestInferenceStatus_CarriesFirstToken(t *testing.T) {
	canned := InferenceStatus{
		SubsystemState: "ready",
		Active:         &ActiveSelection{ModelID: "m"},
	}
	ring := ringOf(
		req("m", 2600, 29000, ""),
		req("m", 35400, 30500, ""),
	)
	s := newServerWithInference(&fakeInference{canned: canned}).
		WithObservability(ObservabilityConfig{Ring: ring})

	r := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got InferenceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FirstToken == nil {
		t.Fatalf("first_token missing from the status body: %s", w.Body.String())
	}
	if got.FirstToken.Ms != 35400 || got.FirstToken.BestMs != 2600 {
		t.Errorf("first_token = %+v, want Ms=35400 BestMs=2600", got.FirstToken)
	}

	// No ring wired (older builds, tests): the key is absent entirely.
	bare := newServerWithInference(&fakeInference{canned: canned})
	r2 := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/status", nil)
	r2.RemoteAddr = "127.0.0.1:1"
	w2 := httptest.NewRecorder()
	bare.Handler().ServeHTTP(w2, r2)
	if body := w2.Body.String(); jsonHasKey(t, body, "first_token") {
		t.Errorf("a daemon with no event ring still emitted first_token: %s", body)
	}
}

func jsonHasKey(t *testing.T, body, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, ok := m[key]
	return ok
}
