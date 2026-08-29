package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

// observabilityMux is a test fixture that serves /state and /events
// based on per-test routes. The caller wires the responses via the
// returned setters; absent setters yield 404.
type observabilityMux struct {
	state  func(http.ResponseWriter, *http.Request)
	events func(http.ResponseWriter, *http.Request)
	// mesh and ping back the overlay measurement (waired#1137). Absent
	// setters 404, which is what a daemon without the mesh route does —
	// and what every test written before the measurement existed relies
	// on to keep the published counts standing on their own.
	mesh func(http.ResponseWriter, *http.Request)
	ping func(http.ResponseWriter, *http.Request)
}

func newObservabilityServer(t *testing.T, m *observabilityMux) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/observability/state":
			if m.state == nil {
				http.NotFound(w, r)
				return
			}
			m.state(w, r)
		case "/waired/v1/observability/events":
			if m.events == nil {
				http.NotFound(w, r)
				return
			}
			m.events(w, r)
		case "/waired/v1/inference/mesh":
			if m.mesh == nil {
				http.NotFound(w, r)
				return
			}
			m.mesh(w, r)
		case "/waired/v1/ping":
			if m.ping == nil {
				http.NotFound(w, r)
				return
			}
			m.ping(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeObservability_ReadyEngine_ReachableMesh_NoFallbacks(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{
					EngineReady:   true,
					ModelID:       "qwen3:8b",
					CapacityTotal: 10,
					CapacityUsed:  2,
				},
				Mesh: management.MeshState{
					PeersEnrolled: 3, PeersReachable: 3, PeersReady: 3,
				},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})

	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(got), got)
	}
	assertFindingStatus(t, got[0], "inference engine", integration.StatusOK, "qwen3:8b", "2 running, 10 conversations kept warm")
	assertFindingStatus(t, got[1], "mesh peers", integration.StatusOK, "3/3")
	assertFindingStatus(t, got[2], "recent fallbacks", integration.StatusOK, "none in last 10 min")
}

func TestProbeObservability_EngineNotReady(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{EngineReady: false},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(got), got)
	}
	assertFindingStatus(t, got[0], "inference engine", integration.StatusWarn, "not ready")
}

// TestProbeObservability_EngineNotReadySaysWhy pins the doctor half of
// waired-agent#1069. PRODUCT CONTRACT: on a host whose engine is down for
// a reason the daemon knows, `waired doctor` says the reason.
//
// It said only "not ready — local inference is offline" while
// runtimes[].last_error had carried the named cause the whole time,
// because management.AgentState had no field for it. That is the command
// every engine failure message points people at.
func TestProbeObservability_EngineNotReadySaysWhy(t *testing.T) {
	const why = "another program is already listening on 127.0.0.1:9479, the port the " +
		"inference engine was told to use — set inference.vllm_port in agent.json to a free port"
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{EngineReady: false, EngineFailureReason: why},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(got), got)
	}
	// Still "not ready", and now with the cause and the fallback note.
	assertFindingStatus(t, got[0], "inference engine", integration.StatusWarn,
		"not ready", why, "mesh peers and api.anthropic.com fallback will be used")
}

// TestProbeObservability_ReadyEngineNamesTheServingEngine pins
// waired-agent#1076: the engine on this line is the one that is serving.
// It was the literal "ollama" over fields the daemon only ever filled
// from the ollama adapter, so a vLLM host was told about an engine that
// was not answering its requests.
func TestProbeObservability_ReadyEngineNamesTheServingEngine(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{
					EngineReady: true, ModelID: "gpt-oss-20b",
					EngineName: "vllm", EngineVersion: "0.11.0",
				},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(got), got)
	}
	assertFindingStatus(t, got[0], "inference engine", integration.StatusOK,
		"gpt-oss-20b", "engine=vllm 0.11.0")
	if strings.Contains(got[0].Detail, "ollama") {
		t.Errorf("detail names ollama on a vLLM host: %q", got[0].Detail)
	}
}

// TestProbeObservability_OlderDaemonNamesNoEngine: an agent predating
// EngineName sends none, and there is nothing trustworthy to print — so
// the suffix is omitted rather than guessed at.
func TestProbeObservability_OlderDaemonNamesNoEngine(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{
					EngineReady: true, ModelID: "qwen3:8b",
					EngineMode: "spawned", EngineVersion: "0.32.15",
				},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	assertFindingStatus(t, got[0], "inference engine", integration.StatusOK, "qwen3:8b")
	if strings.Contains(got[0].Detail, "engine=") {
		t.Errorf("named an engine the daemon did not report: %q", got[0].Detail)
	}
}

// TestProbeObservability_LocalInferenceOffIsNotAFault: a computer told
// not to run AI models itself reports EngineReady=false, exactly like
// one whose engine crashed. Doctor used to warn identically for both and
// name no way forward — on the very hosts whose failure copy points at
// `waired doctor` (#465, waired-ai/waired#1056).
func TestProbeObservability_LocalInferenceOffIsNotAFault(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{EngineReady: false, LocalInferenceState: "disabled"},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(got), got)
	}
	assertFindingStatus(t, got[0], "inference engine", integration.StatusOK, "off on this computer")
	if !strings.Contains(got[0].Detail, "waired inference on") {
		t.Errorf("detail %q does not name the way to turn it on", got[0].Detail)
	}
}

// An older daemon sends no local_inference_state, and must keep the
// pre-#465 reading: EngineReady=false is the only signal available, so
// it stays a warning rather than being reinterpreted as a setting.
func TestProbeObservability_OlderDaemonKeepsTheEngineWarning(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{EngineReady: false},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	assertFindingStatus(t, got[0], "inference engine", integration.StatusWarn, "not ready")
}

func TestProbeObservability_PausedAgentReportsPaused(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{Paused: true, EngineReady: true},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3", len(got))
	}
	assertFindingStatus(t, got[0], "inference engine", integration.StatusWarn, "paused")
}

func TestProbeObservability_MeshDegraded(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{EngineReady: true},
				Mesh:  management.MeshState{PeersEnrolled: 4, PeersReachable: 4, PeersReady: 1},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		},
	})
	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3", len(got))
	}
	assertFindingStatus(t, got[1], "mesh peers", integration.StatusWarn, "only 1 ready")
}

func TestProbeObservability_RecentFallbackBuckets(t *testing.T) {
	cases := []struct {
		name      string
		n         int
		wantState integration.Status
	}{
		{"small bucket warns", 1, integration.StatusWarn},
		{"large bucket warns harder", 8, integration.StatusWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			events := make([]observability.Event, tc.n)
			for i := range events {
				events[i] = observability.Event{
					Seq:      uint64(i + 1),
					TS:       now.Add(-time.Duration(i+1) * time.Minute),
					Kind:     observability.KindFallback,
					Fallback: &observability.FallbackEvent{From: "a", To: "b", Reason: "r"},
				}
			}
			srv := newObservabilityServer(t, &observabilityMux{
				state: func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(management.ObservabilityState{
						Agent: management.AgentState{EngineReady: true},
					})
				},
				events: func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{Events: events})
				},
			})
			got := probeObservability(context.Background(), srv.URL)
			if len(got) != 3 {
				t.Fatalf("got %d findings, want 3", len(got))
			}
			if got[2].Status != tc.wantState {
				t.Errorf("recent fallbacks status = %s, want %s (n=%d, detail=%q)",
					got[2].Status, tc.wantState, tc.n, got[2].Detail)
			}
		})
	}
}

func TestProbeObservability_404OnState_EmitsSingleSkip(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{}) // both routes 404

	got := probeObservability(context.Background(), srv.URL)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1 (skip): %+v", len(got), got)
	}
	if got[0].Status != integration.StatusSkip {
		t.Errorf("status = %s, want StatusSkip", got[0].Status)
	}
	if !strings.Contains(got[0].Detail, "Phase 9") {
		t.Errorf("detail should explain upgrade path, got %q", got[0].Detail)
	}
}

func TestProbeObservability_NoFailNeverEmitsStatusFail(t *testing.T) {
	// Even when every endpoint behaves badly, doctor must not emit
	// StatusFail for observability findings — they are operational
	// signal, not config breakage.
	//
	// The bad-state case alone used to be the whole test, and it never
	// reached the mesh route. It is now a table, so the overlay
	// measurement (waired#1137) is covered by this pin instead of
	// passing it vacuously: a mesh that reports peers and answers no
	// ping is the worst thing this probe can find, and it is still a
	// warning.
	cases := map[string]*observabilityMux{
		"every endpoint behaves badly": {
			state: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
		},
		"the whole mesh fails its overlay ping": {
			state: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(management.ObservabilityState{
					Agent: management.AgentState{EngineReady: true, ModelID: "qwen3:8b"},
					Mesh:  management.MeshState{PeersEnrolled: 3, PeersReachable: 3, PeersReady: 3},
				})
			},
			events: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
			},
			mesh: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
					{DeviceName: "a"}, {DeviceName: "b"}, {DeviceName: "c"},
				}})
			},
			ping: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			},
		},
	}
	for name, mux := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newObservabilityServer(t, mux)
			got := probeObservability(context.Background(), srv.URL)
			for _, f := range got {
				if f.Status == integration.StatusFail {
					t.Errorf("observability emitted StatusFail (forbidden): %+v", f)
				}
			}
		})
	}
}

func TestProbeObservability_UnreachableMgmt_NoFindings(t *testing.T) {
	// Closed server → connect refused. Doctor must stay silent so the
	// existing daemon-unreachable probe carries the message alone.
	srv := newObservabilityServer(t, &observabilityMux{})
	deadURL := srv.URL
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := probeObservability(ctx, deadURL)
	// Closed server returns 404 from the closed test handler... actually
	// no, an httptest.Server post-Close yields a dial-refused, mapped to
	// "transport error" → 0 findings. (If we got the connection-refused
	// before the call ran, observabilityclient.GetState returns a plain
	// non-Unsupported error and probeObservability returns nil.)
	if len(got) != 0 {
		t.Errorf("unreachable mgmt should yield 0 findings, got %d: %+v", len(got), got)
	}
}

func assertFindingStatus(t *testing.T, f integration.AuditFinding, wantSubject string, wantStatus integration.Status, wantInDetail ...string) {
	t.Helper()
	if f.Subject != wantSubject {
		t.Errorf("subject=%q, want %q", f.Subject, wantSubject)
	}
	if f.Status != wantStatus {
		t.Errorf("status=%s, want %s (detail=%q)", f.Status, wantStatus, f.Detail)
	}
	for _, s := range wantInDetail {
		if !strings.Contains(f.Detail, s) {
			t.Errorf("detail %q missing %q", f.Detail, s)
		}
	}
}
