package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// What this device's engine was doing when a request arrived
// (waired-agent#837). These are RECORDS OF TODAY'S BEHAVIOUR for the exact
// wording of the verdicts, and a product contract for one thing only: that
// "we did not look" is never rendered as "nothing was loaded", which is the
// distinction waired-agent#879 was filed to keep.

func TestResidencyVerdict(t *testing.T) {
	cases := []struct {
		name string
		res  runtime.ModelResidency
		want string
		out  string
	}{
		{"never observed", runtime.ModelResidency{}, "qwen3:8b", ""},
		{"observed, nothing loaded", runtime.ModelResidency{Observed: true}, "qwen3:8b", residencyAbsent},
		{"observed, this model", runtime.ModelResidency{Observed: true, Model: "qwen3:8b"}, "qwen3:8b", residencyResident},
		{"observed, another model", runtime.ModelResidency{Observed: true, Model: "qwen3:32b"}, "qwen3:8b", residencyOther},
		{"observed, tag spelled differently", runtime.ModelResidency{Observed: true, Model: "qwen3:8b-q4_K_M"}, "qwen3:8b", residencyOther},
		{"observed, but nothing was asked for", runtime.ModelResidency{Observed: true, Model: "qwen3:8b"}, "", residencyOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := residencyVerdict(tc.res, tc.want); got != tc.out {
				t.Errorf("residencyVerdict = %q, want %q", got, tc.out)
			}
		})
	}
}

// engineFactsGateway is newAdmissionGateway plus the two observation
// closures. Both are real closures over real state rather than constants, so
// the ordering the next test asserts is actually exercised.
func engineFactsGateway(t *testing.T, sel SelectorIface, engineURL string, res runtime.ModelResidency, inflight func() int, admit func(context.Context) func(), rec Recorder) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: engineURL})
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		Recorder:       rec,
		LocalResidency: func() runtime.ModelResidency { return res },
		LocalInflight:  inflight,
		LocalAdmission: admit,
		PeerAdapterFactory: func(string) (runtime.Adapter, error) {
			return fakeAdapter{baseURL: engineURL}, nil
		},
	})
}

func postAnthropic(t *testing.T, gw *Server) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"waired/default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

func nonStreamEngine(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
}

func TestAnthropicMessages_RecordsWhatTheEngineHeld(t *testing.T) {
	cases := []struct {
		name string
		res  runtime.ModelResidency
		sel  router.Selection
		want string
	}{
		{"held this model", runtime.ModelResidency{Observed: true, Model: "qwen3:8b-q4_K_M"},
			router.Selection{Runtime: "ollama", ModelID: "qwen3-8b-instruct", EngineModel: "qwen3:8b-q4_K_M"}, residencyResident},
		{"held nothing", runtime.ModelResidency{Observed: true},
			router.Selection{Runtime: "ollama", ModelID: "qwen3-8b-instruct", EngineModel: "qwen3:8b-q4_K_M"}, residencyAbsent},
		{"held something else", runtime.ModelResidency{Observed: true, Model: "qwen3:32b"},
			router.Selection{Runtime: "ollama", ModelID: "qwen3-8b-instruct", EngineModel: "qwen3:8b-q4_K_M"}, residencyOther},
		{"never observed", runtime.ModelResidency{},
			router.Selection{Runtime: "ollama", ModelID: "qwen3-8b-instruct", EngineModel: "qwen3:8b-q4_K_M"}, ""},
		{"a peer answered, so this device's engine says nothing",
			runtime.ModelResidency{Observed: true, Model: "qwen3:32b"},
			router.Selection{Runtime: remoteRuntimePrefix + "peerX", ModelID: "qwen3-8b-instruct", EngineModel: "qwen3:8b-q4_K_M"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := nonStreamEngine(t)
			defer engine.Close()
			rec := &captureRecorder{}
			gw := engineFactsGateway(t, &fakeSelector{sel: tc.sel}, engine.URL, tc.res, nil, nil, rec)

			if code := postAnthropic(t, gw).Code; code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			got := rec.requestsSnapshot()
			if len(got) != 1 {
				t.Fatalf("recorded %d request events, want 1", len(got))
			}
			if got[0].ModelResidency != tc.want {
				t.Errorf("ModelResidency = %q, want %q", got[0].ModelResidency, tc.want)
			}
		})
	}
}

// TestAnthropicMessages_EngineInflightExcludesThisRequest is the whole
// meaning of the field: it is what this request queued BEHIND. Reading it
// after the admission slot is taken would make every solo turn report 1 and
// the number would say nothing.
func TestAnthropicMessages_EngineInflightExcludesThisRequest(t *testing.T) {
	engine := nonStreamEngine(t)
	defer engine.Close()

	var live atomic.Int32
	admit := func(context.Context) func() {
		live.Add(1)
		return func() { live.Add(-1) }
	}
	inflight := func() int { return int(live.Load()) }

	sel := &fakeSelector{sel: router.Selection{Runtime: "ollama", ModelID: "qwen3-8b-instruct", EngineModel: "qwen3:8b-q4_K_M"}}

	t.Run("solo request", func(t *testing.T) {
		rec := &captureRecorder{}
		gw := engineFactsGateway(t, sel, engine.URL, runtime.ModelResidency{Observed: true}, inflight, admit, rec)
		if code := postAnthropic(t, gw).Code; code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		got := rec.requestsSnapshot()
		if len(got) != 1 {
			t.Fatalf("recorded %d events, want 1", len(got))
		}
		if got[0].EngineInflight != 0 {
			t.Errorf("EngineInflight = %d on an idle engine, want 0 — the count is being read "+
				"after this request took its own slot", got[0].EngineInflight)
		}
	})

	t.Run("one request already on the engine", func(t *testing.T) {
		rec := &captureRecorder{}
		gw := engineFactsGateway(t, sel, engine.URL, runtime.ModelResidency{Observed: true}, inflight, admit, rec)
		release := admit(context.Background()) // as if a session-title call were mid-flight (#856)
		defer release()

		if code := postAnthropic(t, gw).Code; code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		got := rec.requestsSnapshot()
		if len(got) != 1 {
			t.Fatalf("recorded %d events, want 1", len(got))
		}
		if got[0].EngineInflight != 1 {
			t.Errorf("EngineInflight = %d, want 1 — this turn queued behind one request", got[0].EngineInflight)
		}
	})
}

func TestLocalEngineFacts_PeerLegAsksNothingOfTheLocalEngine(t *testing.T) {
	asked := false
	h := NewHandlerSet(Deps{
		LocalResidency: func() runtime.ModelResidency {
			asked = true
			return runtime.ModelResidency{Observed: true, Model: "qwen3:8b"}
		},
		LocalInflight: func() int { asked = true; return 3 },
	})
	res, n := h.localEngineFacts(router.Selection{Runtime: remoteRuntimePrefix + "peerX"})
	if asked || res.Observed || n != 0 {
		t.Errorf("a peer leg consulted this device's engine (asked=%v res=%+v n=%d)", asked, res, n)
	}
}

func TestLocalEngineLogFields_SaysNothingWhenNothingWasObserved(t *testing.T) {
	h := NewHandlerSet(Deps{})
	if f := h.localEngineLogFields(router.Selection{Runtime: "ollama"}, nil); f != nil {
		t.Errorf("unwired surface produced log fields %v; an absent field is 'we did not look'", f)
	}

	h = NewHandlerSet(Deps{LocalResidency: func() runtime.ModelResidency {
		return runtime.ModelResidency{Observed: true, At: time.Now()}
	}})
	f := h.localEngineLogFields(router.Selection{Runtime: "ollama"}, nil)
	if len(f) < 2 || f[0] != "engine_holds" || f[1] != "none" {
		t.Errorf("observed-and-empty rendered as %v, want engine_holds=none", f)
	}
}
