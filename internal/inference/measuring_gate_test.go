package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMeasuringGateAdapter is the readiness gate of waired-agent#1127.
//
// Product contract — owner ruling, 2026-08-29: a node does not accept
// inference from other nodes until it knows what it costs to use.
func TestMeasuringGateAdapter(t *testing.T) {
	served := func(gate func(http.Handler) http.Handler) (int, string) {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		h := http.Handler(next)
		if gate != nil {
			h = gate(next)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		return rec.Code, rec.Body.String()
	}

	t.Run("measuring refuses with its own reason", func(t *testing.T) {
		code, body := served(measuringGateAdapter(func() bool { return true }))
		if code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", code)
		}
		var out struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		if out.Error.Type != "waired_inference_measuring" {
			// Its own code, not "overloaded": a host that is not offering
			// to serve yet has not been asked for too much.
			t.Errorf("type = %q, want waired_inference_measuring", out.Error.Type)
		}
	})

	t.Run("done lets the request through", func(t *testing.T) {
		if code, _ := served(measuringGateAdapter(func() bool { return false })); code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	})

	t.Run("nil closure disables the gate", func(t *testing.T) {
		// The pre-#1127 behaviour, and what an agent built without a
		// provider (--disable-inference, unenrolled) gets.
		if g := measuringGateAdapter(nil); g != nil {
			t.Error("a nil closure must produce no gate at all")
		}
	})
}

// TestHealthz_ReportsMeasuringAndTheSpeed: the gate is enforced on the
// serving path, but a probe must still be able to SEE why — the healthz
// surface deliberately bypasses every operator gate so the body can carry
// the state.
func TestHealthz_ReportsMeasuringAndTheSpeed(t *testing.T) {
	speed := &SpeedReading{
		VariantID: "q4-gguf", DepthTokens: 32768, PrefillTokps: 252.9, DecodeTokps: 15.8,
		TurnSeconds: 228.3, MeasuredAt: "2026-09-14T01:02:03.456Z",
	}
	s := &Server{
		isMeasuringFn: func() bool { return true },
		speedFn:       func() *SpeedReading { return speed },
		engineReadyFn: func() (bool, string) { return true, "qwen3:8b" },
	}
	rec := httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/waired/v1/inference/healthz", nil))

	var snap HealthSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !snap.Measuring {
		t.Error("measuring must reach the probe; a peer that cannot see it cannot route around it")
	}
	if snap.Speed == nil || *snap.Speed != *speed {
		t.Fatalf("Speed = %+v, want %+v", snap.Speed, speed)
	}
}

// TestHealthz_UnmeasuredHostOmitsTheSpeed keeps "nothing measured" off the
// wire entirely rather than as a zero, which would read as a host of no
// speed at all.
func TestHealthz_UnmeasuredHostOmitsTheSpeed(t *testing.T) {
	s := &Server{
		engineReadyFn: func() (bool, string) { return true, "qwen3:8b" },
		speedFn:       func() *SpeedReading { return nil },
	}
	rec := httptest.NewRecorder()
	s.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/waired/v1/inference/healthz", nil))
	if body := rec.Body.String(); jsonHasKey(t, body, "speed") {
		t.Errorf("body carries speed with nothing measured: %s", body)
	}
	if body := rec.Body.String(); jsonHasKey(t, body, "measuring") {
		t.Errorf("body carries measuring when it is false: %s", body)
	}
}

func jsonHasKey(t *testing.T, body, key string) bool {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	_, ok := raw[key]
	return ok
}
